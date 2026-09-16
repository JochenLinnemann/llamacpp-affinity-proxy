package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNormalizeConversationIDOpenWebUI(t *testing.T) {
	headers := http.Header{}
	headers.Set(headerConversation, "chat-123")

	got, ok := normalizeConversationID(headers)
	if !ok || got != "owui:chat-123" {
		t.Fatalf("normalizeConversationID() = %q, %t", got, ok)
	}
}

func TestNormalizeConversationIDHermes(t *testing.T) {
	headers := http.Header{}
	headers.Set(headerHermes, "session-123")

	got, ok := normalizeConversationID(headers)
	if !ok || got != "hermes:session-123" {
		t.Fatalf("normalizeConversationID() = %q, %t", got, ok)
	}
}

func TestNormalizeConversationIDKilo(t *testing.T) {
	headers := http.Header{}
	headers.Set(headerKilo, "task-123")

	got, ok := normalizeConversationID(headers)
	if !ok || got != "kilo:task-123" {
		t.Fatalf("normalizeConversationID() = %q, %t", got, ok)
	}
}

func TestNormalizeConversationIDDoesNotDoublePrefix(t *testing.T) {
	headers := http.Header{}
	headers.Set(headerConversation, "hermes:session-123")

	got, ok := normalizeConversationID(headers)
	if !ok || got != "hermes:session-123" {
		t.Fatalf("normalizeConversationID() = %q, %t", got, ok)
	}
}

func TestAffinityExistingConversationGetsSameSlot(t *testing.T) {
	manager := newAffinityManager(4)
	first := manager.Acquire("hermes:abc")
	second := manager.Acquire("hermes:abc")

	if first.slot != second.slot {
		t.Fatalf("expected same slot, got %d and %d", first.slot, second.slot)
	}
	if second.action != "reuse" {
		t.Fatalf("expected reuse action, got %q", second.action)
	}
}

func TestAffinityNewConversationsOccupyFreeSlots(t *testing.T) {
	manager := newAffinityManager(4)
	slots := []int{
		manager.Acquire("a").slot,
		manager.Acquire("b").slot,
		manager.Acquire("c").slot,
		manager.Acquire("d").slot,
	}
	sort.Ints(slots)

	for i, slot := range slots {
		if slot != i {
			t.Fatalf("expected free slot %d, got %d", i, slot)
		}
	}
}

func TestAffinityEvictsLeastRecentlyUsedConversation(t *testing.T) {
	manager := newAffinityManager(4)
	base := time.Date(2026, 9, 16, 14, 0, 0, 0, time.UTC)
	var ticks int
	manager.now = func() time.Time {
		ticks++
		return base.Add(time.Duration(ticks) * time.Second)
	}

	manager.Acquire("a")
	manager.Acquire("b")
	manager.Acquire("c")
	manager.Acquire("d")
	result := manager.Acquire("e")

	if result.evicted != "a" {
		t.Fatalf("expected a to be evicted, got %q", result.evicted)
	}
	if result.slot != 0 {
		t.Fatalf("expected slot 0 to be reused, got %d", result.slot)
	}
}

func TestAffinityReuseUpdatesLRUOrder(t *testing.T) {
	manager := newAffinityManager(2)
	base := time.Date(2026, 9, 16, 14, 0, 0, 0, time.UTC)
	var ticks int
	manager.now = func() time.Time {
		ticks++
		return base.Add(time.Duration(ticks) * time.Second)
	}

	manager.Acquire("a")
	manager.Acquire("b")
	manager.Acquire("a")
	result := manager.Acquire("c")

	if result.evicted != "b" {
		t.Fatalf("expected b to be evicted after a reuse, got %q", result.evicted)
	}
}

func TestAffinityConcurrentAssignmentKeepsUniqueSlots(t *testing.T) {
	const slotCount = 32
	manager := newAffinityManager(slotCount)

	assigned := make(chan int, slotCount)
	var wg sync.WaitGroup
	for i := 0; i < slotCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			assigned <- manager.Acquire("conversation-" + strconv.Itoa(i)).slot
		}(i)
	}
	wg.Wait()
	close(assigned)

	seen := map[int]bool{}
	for slot := range assigned {
		if seen[slot] {
			t.Fatalf("slot %d assigned more than once", slot)
		}
		seen[slot] = true
	}
	if len(seen) != slotCount {
		t.Fatalf("expected %d unique slots, got %d", slotCount, len(seen))
	}
}

func TestInjectsIDSlotIntoJSONRequest(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		_ = req.Body.Close()
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write(body)
	}))
	defer backend.Close()

	server := newProxyTestServer(t, backend.URL, 4)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test","messages":[],"stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerConversation, "chat-123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.StatusCode; got != http.StatusOK {
		t.Fatalf("status = %d", got)
	}

	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["id_slot"] != float64(0) {
		t.Fatalf("expected id_slot 0, got %#v", payload["id_slot"])
	}
}

