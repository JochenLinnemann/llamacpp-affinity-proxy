package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func TestNormalizeConversationID(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
		want    string
		ok      bool
	}{
		{
			name:    "Kilo task ID",
			headers: testHeaders(headerKilo, "task-123"),
			want:    "kilo:task-123",
			ok:      true,
		},
		{
			name:    "Kilo session ID fallback",
			headers: testHeaders(headerKiloSession, "session-123"),
			want:    "kilo:session-123",
			ok:      true,
		},
		{
			name:    "Kilo affinity fallback",
			headers: testHeaders(headerKiloAffinity, "affinity-123"),
			want:    "kilo:affinity-123",
			ok:      true,
		},
		{
			name: "priority ignores lower-priority headers",
			headers: testHeaders(
				headerConversation, "chat-123",
				headerHermes, "session-123",
				headerKilo, "task-123",
			),
			want: "owui:chat-123",
			ok:   true,
		},
		{
			name: "empty higher-priority headers fall back",
			headers: testHeaders(
				headerConversation, "  ",
				headerHermes, "",
				headerKilo, "task-123",
			),
			want: "kilo:task-123",
			ok:   true,
		},
		{
			name: "empty headers return no ID",
			headers: testHeaders(
				headerConversation, " ",
				headerHermes, "",
				headerKilo, "\t",
			),
			ok: false,
		},
		{
			name:    "existing OpenWebUI namespace",
			headers: testHeaders(headerConversation, "owui:chat-123"),
			want:    "owui:chat-123",
			ok:      true,
		},
		{
			name:    "existing Hermes namespace",
			headers: testHeaders(headerHermes, "hermes:session-123"),
			want:    "hermes:session-123",
			ok:      true,
		},
		{
			name:    "existing Kilo namespace",
			headers: testHeaders(headerKilo, "kilo:task-123"),
			want:    "kilo:task-123",
			ok:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := normalizeConversationID(tt.headers)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("normalizeConversationID() = %q, %t; want %q, %t", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestProxyForwardsOnlyNormalizedConversationHeader(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"conversation":  req.Header.Get(headerConversation),
			"hermes":        req.Header.Get(headerHermes),
			"kilo":          req.Header.Get(headerKilo),
			"kilo_session":  req.Header.Get(headerKiloSession),
			"kilo_affinity": req.Header.Get(headerKiloAffinity),
		})
	}))
	defer backend.Close()

	server := newProxyTestServer(t, backend.URL, 4)
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(headerHermes, "session-123")
	req.Header.Set(headerKilo, "task-123")
	req.Header.Set(headerKiloSession, "session-fallback-123")
	req.Header.Set(headerKiloAffinity, "affinity-123")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	var payload map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["conversation"] != "hermes:session-123" {
		t.Fatalf("expected normalized conversation header, got %#v", payload)
	}
	if payload["hermes"] != "" || payload["kilo"] != "" || payload["kilo_session"] != "" || payload["kilo_affinity"] != "" {
		t.Fatalf("expected source headers to be removed, got %#v", payload)
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

func TestAffinityDefaultClockAdvances(t *testing.T) {
	manager := newAffinityManager(1)
	first := manager.Acquire("hermes:abc")
	time.Sleep(10 * time.Millisecond)
	second := manager.Acquire("hermes:abc")

	if !second.lastUsed.After(first.lastUsed) {
		t.Fatalf("expected second timestamp %v to be after first %v", second.lastUsed, first.lastUsed)
	}
}

func TestLoadConfigParsesBackendResponseHeaderTimeout(t *testing.T) {
	t.Setenv("LISTEN_ADDR", ":9000")
	t.Setenv("BACKEND_URL", "http://backend.example:8801")
	t.Setenv("SLOT_COUNT", "2")
	t.Setenv("BACKEND_RESPONSE_HEADER_TIMEOUT", "42s")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.backendResponseHeaderTimeout != 42*time.Second {
		t.Fatalf("expected backend response header timeout 42s, got %v", cfg.backendResponseHeaderTimeout)
	}
}

func TestNewBackendTransportSetsResponseHeaderTimeout(t *testing.T) {
	transport, ok := newBackendTransport(42 * time.Second).(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if transport.ResponseHeaderTimeout != 42*time.Second {
		t.Fatalf("expected response header timeout 42s, got %v", transport.ResponseHeaderTimeout)
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

func TestGenerationEndpointsInjectSlotAndPreserveResponseMetadata(t *testing.T) {
	paths := []string{
		"/v1/chat/completions",
		"/v1/completions",
		"/v1/responses",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("read body: %v", err)
				}
				_ = req.Body.Close()
				rw.Header().Set("Content-Type", "application/json")
				rw.Header().Set("X-Backend", "ok")
				rw.WriteHeader(http.StatusCreated)
				_, _ = rw.Write(body)
			}))
			defer backend.Close()

			server := newProxyTestServer(t, backend.URL, 4)
			defer server.Close()

			req, err := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(`{"model":"test"}`))
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

			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("expected 201, got %d", resp.StatusCode)
			}
			if resp.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("unexpected content type %q", resp.Header.Get("Content-Type"))
			}
			if resp.Header.Get("X-Backend") != "ok" {
				t.Fatalf("missing backend header, got %q", resp.Header.Get("X-Backend"))
			}

			var payload map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if payload["id_slot"] != float64(0) {
				t.Fatalf("expected id_slot 0, got %#v", payload["id_slot"])
			}
		})
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

