package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	headerConversation = "X-Conversation-Id"
	headerHermes       = "X-Hermes-Session-Id"
	headerKilo         = "X-KiloCode-TaskId"
)

var supportedNamespaces = []string{"owui:", "hermes:", "kilo:"}

var generationPaths = map[string]struct{}{
	"/v1/chat/completions": {},
	"/v1/completions":      {},
	"/v1/responses":        {},
}

type config struct {
	listenAddr string
	backendURL *url.URL
	slotCount  int
}

type affinityEntry struct {
	conversationID string
	lastUsed       time.Time
}

type affinityManager struct {
	mu         sync.Mutex
	slots      []affinityEntry
	convToSlot map[string]int
	now        func() time.Time
}

type affinityResult struct {
	slot     int
	action   string
	evicted  string
	lastUsed time.Time
}

type affinitySlotView struct {
	Slot           int        `json:"slot"`
	ConversationID *string    `json:"conversation_id"`
	LastUsed       *time.Time `json:"last_used,omitempty"`
}

type affinityView struct {
	SlotCount int                `json:"slot_count"`
	Slots     []affinitySlotView `json:"slots"`
}

type proxyServer struct {
	affinity *affinityManager
	proxy    *httputil.ReverseProxy
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	handler := newProxyServer(cfg)
	log.Printf("starting proxy listen_addr=%s backend_url=%s slot_count=%d", cfg.listenAddr, cfg.backendURL.String(), cfg.slotCount)
	if err := http.ListenAndServe(cfg.listenAddr, handler); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func loadConfig() (config, error) {
	listenAddr := getenvDefault("LISTEN_ADDR", ":8001")
	backendRaw := getenvDefault("BACKEND_URL", "http://llcpp-backend:8801")
	slotRaw := getenvDefault("SLOT_COUNT", "4")

	backendURL, err := url.Parse(backendRaw)
	if err != nil {
		return config{}, fmt.Errorf("parse BACKEND_URL: %w", err)
	}
	if backendURL.Scheme == "" || backendURL.Host == "" {
		return config{}, errors.New("BACKEND_URL must include scheme and host")
	}
	if backendURL.Scheme != "http" && backendURL.Scheme != "https" {
		return config{}, errors.New("BACKEND_URL must use http or https")
	}

	slotCount, err := strconv.Atoi(slotRaw)
	if err != nil || slotCount <= 0 {
		return config{}, errors.New("SLOT_COUNT must be a positive integer")
	}

	return config{
		listenAddr: listenAddr,
		backendURL: backendURL,
		slotCount:  slotCount,
	}, nil
}

func getenvDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func newProxyServer(cfg config) http.Handler {
	affinity := newAffinityManager(cfg.slotCount)
	proxy := httputil.NewSingleHostReverseProxy(cfg.backendURL)
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
		log.Printf("backend error method=%s path=%s error=%v", req.Method, req.URL.Path, err)
		http.Error(rw, "bad gateway", http.StatusBadGateway)
	}

	server := &proxyServer{
		affinity: affinity,
		proxy:    proxy,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", server.handleHealth)
	mux.HandleFunc("/_affinity", server.handleAffinity)
	mux.Handle("/", server)
	return mux
}

func newAffinityManager(slotCount int) *affinityManager {
	return &affinityManager{
		slots:      make([]affinityEntry, slotCount),
		convToSlot: make(map[string]int, slotCount),
		now:        time.Now().UTC,
	}
}

func (m *affinityManager) Acquire(conversationID string) affinityResult {
	result, _ := m.acquire(conversationID, nil)
	return result
}

func (m *affinityManager) AcquireChecked(conversationID string, explicitSlot *int) (affinityResult, error) {
	return m.acquire(conversationID, explicitSlot)
}

func (m *affinityManager) acquire(conversationID string, explicitSlot *int) (affinityResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	if slot, ok := m.convToSlot[conversationID]; ok {
		if explicitSlot != nil && *explicitSlot != slot {
			return affinityResult{}, errors.New("conflicting explicit id_slot in request body")
		}
		m.slots[slot].lastUsed = now
		return affinityResult{slot: slot, action: "reuse", lastUsed: now}, nil
	}

	for slot, entry := range m.slots {
		if entry.conversationID == "" {
			if explicitSlot != nil && *explicitSlot != slot {
				return affinityResult{}, errors.New("conflicting explicit id_slot in request body")
			}
			m.slots[slot] = affinityEntry{conversationID: conversationID, lastUsed: now}
			m.convToSlot[conversationID] = slot
			return affinityResult{slot: slot, action: "assign", lastUsed: now}, nil
		}
	}

	lruSlot := 0
	for slot := 1; slot < len(m.slots); slot++ {
		if m.slots[slot].lastUsed.Before(m.slots[lruSlot].lastUsed) {
			lruSlot = slot
		}
	}
	if explicitSlot != nil && *explicitSlot != lruSlot {
		return affinityResult{}, errors.New("conflicting explicit id_slot in request body")
	}

	evicted := m.slots[lruSlot].conversationID
	delete(m.convToSlot, evicted)
	m.slots[lruSlot] = affinityEntry{conversationID: conversationID, lastUsed: now}
	m.convToSlot[conversationID] = lruSlot
	return affinityResult{slot: lruSlot, action: "assign", evicted: evicted, lastUsed: now}, nil
}

func (m *affinityManager) Snapshot() affinityView {
	m.mu.Lock()
	defer m.mu.Unlock()

	view := affinityView{
		SlotCount: len(m.slots),
		Slots:     make([]affinitySlotView, len(m.slots)),
	}

	for slot, entry := range m.slots {
		slotView := affinitySlotView{Slot: slot}
		if entry.conversationID != "" {
			conversationID := entry.conversationID
			lastUsed := entry.lastUsed
			slotView.ConversationID = &conversationID
			slotView.LastUsed = &lastUsed
		}
		view.Slots[slot] = slotView
	}

	return view
}

func (s *proxyServer) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	normalizedID, hasConversation := normalizeConversationID(req.Header)
	if hasConversation {
		req.Header.Set(headerConversation, normalizedID)
	}

	if hasConversation && shouldInjectSlot(req) {
		payload, explicitSlot, err := decodeRequestBody(req)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}

		affinity, err := s.affinity.AcquireChecked(normalizedID, explicitSlot)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		if affinity.evicted == "" {
			log.Printf("affinity conversation=%s slot=%d action=%s", normalizedID, affinity.slot, affinity.action)
		} else {
			log.Printf("affinity conversation=%s slot=%d action=%s evicted=%s", normalizedID, affinity.slot, affinity.action, affinity.evicted)
		}

		if explicitSlot == nil {
			payload["id_slot"] = json.RawMessage(strconv.Itoa(affinity.slot))
		}
		if err := replaceRequestBody(req, payload); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
	}

	s.proxy.ServeHTTP(rw, req)
}

