package main

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIdleSessionUnloadDoesNotLogout(t *testing.T) {
	m := setupTestManager(t)
	m.unloadGrace = 15 * time.Millisecond
	client := NewLoggedInMockClient()
	session := injectMockSession(m, 9001, client)
	m.mu.Lock()
	m.scheduleUnloadLocked(session)
	m.mu.Unlock()

	time.Sleep(60 * time.Millisecond)
	if got := m.GetSession(9001); got != nil {
		t.Fatal("expected idle session to unload")
	}
	if len(client.GetCallsByMethod("Logout")) != 0 {
		t.Fatal("idle unload must not log out the linked device")
	}
	if len(client.GetCallsByMethod("Disconnect")) == 0 {
		t.Fatal("idle unload must disconnect the client")
	}
	if len(client.GetCallsByMethod("RemoveEventHandlers")) == 0 {
		t.Fatal("idle unload must remove event handlers")
	}
}

func TestSessionActivityRefreshesUnloadGrace(t *testing.T) {
	m := setupTestManager(t)
	m.unloadGrace = 30 * time.Millisecond
	client := NewLoggedInMockClient()
	session := injectMockSession(m, 9005, client)
	m.mu.Lock()
	m.scheduleUnloadLocked(session)
	m.mu.Unlock()

	time.Sleep(20 * time.Millisecond)
	if got := m.GetSession(9005); got == nil {
		t.Fatal("expected activity before the original deadline")
	}
	time.Sleep(20 * time.Millisecond)
	m.mu.RLock()
	_, stillLoaded := m.sessions[9005]
	m.mu.RUnlock()
	if !stillLoaded {
		t.Fatal("session activity did not refresh the unload grace period")
	}

	time.Sleep(40 * time.Millisecond)
	m.mu.RLock()
	_, stillLoaded = m.sessions[9005]
	m.mu.RUnlock()
	if stillLoaded {
		t.Fatal("session remained loaded after refreshed grace period elapsed")
	}
}

func TestBaileysTransportDoesNotMutateCallerRequest(t *testing.T) {
	var forwarded *http.Request
	transport := &baileysTransport{base: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		forwarded = req
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
	req, err := http.NewRequest(http.MethodGet, "https://example.invalid/media", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Referer", "https://web.whatsapp.com/")
	req.Header.Set("User-Agent", "test-agent")
	req.Header.Set("Origin", "https://web.whatsapp.com")

	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Referer") == "" || req.Header.Get("User-Agent") == "" {
		t.Fatal("transport mutated the caller request")
	}
	if forwarded == req {
		t.Fatal("transport forwarded the caller request without cloning")
	}
	if forwarded.Header.Get("Referer") != "" || forwarded.Header.Get("User-Agent") != "" {
		t.Fatal("transport did not remove browser-identifying headers")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestActiveSSEListenerPreventsUnload(t *testing.T) {
	m := setupTestManager(t)
	m.unloadGrace = 15 * time.Millisecond
	client := NewLoggedInMockClient()
	session := injectMockSession(m, 9002, client)
	m.mu.Lock()
	m.scheduleUnloadLocked(session)
	m.mu.Unlock()

	acquired, ok := m.acquireSSEListener(9002)
	if !ok || acquired != session {
		t.Fatal("failed to acquire listener")
	}
	time.Sleep(50 * time.Millisecond)
	if m.GetSession(9002) == nil {
		t.Fatal("session with an active SSE listener was unloaded")
	}

	m.releaseSSEListener(session)
	time.Sleep(50 * time.Millisecond)
	if m.GetSession(9002) != nil {
		t.Fatal("session did not unload after listener grace period")
	}
}

func TestListenerUnloadRace(t *testing.T) {
	m := setupTestManager(t)
	m.unloadGrace = time.Millisecond
	client := NewLoggedInMockClient()
	session := injectMockSession(m, 9003, client)
	m.mu.Lock()
	m.scheduleUnloadLocked(session)
	m.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, ok := m.acquireSSEListener(9003); ok {
				time.Sleep(time.Millisecond)
				m.releaseSSEListener(got)
			}
		}()
	}
	wg.Wait()
	time.Sleep(10 * time.Millisecond)

	if len(client.GetCallsByMethod("Logout")) != 0 {
		t.Fatal("automatic unload raced into logout")
	}
}

func TestExplicitRemoveStillLogsOut(t *testing.T) {
	m := setupTestManager(t)
	client := NewLoggedInMockClient()
	injectMockSession(m, 9004, client)
	m.RemoveSession(9004)
	if len(client.GetCallsByMethod("Logout")) == 0 {
		t.Fatal("explicit removal must log out")
	}
}
