package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
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
	headerConversation                  = "X-Conversation-Id"
	headerHermes                        = "X-Hermes-Session-Id"
	headerKilo                          = "X-KiloCode-TaskId"
    headerKiloSession                   = "X-Session-Id"
    headerKiloAffinity                  = "X-Session-Affinity"
	defaultReadHeaderTimeout            = 10 * time.Second
	defaultBackendResponseHeaderTimeout = 5 * time.Minute
)

var supportedNamespaces = []string{"owui:", "hermes:", "kilo:"}

var generationPaths = map[string]struct{}{
	"/v1/chat/completions": {},
	"/v1/completions":      {},
	"/v1/responses":        {},
}

var backendTransportBaseline = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          100,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}

var (
	errConflictingExplicitSlot       = errors.New("conflicting explicit id_slot in request body")
	errInvalidExplicitSlot           = errors.New("invalid explicit id_slot in request body")
	errRewriteBodyTooLarge           = errors.New("request body too large for id_slot injection")
	maxRewriteBodyBytes        int64 = 32 << 20
)

type config struct {
	listenAddr                   string
	backendURL                   *url.URL
	slotCount                    int
	backendResponseHeaderTimeout time.Duration
	transport                    http.RoundTripper
}

type affinityEntry struct {
	conversationID string
	lastUsed       time.Time
	active         int
}

