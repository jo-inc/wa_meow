package main

import (
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