func TestRequestsWithoutConversationHeadersPassThroughWithoutIDSlot(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		_ = req.Body.Close()
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write(body)
	}))
	defer backend.Close()

	server := newProxyTestServer(t, backend.URL, 4)
	defer server.Close()

	resp, err := http.DefaultClient.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"test","messages":[]}`))
	if err != nil {
		t.Fatalf("post request: %v", err)
	}
	defer resp.Body.Close()

	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := payload["id_slot"]; ok {
		t.Fatalf("expected no id_slot, got %#v", payload["id_slot"])
	}
}

func TestConflictingExplicitIDSlotReturnsBadRequestWithoutChangingAffinity(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		t.Fatal("backend should not receive conflicting request")
	}))
	defer backend.Close()

	server := newProxyTestServer(t, backend.URL, 4)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test","id_slot":3}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerConversation, "chat-1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	affinityResp, err := http.Get(server.URL + "/_affinity")
	if err != nil {
		t.Fatalf("get affinity: %v", err)
	}
	defer affinityResp.Body.Close()

	var view affinityView
	if err := json.NewDecoder(affinityResp.Body).Decode(&view); err != nil {
		t.Fatalf("decode affinity: %v", err)
	}
	if got := sortedConversations(view); len(got) != 0 {
		t.Fatalf("expected no affinity assignment, got %#v", got)
	}
}

func TestStreamingResponsesForwardIncrementally(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := rw.(http.Flusher)
		if !ok {
			t.Fatal("response writer does not support flushing")
		}
		_, _ = rw.Write([]byte("data: first\n\n"))
		flusher.Flush()
		time.Sleep(200 * time.Millisecond)
		_, _ = rw.Write([]byte("data: second\n\n"))
		flusher.Flush()
	}))
	defer backend.Close()

	server := newProxyTestServer(t, backend.URL, 4)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test","messages":[],"stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerConversation, "chat-1")

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	firstChunk, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("first chunk arrived too late: %v", elapsed)
	}
	if firstChunk != "data: first\n" {
		t.Fatalf("unexpected first chunk %q", firstChunk)
	}

	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read rest: %v", err)
	}
	if !strings.Contains(string(rest), "data: second") {
		t.Fatalf("missing second chunk in %q", string(rest))
	}
}

func TestAffinityEndpoint(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	server := newProxyTestServer(t, backend.URL, 2)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerHermes, "abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	resp.Body.Close()

	affinityResp, err := http.Get(server.URL + "/_affinity")
	if err != nil {
		t.Fatalf("get affinity: %v", err)
	}
	defer affinityResp.Body.Close()

	var view affinityView
	if err := json.NewDecoder(affinityResp.Body).Decode(&view); err != nil {
		t.Fatalf("decode affinity: %v", err)
	}
	if got := sortedConversations(view); len(got) != 1 || got[0] != "hermes:abc" {
		t.Fatalf("unexpected affinity view: %#v", got)
	}
}

func TestNonGenerationRequestPassesThroughWithoutUpdatingAffinity(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	server := newProxyTestServer(t, backend.URL, 1)
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(headerHermes, "abc")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	resp.Body.Close()

	affinityResp, err := http.Get(server.URL + "/_affinity")
	if err != nil {
		t.Fatalf("get affinity: %v", err)
	}
	defer affinityResp.Body.Close()

	var view affinityView
	if err := json.NewDecoder(affinityResp.Body).Decode(&view); err != nil {
		t.Fatalf("decode affinity: %v", err)
	}
	if got := sortedConversations(view); len(got) != 0 {
		t.Fatalf("unexpected affinity view: %#v", got)
	}
}

func TestAffinityEndpointRejectsUntrustedCaller(t *testing.T) {
	backendURL, err := url.Parse("http://backend.example")
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	handler := newProxyServer(config{
		listenAddr: ":0",
		backendURL: backendURL,
		slotCount:  1,
	})

	req := httptest.NewRequest(http.MethodGet, "/_affinity", nil)
	req.RemoteAddr = "198.51.100.10:1234"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func newProxyTestServer(t *testing.T, backend string, slotCount int) *httptest.Server {
	t.Helper()
	backendURL, err := url.Parse(backend)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	return httptest.NewServer(newProxyServer(config{
		listenAddr: ":0",
		backendURL: backendURL,
		slotCount:  slotCount,
	}))
}

func sortedConversations(view affinityView) []string {
	conversations := make([]string, 0, len(view.Slots))
	for _, slot := range view.Slots {
		if slot.ConversationID != nil {
			conversations = append(conversations, *slot.ConversationID)
		}
	}
	sort.Strings(conversations)
	return conversations
}