type affinityManager struct {
	mu         sync.Mutex
	slots      []affinityEntry
	convToSlot map[string]int
	now        func() time.Time
	waitCh     chan struct{}
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

type slotLease struct {
	manager *affinityManager
	slot    int
	once    sync.Once
}

type affinityReservation struct {
	lease  *slotLease
	result affinityResult
}

type reservationContextKey struct{}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	handler := newProxyServer(cfg)
	log.Printf("starting proxy listen_addr=%s backend_url=%s slot_count=%d", cfg.listenAddr, cfg.backendURL.Redacted(), cfg.slotCount)
	server := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
	}
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func loadConfig() (config, error) {
	listenAddr := getenvDefault("LISTEN_ADDR", ":8001")
	backendRaw := getenvDefault("BACKEND_URL", "http://llcpp-backend:8801")
	slotRaw := getenvDefault("SLOT_COUNT", "4")
	backendResponseHeaderTimeoutRaw := getenvDefault("BACKEND_RESPONSE_HEADER_TIMEOUT", defaultBackendResponseHeaderTimeout.String())

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

	backendResponseHeaderTimeout, err := time.ParseDuration(backendResponseHeaderTimeoutRaw)
	if err != nil || backendResponseHeaderTimeout <= 0 {
		return config{}, errors.New("BACKEND_RESPONSE_HEADER_TIMEOUT must be a positive duration")
	}

	return config{
		listenAddr:                   listenAddr,
		backendURL:                   backendURL,
		slotCount:                    slotCount,
		backendResponseHeaderTimeout: backendResponseHeaderTimeout,
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
	transport := cfg.transport
	if transport == nil {
		transport = newBackendTransport(cfg.backendResponseHeaderTimeout)
	}
	proxy.Transport = &leasingTransport{base: transport}
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

func newBackendTransport(timeout time.Duration) http.RoundTripper {
	if timeout <= 0 {
		timeout = defaultBackendResponseHeaderTimeout
	}
	transport := backendTransportBaseline.Clone()
	transport.ResponseHeaderTimeout = timeout
	return transport
}

func newAffinityManager(slotCount int) *affinityManager {
	return &affinityManager{
		slots:      make([]affinityEntry, slotCount),
		convToSlot: make(map[string]int, slotCount),
		now: func() time.Time {
			return time.Now().UTC()
		},
		waitCh: make(chan struct{}),
	}
}

func (m *affinityManager) Acquire(conversationID string) affinityResult {
	reservation, err := m.ReserveChecked(context.Background(), conversationID, nil)
	if err != nil {
		return affinityResult{}
	}
	reservation.lease.Release()
	return reservation.result
}

func (m *affinityManager) ReserveChecked(ctx context.Context, conversationID string, explicitSlot *int) (affinityReservation, error) {
	for {
		m.mu.Lock()
		reservation, waitCh, err := m.tryReserveLocked(conversationID, explicitSlot)
		m.mu.Unlock()
		if err != nil {
			return affinityReservation{}, err
		}
		if waitCh == nil {
			return reservation, nil
		}

		select {
		case <-ctx.Done():
			return affinityReservation{}, ctx.Err()
		case <-waitCh:
		}
	}
}

func (m *affinityManager) tryReserveLocked(conversationID string, explicitSlot *int) (affinityReservation, chan struct{}, error) {
	now := m.now()
	if explicitSlot != nil && (*explicitSlot < 0 || *explicitSlot >= len(m.slots)) {
		return affinityReservation{}, nil, errInvalidExplicitSlot
	}
	if slot, ok := m.convToSlot[conversationID]; ok {
		if explicitSlot != nil && *explicitSlot != slot {
			return affinityReservation{}, nil, errConflictingExplicitSlot
		}
		m.slots[slot].lastUsed = now
		m.slots[slot].active++
		return affinityReservation{
			lease:  &slotLease{manager: m, slot: slot},
			result: affinityResult{slot: slot, action: "reuse", lastUsed: now},
		}, nil, nil
	}

	if explicitSlot != nil {
		slot := *explicitSlot
		entry := m.slots[slot]
		if entry.conversationID == "" {
			m.slots[slot] = affinityEntry{conversationID: conversationID, lastUsed: now, active: 1}
			m.convToSlot[conversationID] = slot
			return affinityReservation{
				lease:  &slotLease{manager: m, slot: slot},
				result: affinityResult{slot: slot, action: "assign", lastUsed: now},
			}, nil, nil
		}
		return affinityReservation{}, nil, errConflictingExplicitSlot
	}

	for slot, entry := range m.slots {
		if entry.conversationID == "" {
			m.slots[slot] = affinityEntry{conversationID: conversationID, lastUsed: now, active: 1}
			m.convToSlot[conversationID] = slot
			return affinityReservation{
				lease:  &slotLease{manager: m, slot: slot},
				result: affinityResult{slot: slot, action: "assign", lastUsed: now},
			}, nil, nil
		}
	}

	lruSlot, ok := m.findEvictableLRUSlotLocked()
	if !ok {
		return affinityReservation{}, m.waitCh, nil
	}
	evicted := m.slots[lruSlot].conversationID
	delete(m.convToSlot, evicted)
	m.slots[lruSlot] = affinityEntry{conversationID: conversationID, lastUsed: now, active: 1}
	m.convToSlot[conversationID] = lruSlot
	return affinityReservation{
		lease:  &slotLease{manager: m, slot: lruSlot},
		result: affinityResult{slot: lruSlot, action: "assign", evicted: evicted, lastUsed: now},
	}, nil, nil
}

func (m *affinityManager) findEvictableLRUSlotLocked() (int, bool) {
	lruSlot := -1
	for slot, entry := range m.slots {
		if entry.conversationID == "" || entry.active > 0 {
			continue
		}
		if lruSlot == -1 || entry.lastUsed.Before(m.slots[lruSlot].lastUsed) {
			lruSlot = slot
		}
	}
	return lruSlot, lruSlot >= 0
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
		
		req.Header.Del(headerHermes)
		req.Header.Del(headerKilo)
        req.Header.Del(headerKiloSession)
        req.Header.Del(headerKiloAffinity)
	}

	if hasConversation {
		needsInjection := shouldInjectSlot(req)
		var (
			payload      map[string]json.RawMessage
			explicitSlot *int
			err          error
		)
		if needsInjection {
			payload, explicitSlot, err = decodeRequestBody(req)
			if err != nil {
				http.Error(rw, err.Error(), http.StatusBadRequest)
				return
			}
			reservation, err := s.affinity.ReserveChecked(req.Context(), normalizedID, explicitSlot)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				http.Error(rw, err.Error(), http.StatusBadRequest)
				return
			}
			defer reservation.lease.Release()
			affinity := reservation.result
			if affinity.evicted == "" {
				log.Printf("affinity conversation=%s slot=%d action=%s", normalizedID, affinity.slot, affinity.action)
			} else {
				log.Printf("affinity conversation=%s slot=%d action=%s evicted=%s", normalizedID, affinity.slot, affinity.action, affinity.evicted)
			}
			if explicitSlot == nil {
				payload["id_slot"] = json.RawMessage(strconv.Itoa(affinity.slot))
			}
			if err := replaceRequestBody(req, payload); err != nil {
				reservation.lease.Release()
				http.Error(rw, err.Error(), http.StatusBadRequest)
				return
			}
			req = req.WithContext(context.WithValue(req.Context(), reservationContextKey{}, reservation.lease))
		}
	}

	s.proxy.ServeHTTP(rw, req)
}