func (s *proxyServer) handleHealth(rw http.ResponseWriter, _ *http.Request) {
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write([]byte("ok\n"))
}

func (s *proxyServer) handleAffinity(rw http.ResponseWriter, _ *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(rw).Encode(s.affinity.Snapshot()); err != nil {
		log.Printf("backend error method=GET path=/_affinity error=%v", err)
	}
}

func normalizeConversationID(headers http.Header) (string, bool) {
	for _, candidate := range []struct {
		header string
		prefix string
	}{
		{header: headerConversation, prefix: "owui:"},
		{header: headerHermes, prefix: "hermes:"},
		{header: headerKilo, prefix: "kilo:"},
	} {
		value := strings.TrimSpace(headers.Get(candidate.header))
		if value == "" {
			continue
		}
		if hasNamespace(value) {
			return value, true
		}
		return candidate.prefix + value, true
	}
	return "", false
}

func hasNamespace(value string) bool {
	lower := strings.ToLower(value)
	for _, namespace := range supportedNamespaces {
		if strings.HasPrefix(lower, namespace) {
			return true
		}
	}
	return false
}

func shouldInjectSlot(req *http.Request) bool {
	if req.Method != http.MethodPost {
		return false
	}
	if _, ok := generationPaths[req.URL.Path]; !ok {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	return mediaType == "application/json"
}

func decodeRequestBody(req *http.Request) (map[string]json.RawMessage, *int, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read request body: %w", err)
	}
	_ = req.Body.Close()

	payload := make(map[string]json.RawMessage)
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, nil, fmt.Errorf("invalid JSON request body: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, nil, fmt.Errorf("invalid JSON request body: multiple JSON values are not supported")
	}
	if existing, ok := payload["id_slot"]; ok {
		existingSlot, err := decodeExplicitSlot(existing)
		if err != nil {
			return nil, nil, fmt.Errorf("conflicting explicit id_slot in request body")
		}
		return payload, &existingSlot, nil
	}
	return payload, nil, nil
}

func replaceRequestBody(req *http.Request, payload map[string]json.RawMessage) error {
	modifiedBody, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode JSON request body: %w", err)
	}

	req.Body = io.NopCloser(bytes.NewReader(modifiedBody))
	req.ContentLength = int64(len(modifiedBody))
	req.Header.Set("Content-Length", strconv.Itoa(len(modifiedBody)))
	return nil
}

func decodeExplicitSlot(raw json.RawMessage) (int, error) {
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(number.String())
	if err != nil {
		return 0, err
	}
	return value, nil
}