func TestExplicitIDSlotUsesRequestedFreeSlot(t *testing.T) {
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

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["id_slot"] != float64(3) {
		t.Fatalf("expected id_slot 3, got %#v", payload["id_slot"])
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
	if view.Slots[3].ConversationID == nil || *view.Slots[3].ConversationID != "owui:chat-1" {
		t.Fatalf("expected slot 3 to hold conversation, got %#v", view.Slots[3].ConversationID)
	}
}

func TestConflictingExplicitIDSlotReturnsBadRequestWithoutChangingAffinity(t *testing.T) {
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

	firstReq, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test"}`))
	if err != nil {
		t.Fatalf("new first request: %v", err)
	}
	firstReq.Header.Set("Content-Type", "application/json")
	firstReq.Header.Set(headerConversation, "chat-1")

	firstResp, err := http.DefaultClient.Do(firstReq)
	if err != nil {
		t.Fatalf("do first request: %v", err)
	}
	firstResp.Body.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test","id_slot":1}`))
	if err != nil {
		t.Fatalf("new conflicting request: %v", err)
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
	if got := sortedConversations(view); len(got) != 1 || got[0] != "owui:chat-1" {
		t.Fatalf("expected original affinity assignment to remain unchanged, got %#v", got)
	}
	if view.Slots[0].ConversationID == nil || *view.Slots[0].ConversationID != "owui:chat-1" {
		t.Fatalf("expected original slot assignment to remain in slot 0, got %#v", view.Slots[0].ConversationID)
	}
}

func TestNegativeExplicitIDSlotReturnsBadRequest(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		t.Fatal("backend should not receive invalid request")
	}))
	defer backend.Close()

	server := newProxyTestServer(t, backend.URL, 4)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test","id_slot":-1}`))
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
}

func TestScientificNotationExplicitIDSlotReturnsBadRequest(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		t.Fatal("backend should not receive invalid request")
	}))
	defer backend.Close()

	server := newProxyTestServer(t, backend.URL, 4)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test","id_slot":1e0}`))
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
}

func TestNullExplicitIDSlotGetsAssignedByProxy(t *testing.T) {
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

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test","id_slot":null}`))
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

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["id_slot"] != float64(0) {
		t.Fatalf("expected proxy-assigned id_slot 0, got %#v", payload["id_slot"])
	}
}

func TestInvalidBodiesReturnBadRequestWithoutChangingAffinity(t *testing.T) {
	testCases := []struct {
		name string
		body string
	}{
		{name: "null", body: `null`},
		{name: "array", body: `[]`},
		{name: "malformed", body: `{"model":`},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
				t.Fatal("backend should not receive invalid request")
			}))
			defer backend.Close()

			server := newProxyTestServer(t, backend.URL, 1)
			defer server.Close()

			req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(tc.body))
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

			assertNoAffinityEntries(t, server.URL)
		})
	}
}

func TestOversizedRewriteBodyReturnsBadRequest(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		t.Fatal("backend should not receive oversized request")
	}))
	defer backend.Close()

	server := newProxyTestServer(t, backend.URL, 4)
	defer server.Close()

	previousLimit := maxRewriteBodyBytes
	maxRewriteBodyBytes = 16
	defer func() {
		maxRewriteBodyBytes = previousLimit
	}()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewBufferString(`{"model":"test","messages":["this is too large"]}`))
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
}

func TestStreamingResponsesForwardIncrementally(t *testing.T) {
	allowSecondWrite := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := rw.(http.Flusher)
		if !ok {
			t.Fatal("response writer does not support flushing")
		}
		_, _ = rw.Write([]byte("data: first\n\n"))
		flusher.Flush()
		<-allowSecondWrite
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
	if firstChunk != "data: first\n" {
		t.Fatalf("unexpected first chunk %q", firstChunk)
	}

	close(allowSecondWrite)

	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read rest: %v", err)
	}
	if !strings.Contains(string(rest), "data: second") {
		t.Fatalf("missing second chunk in %q", string(rest))
	}
}