func (s *proxyServer) handleHealth(rw http.ResponseWriter, _ *http.Request) {
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write([]byte("ok\n"))
}

func (s *proxyServer) handleAffinity(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isLoopbackDiagnosticsCaller(req.RemoteAddr) {
		http.Error(rw, "forbidden", http.StatusForbidden)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(rw).Encode(s.affinity.Snapshot()); err != nil {
		log.Printf("diagnostics error method=GET path=/_affinity error=%v", err)
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
        {header: headerKiloSession, prefix: "kilo:"},
        {header: headerKiloAffinity, prefix: "kilo:"},
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

func isLoopbackDiagnosticsCaller(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if zoneIndex := strings.Index(host, "%"); zoneIndex >= 0 {
		host = host[:zoneIndex]
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
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
	body, err := io.ReadAll(io.LimitReader(req.Body, maxRewriteBodyBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read request body: %w", err)
	}
	_ = req.Body.Close()
	if int64(len(body)) > maxRewriteBodyBytes {
		return nil, nil, errRewriteBodyTooLarge
	}

	payload := make(map[string]json.RawMessage)
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, nil, fmt.Errorf("invalid JSON request body: %w", err)
	}
	if payload == nil {
		return nil, nil, errors.New("invalid JSON request body: top-level JSON value must be an object")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, nil, fmt.Errorf("invalid JSON request body: multiple JSON values are not supported")
	}
	if existing, ok := payload["id_slot"]; ok {
		if bytes.Equal(bytes.TrimSpace(existing), []byte("null")) {
			delete(payload, "id_slot")
			return payload, nil, nil
		}
		existingSlot, err := decodeExplicitSlot(existing)
		if err != nil {
			return nil, nil, errInvalidExplicitSlot
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
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return 0, errInvalidExplicitSlot
	}
	for index, r := range value {
		if r == '-' && index == 0 {
			continue
		}
		if r < '0' || r > '9' {
			return 0, errInvalidExplicitSlot
		}
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, err
	}
	return parsed, nil
}

func (l *slotLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.manager.mu.Lock()
		defer l.manager.mu.Unlock()
		if l.slot < 0 || l.slot >= len(l.manager.slots) {
			return
		}
		if l.manager.slots[l.slot].active > 0 {
			l.manager.slots[l.slot].active--
			l.manager.notifyWaitersLocked()
		}
	})
}

func (m *affinityManager) notifyWaitersLocked() {
	close(m.waitCh)
	m.waitCh = make(chan struct{})
}

type leasingTransport struct {
	base http.RoundTripper
}

func (t *leasingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	transport := t.base
	if transport == nil {
		transport = http.DefaultTransport
	}

	lease, _ := req.Context().Value(reservationContextKey{}).(*slotLease)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		lease.Release()
		return nil, err
	}
	if lease != nil && resp.Body != nil {
		resp.Body = &releasingReadCloser{ReadCloser: resp.Body, release: lease.Release}
	}
	return resp, nil
}

type releasingReadCloser struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (r *releasingReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		r.releaseOnce()
	}
	return n, err
}

func (r *releasingReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.releaseOnce()
	return err
}

func (r *releasingReadCloser) releaseOnce() {
	r.once.Do(func() {
		if r.release != nil {
			r.release()
		}
	})
}