func TestOverlappingRequestsWaitForActiveSlotRelease(t *testing.T) {
	firstArrived := make(chan struct{})
	secondArrived := make(chan struct{})
	releaseFirst := make(chan struct{})

	backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		switch req.Header.Get(headerConversation) {
		case "owui:first":
			close(firstArrived)
			<-releaseFirst
			rw.Header().Set("Content-Type", "application/json")
			_, _ = rw.Write([]byte(`{"ok":true}`))
		case "owui:second":
			close(secondArrived)
			rw.Header().Set("Content-Type", "application/json")
			_, _ = rw.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected conversation header %q", req.Header.Get(headerConversation))
		}
	}))
	defer backend.Close()

	server := newProxyTestServer(t, backend.URL, 1)
	defer server.Close()

	firstDone := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(headerConversation, "first")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			firstDone <- err
			return
		}
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		firstDone <- err
	}()

	<-firstArrived

	secondDone := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(headerConversation, "second")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			secondDone <- err
			return
		}
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		secondDone <- err
	}()

	select {
	case <-secondArrived:
		t.Fatal("second request reached backend before first request completed")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseFirst)

	if err := <-firstDone; err != nil {
		t.Fatalf("first request failed: %v", err)
	}

	select {
	case <-secondArrived:
	case <-time.After(time.Second):
		t.Fatal("second request never reached backend after first completed")
	}

	if err := <-secondDone; err != nil {
		t.Fatalf("second request failed: %v", err)
	}
}

func TestCancellationReleasesActiveSlot(t *testing.T) {
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.Header.Get(headerConversation) {
		case "owui:first":
			close(firstStarted)
			<-req.Context().Done()
			return nil, req.Context().Err()
		case "owui:second":
			close(secondStarted)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		default:
			t.Fatalf("unexpected conversation header %q", req.Header.Get(headerConversation))
			return nil, nil
		}
	})

	server := newProxyTestServerWithTransport(t, "http://backend.example", 1, transport)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	firstErr := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(headerConversation, "first")
		_, err := http.DefaultClient.Do(req)
		firstErr <- err
	}()

	<-firstStarted

	secondDone := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(headerConversation, "second")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			secondDone <- err
			return
		}
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		secondDone <- err
	}()

	select {
	case <-secondStarted:
		t.Fatal("second request reached backend before cancellation released the slot")
	case <-time.After(100 * time.Millisecond):
	}

	cancel()

	if err := <-firstErr; err == nil {
		t.Fatal("expected canceled first request to return an error")
	}

	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second request never reached backend after cancellation")
	}

	if err := <-secondDone; err != nil {
		t.Fatalf("second request failed: %v", err)
	}
}

func TestBackendFailureReturnsBadGateway(t *testing.T) {
	server := newProxyTestServerWithTransport(t, "http://backend.example", 1, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("backend unavailable")
	}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test"}`))
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

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", resp.StatusCode)
	}
}

func TestEarlyProxyAbortReleasesReservation(t *testing.T) {
	handler := newProxyServer(config{
		listenAddr: ":0",
		backendURL: mustParseURL(t, "http://backend.example"),
		slotCount:  1,
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		}),
	})

	firstReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test"}`))
	firstReq.Header.Set("Content-Type", "application/json")
	firstReq.Header.Set(headerConversation, "first")
	firstReq.Header.Set("Connection", "Upgrade")
	firstReq.Header.Set("Upgrade", string([]byte{0x7f}))
	firstRec := httptest.NewRecorder()

	handler.ServeHTTP(firstRec, firstReq)

	if firstRec.Code != http.StatusBadGateway {
		t.Fatalf("expected early proxy abort to return 502, got %d", firstRec.Code)
	}

	secondReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test"}`))
	secondReq.Header.Set("Content-Type", "application/json")
	secondReq.Header.Set(headerConversation, "second")
	secondRec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(secondRec, secondReq)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second request blocked after early proxy abort")
	}

	if secondRec.Code != http.StatusOK {
		t.Fatalf("expected second request to succeed, got %d", secondRec.Code)
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

func TestLoopbackDiagnosticsCallerAcceptsIPv6Zone(t *testing.T) {
	if !isLoopbackDiagnosticsCaller("[::1%lo0]:1234") {
		t.Fatal("expected IPv6 loopback with zone to be allowed")
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

func testHeaders(values ...string) http.Header {
	headers := http.Header{}
	for index := 0; index < len(values); index += 2 {
		headers.Set(values[index], values[index+1])
	}
	return headers
}

func newProxyTestServerWithTransport(t *testing.T, backend string, slotCount int, transport http.RoundTripper) *httptest.Server {
	t.Helper()
	return httptest.NewServer(newProxyServer(config{
		listenAddr: ":0",
		backendURL: mustParseURL(t, backend),
		slotCount:  slotCount,
		transport:  transport,
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

func assertNoAffinityEntries(t *testing.T, serverURL string) {
	t.Helper()

	resp, err := http.Get(serverURL + "/_affinity")
	if err != nil {
		t.Fatalf("get affinity: %v", err)
	}
	defer resp.Body.Close()

	var view affinityView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatalf("decode affinity: %v", err)
	}
	if got := sortedConversations(view); len(got) != 0 {
		t.Fatalf("expected no affinity entries, got %#v", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	return parsed
}
