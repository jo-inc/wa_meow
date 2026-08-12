package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Test helper: create a session manager with a mock client injected
func setupTestManager(t *testing.T) *SessionManager {
	t.Helper()
	manager := NewSessionManager(t.TempDir(), "", "")
	manager.connectReadyWait = 20 * time.Millisecond
	manager.connectReadyPoll = time.Millisecond
	return manager
}

// Test helper: inject a mock session into the manager
func injectMockSession(m *SessionManager, userID int, client *MockWhatsAppClient) *UserSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := &UserSession{
		UserID:     userID,
		Client:     client,
		DBPath:     "",
		LastUsed:   time.Now(),
		QRChannel:  make(chan string, 10),
		LoginDone:  make(chan bool, 1),
		EventChan:  make(chan MessageEvent, 100),
		MediaCache: make(map[string]*mediaCacheEntry),
	}
	m.sessions[userID] = session
	return session
}

// ==================== SessionManager Tests ====================

func TestNewSessionManager(t *testing.T) {
	t.Run("creates manager with empty encryption key", func(t *testing.T) {
		m := NewSessionManager("/tmp/test", "", "")
		if m == nil {
			t.Fatal("expected non-nil manager")
		}
		if m.encryptKey != nil {
			t.Error("expected nil encryption key")
		}
	})

	t.Run("creates manager with valid encryption key", func(t *testing.T) {
		key := base64.StdEncoding.EncodeToString(make([]byte, 32))
		m := NewSessionManager("/tmp/test", "http://localhost:8000", key)
		if m == nil {
			t.Fatal("expected non-nil manager")
		}
		if m.encryptKey == nil {
			t.Error("expected non-nil encryption key")
		}
		if len(m.encryptKey) != 32 {
			t.Errorf("expected 32-byte key, got %d", len(m.encryptKey))
		}
	})

	t.Run("ignores invalid encryption key", func(t *testing.T) {
		m := NewSessionManager("/tmp/test", "", "not-valid-base64!")
		if m.encryptKey != nil {
			t.Error("expected nil encryption key for invalid input")
		}
	})

	t.Run("ignores wrong-length encryption key", func(t *testing.T) {
		key := base64.StdEncoding.EncodeToString(make([]byte, 16))
		m := NewSessionManager("/tmp/test", "", key)
		if m.encryptKey != nil {
			t.Error("expected nil encryption key for wrong length")
		}
	})
}

func TestRestoreSessionIfMissing(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	backup := []byte("backup database")
	requests := atomic.Int32{}

	var encrypted string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		json.NewEncoder(w).Encode(map[string]string{"data": encrypted})
	}))
	defer server.Close()

	t.Run("preserves an existing local database", func(t *testing.T) {
		manager := NewSessionManager(t.TempDir(), server.URL, key)
		encrypted, _ = manager.encrypt(backup)
		path := filepath.Join(manager.dataDir, "user_41.db")
		local := []byte("current local database")
		if err := os.WriteFile(path, local, 0600); err != nil {
			t.Fatal(err)
		}
		before := requests.Load()
		if err := manager.restoreSessionIfMissing(41); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(path)
		if !bytes.Equal(got, local) {
			t.Fatalf("existing database was overwritten: %q", got)
		}
		if requests.Load() != before {
			t.Fatal("backend restore was requested for an existing database")
		}
	})

	t.Run("restores a missing local database", func(t *testing.T) {
		manager := NewSessionManager(t.TempDir(), server.URL, key)
		encrypted, _ = manager.encrypt(backup)
		before := requests.Load()
		if err := manager.restoreSessionIfMissing(42); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(manager.dataDir, "user_42.db"))
		if !bytes.Equal(got, backup) {
			t.Fatalf("expected restored backup, got %q", got)
		}
		if requests.Load() != before+1 {
			t.Fatal("expected one backend restore request")
		}
	})
}

func TestEncryptDecrypt(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	m := NewSessionManager("/tmp/test", "", key)

	t.Run("encrypts and decrypts successfully", func(t *testing.T) {
		original := []byte("hello world, this is a test message")
		encrypted, err := m.encrypt(original)
		if err != nil {
			t.Fatalf("encrypt failed: %v", err)
		}
		if encrypted == "" {
			t.Fatal("expected non-empty encrypted string")
		}

		decrypted, err := m.decrypt(encrypted)
		if err != nil {
			t.Fatalf("decrypt failed: %v", err)
		}
		if string(decrypted) != string(original) {
			t.Errorf("expected %q, got %q", original, decrypted)
		}
	})

	t.Run("different encryptions produce different ciphertexts", func(t *testing.T) {
		data := []byte("same data")
		enc1, _ := m.encrypt(data)
		enc2, _ := m.encrypt(data)
		if enc1 == enc2 {
			t.Error("expected different ciphertexts due to random nonce")
		}
	})

	t.Run("fails to decrypt with no key", func(t *testing.T) {
		m2 := NewSessionManager("/tmp/test", "", "")
		_, err := m2.decrypt("somedata")
		if err == nil {
			t.Error("expected error when decrypting without key")
		}
	})

	t.Run("fails to encrypt with no key", func(t *testing.T) {
		m2 := NewSessionManager("/tmp/test", "", "")
		_, err := m2.encrypt([]byte("test"))
		if err == nil {
			t.Error("expected error when encrypting without key")
		}
	})

	t.Run("fails to decrypt invalid base64", func(t *testing.T) {
		_, err := m.decrypt("not-valid-base64!!!")
		if err == nil {
			t.Error("expected error for invalid base64")
		}
	})

	t.Run("fails to decrypt short ciphertext", func(t *testing.T) {
		short := base64.StdEncoding.EncodeToString([]byte("abc"))
		_, err := m.decrypt(short)
		if err == nil {
			t.Error("expected error for too-short ciphertext")
		}
	})
}

func TestPIIFingerprint(t *testing.T) {
	t.Run("returns stable hash for same input", func(t *testing.T) {
		first := piiFingerprint("1234567890@s.whatsapp.net")
		second := piiFingerprint("1234567890@s.whatsapp.net")
		if first != second {
			t.Fatalf("expected stable fingerprint, got %q vs %q", first, second)
		}
		if first == "1234567890@s.whatsapp.net" || first == "" {
			t.Fatalf("expected redacted fingerprint, got %q", first)
		}
	})

	t.Run("handles empty input", func(t *testing.T) {
		if got := piiFingerprint(""); got != "none" {
			t.Fatalf("expected none, got %q", got)
		}
	})
}

func TestPIIPresence(t *testing.T) {
	if got := piiPresence(""); got != "absent" {
		t.Fatalf("expected absent, got %q", got)
	}

	got := piiPresence("/v/t62.7117-24/signed-token")
	if !strings.Contains(got, "present(hash=") || strings.Contains(got, "signed-token") {
		t.Fatalf("expected redacted presence summary, got %q", got)
	}
}

func TestClassifyWhatsAppClientError(t *testing.T) {
	tests := []struct {
		message string
		want    string
	}{
		{"Error reading from websocket: failed to read frame header: EOF", "socket_eof"},
		{"Failed to sync app state: mismatching LTHash", "app_state_lthash"},
		{"Error reconnecting after autoreconnect sleep: network unavailable", "reconnect_failed"},
		{"unrelated client error", ""},
	}
	for _, test := range tests {
		if got := classifyWhatsAppClientError(test.message); got != test.want {
			t.Errorf("classifyWhatsAppClientError(%q) = %q, want %q", test.message, got, test.want)
		}
	}
}

func TestSocketEOFBurst(t *testing.T) {
	socketEOFBurstState.Lock()
	socketEOFBurstState.occurred = nil
	socketEOFBurstState.lastReported = time.Time{}
	socketEOFBurstState.Unlock()

	now := time.Now()
	if isSocketEOFBurst(now) || isSocketEOFBurst(now.Add(time.Second)) {
		t.Fatal("expected fewer than three EOFs not to be a burst")
	}
	if !isSocketEOFBurst(now.Add(2 * time.Second)) {
		t.Fatal("expected three EOFs within five minutes to be a burst")
	}
	if isSocketEOFBurst(now.Add(3 * time.Second)) {
		t.Fatal("expected burst reporting cooldown")
	}
}

func TestWhatsAppTransportSignal(t *testing.T) {
	tests := []struct {
		event interface{}
		want  string
	}{
		{&events.Connected{}, "connected"},
		{&events.Disconnected{}, "disconnected"},
		{&events.LoggedOut{}, "logged_out"},
		{&events.StreamReplaced{}, "stream_replaced"},
		{&events.ConnectFailure{}, "connect_failure"},
		{&events.KeepAliveTimeout{}, "keepalive_timeout"},
		{&events.KeepAliveRestored{}, "keepalive_restored"},
		{&events.Message{}, ""},
	}
	for _, test := range tests {
		if got := whatsappTransportSignal(test.event); got != test.want {
			t.Errorf("whatsappTransportSignal(%T) = %q, want %q", test.event, got, test.want)
		}
	}
}

func TestSessionManager_GetSession(t *testing.T) {
	m := setupTestManager(t)

	t.Run("returns nil for non-existent session", func(t *testing.T) {
		session := m.GetSession(12345)
		if session != nil {
			t.Error("expected nil for non-existent session")
		}
	})

	t.Run("returns existing session", func(t *testing.T) {
		mock := NewLoggedInMockClient()
		injectMockSession(m, 100, mock)

		session := m.GetSession(100)
		if session == nil {
			t.Fatal("expected session to exist")
		}
		if session.UserID != 100 {
			t.Errorf("expected userID 100, got %d", session.UserID)
		}
	})

	t.Run("updates LastUsed on access", func(t *testing.T) {
		mock := NewLoggedInMockClient()
		session := injectMockSession(m, 101, mock)
		oldTime := session.LastUsed

		time.Sleep(10 * time.Millisecond)
		m.GetSession(101)

		if !session.LastUsed.After(oldTime) {
			t.Error("expected LastUsed to be updated")
		}
	})
}

func TestSessionManager_RemoveSession(t *testing.T) {
	m := setupTestManager(t)

	t.Run("does not panic for non-existent session", func(t *testing.T) {
		m.RemoveSession(12345) // Should not panic
	})

	t.Run("removes and logs out session when logged in", func(t *testing.T) {
		mock := NewLoggedInMockClient()
		injectMockSession(m, 200, mock)

		m.RemoveSession(200)

		if m.GetSession(200) != nil {
			t.Error("expected session to be removed")
		}
		calls := mock.GetCallsByMethod("Logout")
		if len(calls) == 0 {
			t.Error("expected Logout to be called")
		}
	})

	t.Run("removes and disconnects session when not logged in", func(t *testing.T) {
		mock := NewConnectedMockClient()
		injectMockSession(m, 201, mock)

		m.RemoveSession(201)

		if m.GetSession(201) != nil {
			t.Error("expected session to be removed")
		}
		calls := mock.GetCallsByMethod("Disconnect")
		if len(calls) == 0 {
			t.Error("expected Disconnect to be called")
		}
	})
}

// ==================== Helper Function Tests ====================

func TestJsonResponse(t *testing.T) {
	w := httptest.NewRecorder()
	data := map[string]interface{}{
		"name":  "test",
		"value": 42,
	}

	jsonResponse(w, data)

	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", w.Header().Get("Content-Type"))
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["name"] != "test" {
		t.Errorf("expected name 'test', got %v", resp["name"])
	}
}

func TestErrorResponse(t *testing.T) {
	w := httptest.NewRecorder()
	errorResponse(w, http.StatusBadRequest, "something went wrong")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type application/json")
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "something went wrong" {
		t.Errorf("expected error message, got %q", resp["error"])
	}
}

// ==================== Health Handler Tests ====================

func TestHealthHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	healthHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "ok" {
		t.Errorf("expected status 'ok', got %q", resp["status"])
	}
}

// ==================== Session Handler Tests ====================

func TestCreateSessionHandler(t *testing.T) {
	t.Run("rejects non-POST methods", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
		w := httptest.NewRecorder()
		createSessionHandler(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected 405, got %d", w.Code)
		}
	})

	t.Run("rejects invalid JSON", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString("not json"))
		w := httptest.NewRecorder()
		createSessionHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("recovers when WhatsApp deleted the cached device", func(t *testing.T) {
		var deleted atomic.Bool
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete || r.URL.Path != "/api/whatsapp/session" || r.URL.Query().Get("user_id") != "302" {
				t.Errorf("unexpected backend request: %s %s", r.Method, r.URL.String())
				http.Error(w, "unexpected request", http.StatusBadRequest)
				return
			}
			deleted.Store(true)
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		manager = setupTestManager(t)
		manager.joBotURL = backend.URL
		mock := NewMockClient()
		mock.ConnectError = store.ErrDeviceDeleted
		injectMockSession(manager, 302, mock)

		req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(`{"user_id":302}`))
		w := httptest.NewRecorder()
		createSessionHandler(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp map[string]interface{}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp["status"] != "needs_qr" {
			t.Fatalf("expected needs_qr, got %v", resp["status"])
		}
		if manager.GetSession(302) != nil {
			t.Fatal("expected deleted device session to be discarded")
		}
		if !deleted.Load() {
			t.Fatal("expected persisted deleted device session to be removed")
		}
	})
}

func TestGetStatusHandler(t *testing.T) {
	t.Run("requires user_id parameter", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/sessions/status", nil)
		w := httptest.NewRecorder()
		getStatusHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("returns disconnected for unknown session", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/sessions/status?user_id=99999", nil)
		w := httptest.NewRecorder()
		getStatusHandler(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		var resp map[string]interface{}
		json.NewDecoder(w.Body).Decode(&resp)
		if resp["connected"] != false {
			t.Error("expected connected=false")
		}
	})

	t.Run("returns status for connected session", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		injectMockSession(manager, 300, mock)

		req := httptest.NewRequest(http.MethodGet, "/sessions/status?user_id=300", nil)
		w := httptest.NewRecorder()
		getStatusHandler(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		var resp map[string]interface{}
		json.NewDecoder(w.Body).Decode(&resp)
		if resp["connected"] != true {
			t.Error("expected connected=true")
		}
		if resp["logged_in"] != true {
			t.Error("expected logged_in=true")
		}
		if resp["phone"] != "1234567890" {
			t.Errorf("expected phone '1234567890', got %v", resp["phone"])
		}
	})
}

func TestDeleteSessionHandler(t *testing.T) {
	t.Run("rejects non-DELETE methods", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodPost, "/sessions/delete?user_id=1", nil)
		w := httptest.NewRecorder()
		deleteSessionHandler(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected 405, got %d", w.Code)
		}
	})

	t.Run("requires user_id parameter", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodDelete, "/sessions/delete", nil)
		w := httptest.NewRecorder()
		deleteSessionHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("disconnects existing session", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		injectMockSession(manager, 400, mock)

		req := httptest.NewRequest(http.MethodDelete, "/sessions/delete?user_id=400", nil)
		w := httptest.NewRecorder()
		deleteSessionHandler(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		var resp map[string]string
		json.NewDecoder(w.Body).Decode(&resp)
		if resp["status"] != "disconnected" {
			t.Errorf("expected status 'disconnected', got %q", resp["status"])
		}

		// Verify Logout was called (since mock was logged in)
		calls := mock.GetCallsByMethod("Logout")
		if len(calls) == 0 {
			t.Error("expected Logout to be called")
		}
	})
}

func TestGetQRHandler(t *testing.T) {
	t.Run("requires user_id parameter", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/sessions/qr", nil)
		w := httptest.NewRecorder()
		getQRHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("returns 404 for unknown session", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/sessions/qr?user_id=99999", nil)
		w := httptest.NewRecorder()
		getQRHandler(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})
}

func TestSaveSessionHandler(t *testing.T) {
	t.Run("rejects non-POST methods", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/sessions/save?user_id=1", nil)
		w := httptest.NewRecorder()
		saveSessionHandler(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected 405, got %d", w.Code)
		}
	})

	t.Run("requires user_id parameter", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodPost, "/sessions/save", nil)
		w := httptest.NewRecorder()
		saveSessionHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("returns success for existing session", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		injectMockSession(manager, 500, mock)

		req := httptest.NewRequest(http.MethodPost, "/sessions/save?user_id=500", nil)
		w := httptest.NewRecorder()
		saveSessionHandler(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		var resp map[string]string
		json.NewDecoder(w.Body).Decode(&resp)
		if resp["status"] != "saved" {
			t.Errorf("expected status 'saved', got %q", resp["status"])
		}
	})
}

// ==================== Message Handler Tests ====================

func TestSendMessageHandler(t *testing.T) {
	t.Run("rejects non-POST methods", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/messages/send", nil)
		w := httptest.NewRecorder()
		sendMessageHandler(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected 405, got %d", w.Code)
		}
	})

	t.Run("rejects invalid JSON", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodPost, "/messages/send", bytes.NewBufferString("bad"))
		w := httptest.NewRecorder()
		sendMessageHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("returns 404 for unknown session", func(t *testing.T) {
		manager = setupTestManager(t)
		body := `{"user_id": 99999, "chat_jid": "123@s.whatsapp.net", "text": "hello"}`
		req := httptest.NewRequest(http.MethodPost, "/messages/send", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		sendMessageHandler(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})

	t.Run("restores and reconnects an unloaded session", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		mock.SetConnected(false)
		manager.loadSession = func(userID int) (*UserSession, error) {
			if userID != 601 {
				t.Fatalf("expected user 601, got %d", userID)
			}
			return injectMockSession(manager, userID, mock), nil
		}

		body := `{"user_id": 601, "chat_jid": "1234567890@s.whatsapp.net", "text": "hello"}`
		req := httptest.NewRequest(http.MethodPost, "/messages/send", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		sendMessageHandler(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if calls := mock.GetCallsByMethod("Connect"); len(calls) != 1 {
			t.Fatalf("expected one reconnect, got %d", len(calls))
		}
		if calls := mock.GetCallsByMethod("SendMessage"); len(calls) != 1 {
			t.Fatalf("expected one message send, got %d", len(calls))
		}
	})

	t.Run("returns 400 when not logged in", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewConnectedMockClient() // Connected but not logged in
		injectMockSession(manager, 600, mock)

		body := `{"user_id": 600, "chat_jid": "123@s.whatsapp.net", "text": "hello"}`
		req := httptest.NewRequest(http.MethodPost, "/messages/send", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		sendMessageHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("sends message successfully", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		injectMockSession(manager, 602, mock)

		body := `{"user_id": 602, "chat_jid": "1234567890@s.whatsapp.net", "text": "hello world"}`
		req := httptest.NewRequest(http.MethodPost, "/messages/send", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		sendMessageHandler(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		var resp map[string]interface{}
		json.NewDecoder(w.Body).Decode(&resp)
		if resp["id"] != "mock-msg-id" {
			t.Errorf("expected id 'mock-msg-id', got %v", resp["id"])
		}

		calls := mock.GetCallsByMethod("SendMessage")
		if len(calls) != 1 {
			t.Errorf("expected 1 SendMessage call, got %d", len(calls))
		}
	})

	t.Run("waits for reconnect login readiness", func(t *testing.T) {
		manager = setupTestManager(t)
		manager.connectReadyWait = 200 * time.Millisecond
		mock := NewLoggedInMockClient()
		mock.SetConnected(false)
		mock.SetLoggedIn(false)
		injectMockSession(manager, 605, mock)
		go func() {
			time.Sleep(10 * time.Millisecond)
			mock.SetLoggedIn(true)
		}()

		body := `{"user_id":605,"chat_jid":"1234567890@s.whatsapp.net","text":"hello"}`
		req := httptest.NewRequest(http.MethodPost, "/messages/send", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		sendMessageHandler(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 after login became ready, got %d: %s", w.Code, w.Body.String())
		}
		if calls := mock.GetCallsByMethod("Connect"); len(calls) != 1 {
			t.Fatalf("expected one reconnect, got %d", len(calls))
		}
	})

	t.Run("deduplicates a stable delivery key after manager restart", func(t *testing.T) {
		dataDir := t.TempDir()
		manager = NewSessionManager(dataDir, "", "")
		manager.connectReadyWait = 20 * time.Millisecond
		manager.connectReadyPoll = time.Millisecond
		firstClient := NewLoggedInMockClient()
		injectMockSession(manager, 606, firstClient)
		body := `{"user_id":606,"chat_jid":"1234567890@s.whatsapp.net","text":"hello","delivery_key":"cron-v1-test"}`

		first := httptest.NewRecorder()
		sendMessageHandler(first, httptest.NewRequest(http.MethodPost, "/messages/send", bytes.NewBufferString(body)))
		if first.Code != http.StatusOK {
			t.Fatalf("first send failed: %d: %s", first.Code, first.Body.String())
		}
		calls := firstClient.GetCallsByMethod("SendMessage")
		if len(calls) != 1 {
			t.Fatalf("expected one provider send, got %d", len(calls))
		}
		extra := calls[0].Args[3].([]whatsmeow.SendRequestExtra)
		if len(extra) != 1 || extra[0].ID != stableDeliveryMessageID(606, "cron-v1-test") {
			t.Fatalf("provider message ID was not stable: %#v", extra)
		}

		manager = NewSessionManager(dataDir, "", "")
		secondClient := NewLoggedInMockClient()
		injectMockSession(manager, 606, secondClient)
		second := httptest.NewRecorder()
		sendMessageHandler(second, httptest.NewRequest(http.MethodPost, "/messages/send", bytes.NewBufferString(body)))
		if second.Code != http.StatusOK {
			t.Fatalf("duplicate request failed: %d: %s", second.Code, second.Body.String())
		}
		if calls := secondClient.GetCallsByMethod("SendMessage"); len(calls) != 0 {
			t.Fatalf("expected no duplicate provider send, got %d", len(calls))
		}
		var response map[string]interface{}
		json.NewDecoder(second.Body).Decode(&response)
		if response["deduplicated"] != true {
			t.Fatalf("expected deduplicated response, got %#v", response)
		}
	})

	t.Run("handles SendMessage error", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		mock.SendMessageError = errors.New("network error")
		injectMockSession(manager, 603, mock)

		body := `{"user_id": 603, "chat_jid": "1234567890@s.whatsapp.net", "text": "hello"}`
		req := httptest.NewRequest(http.MethodPost, "/messages/send", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		sendMessageHandler(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", w.Code)
		}
	})

	t.Run("sends reply message", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		injectMockSession(manager, 604, mock)

		body := `{"user_id": 604, "chat_jid": "1234567890@s.whatsapp.net", "text": "reply", "reply_to": "original-msg-id"}`
		req := httptest.NewRequest(http.MethodPost, "/messages/send", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		sendMessageHandler(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})
}

func TestSendReactionHandler(t *testing.T) {
	t.Run("rejects non-POST methods", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/messages/react", nil)
		w := httptest.NewRecorder()
		sendReactionHandler(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected 405, got %d", w.Code)
		}
	})

	t.Run("rejects invalid JSON", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodPost, "/messages/react", bytes.NewBufferString("bad"))
		w := httptest.NewRecorder()
		sendReactionHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("returns 404 for unknown session", func(t *testing.T) {
		manager = setupTestManager(t)
		body := `{"user_id": 99999, "chat_jid": "123@s.whatsapp.net", "message_id": "msg-1", "emoji": "👍"}`
		req := httptest.NewRequest(http.MethodPost, "/messages/react", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		sendReactionHandler(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})

	t.Run("returns 400 when not logged in", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewConnectedMockClient()
		injectMockSession(manager, 700, mock)

		body := `{"user_id": 700, "chat_jid": "123@s.whatsapp.net", "message_id": "msg-1", "emoji": "👍"}`
		req := httptest.NewRequest(http.MethodPost, "/messages/react", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		sendReactionHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("sends reaction successfully", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		injectMockSession(manager, 701, mock)

		body := `{"user_id": 701, "chat_jid": "1234567890@s.whatsapp.net", "message_id": "msg-123", "emoji": "👍"}`
		req := httptest.NewRequest(http.MethodPost, "/messages/react", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		sendReactionHandler(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		calls := mock.GetCallsByMethod("SendMessage")
		if len(calls) != 1 {
			t.Errorf("expected 1 SendMessage call, got %d", len(calls))
		}
	})

	t.Run("handles SendMessage error", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		mock.SendMessageError = errors.New("reaction failed")
		injectMockSession(manager, 702, mock)

		body := `{"user_id": 702, "chat_jid": "1234567890@s.whatsapp.net", "message_id": "msg-123", "emoji": "👍"}`
		req := httptest.NewRequest(http.MethodPost, "/messages/react", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		sendReactionHandler(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", w.Code)
		}
	})
}

func TestSetTypingHandler(t *testing.T) {
	t.Run("rejects non-POST methods", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/messages/typing", nil)
		w := httptest.NewRecorder()
		setTypingHandler(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected 405, got %d", w.Code)
		}
	})

	t.Run("rejects invalid JSON", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodPost, "/messages/typing", bytes.NewBufferString("bad"))
		w := httptest.NewRecorder()
		setTypingHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("returns 404 for unknown session", func(t *testing.T) {
		manager = setupTestManager(t)
		body := `{"user_id": 99999, "chat_jid": "123@s.whatsapp.net", "typing": true}`
		req := httptest.NewRequest(http.MethodPost, "/messages/typing", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		setTypingHandler(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})

	t.Run("returns 400 when not logged in", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewConnectedMockClient()
		injectMockSession(manager, 799, mock)

		body := `{"user_id": 799, "chat_jid": "123@s.whatsapp.net", "typing": true}`
		req := httptest.NewRequest(http.MethodPost, "/messages/typing", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		setTypingHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("sets typing indicator successfully", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		injectMockSession(manager, 800, mock)

		body := `{"user_id": 800, "chat_jid": "1234567890@s.whatsapp.net", "typing": true}`
		req := httptest.NewRequest(http.MethodPost, "/messages/typing", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		setTypingHandler(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		calls := mock.GetCallsByMethod("SendChatPresence")
		if len(calls) != 1 {
			t.Errorf("expected 1 SendChatPresence call, got %d", len(calls))
		}
	})

	t.Run("sets typing false (paused)", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		injectMockSession(manager, 802, mock)

		body := `{"user_id": 802, "chat_jid": "1234567890@s.whatsapp.net", "typing": false}`
		req := httptest.NewRequest(http.MethodPost, "/messages/typing", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		setTypingHandler(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("handles SendChatPresence error", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		mock.SendPresenceError = errors.New("presence error")
		injectMockSession(manager, 801, mock)

		body := `{"user_id": 801, "chat_jid": "1234567890@s.whatsapp.net", "typing": true}`
		req := httptest.NewRequest(http.MethodPost, "/messages/typing", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		setTypingHandler(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", w.Code)
		}
	})
}

func TestSendImageHandler(t *testing.T) {
	t.Run("rejects non-POST methods", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/messages/image", nil)
		w := httptest.NewRecorder()
		sendImageHandler(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected 405, got %d", w.Code)
		}
	})

	t.Run("rejects invalid JSON", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodPost, "/messages/image", bytes.NewBufferString("bad"))
		w := httptest.NewRecorder()
		sendImageHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("returns 404 for unknown session", func(t *testing.T) {
		manager = setupTestManager(t)
		imgData := base64.StdEncoding.EncodeToString([]byte("img"))
		body := `{"user_id": 99999, "chat_jid": "123@s.whatsapp.net", "image_b64": "` + imgData + `", "mime_type": "image/jpeg"}`
		req := httptest.NewRequest(http.MethodPost, "/messages/image", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		sendImageHandler(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})

	t.Run("returns 400 when not logged in", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewConnectedMockClient()
		injectMockSession(manager, 899, mock)

		imgData := base64.StdEncoding.EncodeToString([]byte("img"))
		body := `{"user_id": 899, "chat_jid": "123@s.whatsapp.net", "image_b64": "` + imgData + `", "mime_type": "image/jpeg"}`
		req := httptest.NewRequest(http.MethodPost, "/messages/image", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		sendImageHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("rejects invalid base64", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		injectMockSession(manager, 900, mock)

		body := `{"user_id": 900, "chat_jid": "123@s.whatsapp.net", "image_b64": "not-valid-base64!!!", "mime_type": "image/jpeg"}`
		req := httptest.NewRequest(http.MethodPost, "/messages/image", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		sendImageHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("sends image successfully", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		injectMockSession(manager, 901, mock)

		imgData := base64.StdEncoding.EncodeToString([]byte("fake-image-data"))
		body := `{"user_id": 901, "chat_jid": "1234567890@s.whatsapp.net", "image_b64": "` + imgData + `", "mime_type": "image/jpeg", "caption": "test image"}`
		req := httptest.NewRequest(http.MethodPost, "/messages/image", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		sendImageHandler(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		uploadCalls := mock.GetCallsByMethod("Upload")
		if len(uploadCalls) != 1 {
			t.Errorf("expected 1 Upload call, got %d", len(uploadCalls))
		}

		sendCalls := mock.GetCallsByMethod("SendMessage")
		if len(sendCalls) != 1 {
			t.Errorf("expected 1 SendMessage call, got %d", len(sendCalls))
		}
	})

	t.Run("handles upload error", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		mock.UploadError = errors.New("upload failed")
		injectMockSession(manager, 902, mock)

		imgData := base64.StdEncoding.EncodeToString([]byte("fake-image-data"))
		body := `{"user_id": 902, "chat_jid": "1234567890@s.whatsapp.net", "image_b64": "` + imgData + `", "mime_type": "image/jpeg"}`
		req := httptest.NewRequest(http.MethodPost, "/messages/image", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		sendImageHandler(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", w.Code)
		}
	})

	t.Run("handles SendMessage error", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		mock.SendMessageError = errors.New("send failed")
		injectMockSession(manager, 903, mock)

		imgData := base64.StdEncoding.EncodeToString([]byte("fake-image-data"))
		body := `{"user_id": 903, "chat_jid": "1234567890@s.whatsapp.net", "image_b64": "` + imgData + `", "mime_type": "image/jpeg"}`
		req := httptest.NewRequest(http.MethodPost, "/messages/image", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		sendImageHandler(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", w.Code)
		}
	})
}

func TestSendLocationHandler(t *testing.T) {
	t.Run("rejects non-POST methods", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/messages/location", nil)
		w := httptest.NewRecorder()
		sendLocationHandler(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected 405, got %d", w.Code)
		}
	})

	t.Run("rejects invalid JSON", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodPost, "/messages/location", bytes.NewBufferString("bad"))
		w := httptest.NewRecorder()
		sendLocationHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("returns 404 for unknown session", func(t *testing.T) {
		manager = setupTestManager(t)
		body := `{"user_id": 99999, "chat_jid": "123@s.whatsapp.net", "latitude": 0, "longitude": 0}`
		req := httptest.NewRequest(http.MethodPost, "/messages/location", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		sendLocationHandler(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})

	t.Run("returns 400 when not logged in", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewConnectedMockClient()
		injectMockSession(manager, 1000, mock)

		body := `{"user_id": 1000, "chat_jid": "123@s.whatsapp.net", "latitude": 0, "longitude": 0}`
		req := httptest.NewRequest(http.MethodPost, "/messages/location", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		sendLocationHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("sends location successfully", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		injectMockSession(manager, 1001, mock)

		body := `{"user_id": 1001, "chat_jid": "1234567890@s.whatsapp.net", "latitude": 37.7749, "longitude": -122.4194, "name": "San Francisco", "address": "CA, USA"}`
		req := httptest.NewRequest(http.MethodPost, "/messages/location", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		sendLocationHandler(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		calls := mock.GetCallsByMethod("SendMessage")
		if len(calls) != 1 {
			t.Errorf("expected 1 SendMessage call, got %d", len(calls))
		}
	})

	t.Run("handles SendMessage error", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		mock.SendMessageError = errors.New("location send failed")
		injectMockSession(manager, 1002, mock)

		body := `{"user_id": 1002, "chat_jid": "1234567890@s.whatsapp.net", "latitude": 37.7749, "longitude": -122.4194}`
		req := httptest.NewRequest(http.MethodPost, "/messages/location", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		sendLocationHandler(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", w.Code)
		}
	})
}

// ==================== Chat Handler Tests ====================

func TestDownloadMediaHandler(t *testing.T) {
	t.Run("rejects non-POST methods", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/media/download", nil)
		w := httptest.NewRecorder()
		downloadMediaHandler(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected 405, got %d", w.Code)
		}
	})

	t.Run("rejects invalid JSON", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodPost, "/media/download", bytes.NewBufferString("bad"))
		w := httptest.NewRecorder()
		downloadMediaHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("returns 404 for unknown session", func(t *testing.T) {
		manager = setupTestManager(t)
		body := `{"user_id": 99999, "url": "https://example.com/media", "mime_type": "image/jpeg"}`
		req := httptest.NewRequest(http.MethodPost, "/media/download", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		downloadMediaHandler(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})

	t.Run("returns 400 when not logged in", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewConnectedMockClient()
		injectMockSession(manager, 1399, mock)

		body := `{"user_id": 1399, "url": "https://example.com/media", "mime_type": "image/jpeg"}`
		req := httptest.NewRequest(http.MethodPost, "/media/download", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		downloadMediaHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("downloads media successfully", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		mock.DownloadData = []byte("image-binary-data")
		injectMockSession(manager, 1400, mock)

		body := `{"user_id": 1400, "url": "https://example.com/media", "mime_type": "image/jpeg"}`
		req := httptest.NewRequest(http.MethodPost, "/media/download", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		downloadMediaHandler(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		var resp map[string]interface{}
		json.NewDecoder(w.Body).Decode(&resp)
		if resp["mime_type"] != "image/jpeg" {
			t.Errorf("expected mime_type 'image/jpeg', got %v", resp["mime_type"])
		}

		dataB64, ok := resp["data"].(string)
		if !ok {
			t.Fatal("expected data to be a string")
		}
		decoded, _ := base64.StdEncoding.DecodeString(dataB64)
		if string(decoded) != "image-binary-data" {
			t.Errorf("expected decoded data 'image-binary-data', got %s", decoded)
		}
	})

	t.Run("handles download error", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		mock.DownloadError = errors.New("download failed")
		injectMockSession(manager, 1401, mock)

		body := `{"user_id": 1401, "url": "https://example.com/media", "mime_type": "image/jpeg"}`
		req := httptest.NewRequest(http.MethodPost, "/media/download", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		downloadMediaHandler(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", w.Code)
		}
	})
}

// ==================== Events Handler Tests ====================

func TestEventsHandler(t *testing.T) {
	t.Run("requires user_id parameter", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/events", nil)
		w := httptest.NewRecorder()
		eventsHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("returns 404 for unknown session", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/events?user_id=99999", nil)
		w := httptest.NewRecorder()
		eventsHandler(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})
}

// ==================== HandleEvent Tests ====================

func TestUserSession_handleEvent(t *testing.T) {
	ptr := func(s string) *string { return &s }
	ptrF := func(f float64) *float64 { return &f }
	ptrU := func(u uint64) *uint64 { return &u }

	// Helper to create MessageInfo with embedded MessageSource
	makeInfo := func(id string) types.MessageInfo {
		return types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:   types.JID{User: "chat", Server: types.DefaultUserServer},
				Sender: types.JID{User: "sender", Server: types.DefaultUserServer},
			},
			ID:        types.MessageID(id),
			Timestamp: time.Now(),
		}
	}

	// Helper to create a test session with mock client
	makeTestSession := func() *UserSession {
		return &UserSession{
			UserID:     1,
			Client:     NewLoggedInMockClient(),
			EventChan:  make(chan MessageEvent, 10),
			MediaCache: make(map[string]*mediaCacheEntry),
		}
	}

	t.Run("handles text message with Conversation", func(t *testing.T) {
		session := makeTestSession()

		evt := &events.Message{
			Info: types.MessageInfo{
				MessageSource: types.MessageSource{
					Chat:     types.JID{User: "chat123", Server: types.DefaultUserServer},
					Sender:   types.JID{User: "sender456", Server: types.DefaultUserServer},
					IsFromMe: false,
				},
				ID:        "msg-001",
				PushName:  "John",
				Timestamp: time.Unix(1234567890, 0),
			},
			Message: &waE2E.Message{
				Conversation: ptr("Hello world"),
			},
		}

		session.handleEvent(evt)

		select {
		case msg := <-session.EventChan:
			if msg.Type != "message" {
				t.Errorf("expected type 'message', got %q", msg.Type)
			}
			payload := msg.Payload.(MessagePayload)
			if payload.Text != "Hello world" {
				t.Errorf("expected text 'Hello world', got %q", payload.Text)
			}
			if payload.ID != "msg-001" {
				t.Errorf("expected id 'msg-001', got %q", payload.ID)
			}
		default:
			t.Fatal("expected message in channel")
		}
	})

	t.Run("handles ExtendedTextMessage", func(t *testing.T) {
		session := makeTestSession()

		evt := &events.Message{
			Info: makeInfo("msg-002"),
			Message: &waE2E.Message{
				ExtendedTextMessage: &waE2E.ExtendedTextMessage{
					Text: ptr("Extended text message"),
				},
			},
		}

		session.handleEvent(evt)

		msg := <-session.EventChan
		payload := msg.Payload.(MessagePayload)
		if payload.Text != "Extended text message" {
			t.Errorf("expected 'Extended text message', got %q", payload.Text)
		}
	})

	t.Run("handles image message", func(t *testing.T) {
		session := makeTestSession()

		evt := &events.Message{
			Info: makeInfo("msg-003"),
			Message: &waE2E.Message{
				ImageMessage: &waE2E.ImageMessage{
					Caption:    ptr("My photo"),
					Mimetype:   ptr("image/jpeg"),
					URL:        ptr("https://example.com/img.jpg"),
					DirectPath: ptr("/v/media/123"),
					FileLength: ptrU(12345),
				},
			},
		}

		session.handleEvent(evt)

		msg := <-session.EventChan
		payload := msg.Payload.(MessagePayload)
		if payload.MediaType != "image" {
			t.Errorf("expected media_type 'image', got %q", payload.MediaType)
		}
		if payload.Caption != "My photo" {
			t.Errorf("expected caption 'My photo', got %q", payload.Caption)
		}
		if payload.MimeType != "image/jpeg" {
			t.Errorf("expected mime_type 'image/jpeg', got %q", payload.MimeType)
		}
	})

	t.Run("handles location message", func(t *testing.T) {
		session := makeTestSession()

		evt := &events.Message{
			Info: makeInfo("msg-004"),
			Message: &waE2E.Message{
				LocationMessage: &waE2E.LocationMessage{
					DegreesLatitude:  ptrF(37.7749),
					DegreesLongitude: ptrF(-122.4194),
					Name:             ptr("San Francisco"),
					Address:          ptr("CA, USA"),
				},
			},
		}

		session.handleEvent(evt)

		msg := <-session.EventChan
		payload := msg.Payload.(MessagePayload)
		if payload.MediaType != "location" {
			t.Errorf("expected media_type 'location', got %q", payload.MediaType)
		}
		if payload.Latitude != 37.7749 {
			t.Errorf("expected latitude 37.7749, got %f", payload.Latitude)
		}
		if payload.Text != "San Francisco - CA, USA" {
			t.Errorf("expected text 'San Francisco - CA, USA', got %q", payload.Text)
		}
	})

	t.Run("handles location with only address", func(t *testing.T) {
		session := makeTestSession()

		evt := &events.Message{
			Info: makeInfo("msg-005"),
			Message: &waE2E.Message{
				LocationMessage: &waE2E.LocationMessage{
					DegreesLatitude:  ptrF(0),
					DegreesLongitude: ptrF(0),
					Address:          ptr("Some Address"),
				},
			},
		}

		session.handleEvent(evt)

		msg := <-session.EventChan
		payload := msg.Payload.(MessagePayload)
		if payload.Text != "Some Address" {
			t.Errorf("expected text 'Some Address', got %q", payload.Text)
		}
	})

	t.Run("handles live location message", func(t *testing.T) {
		session := makeTestSession()

		evt := &events.Message{
			Info: makeInfo("msg-006"),
			Message: &waE2E.Message{
				LiveLocationMessage: &waE2E.LiveLocationMessage{
					DegreesLatitude:  ptrF(40.7128),
					DegreesLongitude: ptrF(-74.0060),
					Caption:          ptr("Live from NYC"),
				},
			},
		}

		session.handleEvent(evt)

		msg := <-session.EventChan
		payload := msg.Payload.(MessagePayload)
		if payload.MediaType != "live_location" {
			t.Errorf("expected media_type 'live_location', got %q", payload.MediaType)
		}
		if payload.Caption != "Live from NYC" {
			t.Errorf("expected caption 'Live from NYC', got %q", payload.Caption)
		}
	})

	t.Run("handles contact message", func(t *testing.T) {
		session := makeTestSession()

		evt := &events.Message{
			Info: makeInfo("msg-007"),
			Message: &waE2E.Message{
				ContactMessage: &waE2E.ContactMessage{
					DisplayName: ptr("Jane Doe"),
					Vcard:       ptr("BEGIN:VCARD\nVERSION:3.0\nFN:Jane Doe\nEND:VCARD"),
				},
			},
		}

		session.handleEvent(evt)

		msg := <-session.EventChan
		payload := msg.Payload.(MessagePayload)
		if payload.MediaType != "contact" {
			t.Errorf("expected media_type 'contact', got %q", payload.MediaType)
		}
		if payload.ContactName != "Jane Doe" {
			t.Errorf("expected contact_name 'Jane Doe', got %q", payload.ContactName)
		}
	})

	t.Run("handles contacts array message", func(t *testing.T) {
		session := makeTestSession()

		evt := &events.Message{
			Info: makeInfo("msg-008"),
			Message: &waE2E.Message{
				ContactsArrayMessage: &waE2E.ContactsArrayMessage{
					Contacts: []*waE2E.ContactMessage{
						{DisplayName: ptr("Contact 1"), Vcard: ptr("vcard1")},
						{DisplayName: ptr("Contact 2"), Vcard: ptr("vcard2")},
					},
				},
			},
		}

		session.handleEvent(evt)

		// Should receive 2 messages
		msg1 := <-session.EventChan
		payload1 := msg1.Payload.(MessagePayload)
		if payload1.ContactName != "Contact 1" {
			t.Errorf("expected 'Contact 1', got %q", payload1.ContactName)
		}

		msg2 := <-session.EventChan
		payload2 := msg2.Payload.(MessagePayload)
		if payload2.ContactName != "Contact 2" {
			t.Errorf("expected 'Contact 2', got %q", payload2.ContactName)
		}
	})

	t.Run("ignores empty messages", func(t *testing.T) {
		session := makeTestSession()

		evt := &events.Message{
			Info:    makeInfo("msg-009"),
			Message: &waE2E.Message{}, // Empty message
		}

		session.handleEvent(evt)

		select {
		case <-session.EventChan:
			t.Fatal("should not receive event for empty message")
		default:
			// Expected
		}
	})

	t.Run("ignores non-Message events", func(t *testing.T) {
		session := makeTestSession()

		// Pass a different event type
		session.handleEvent("some string event")

		select {
		case <-session.EventChan:
			t.Fatal("should not receive event for non-Message type")
		default:
			// Expected
		}
	})

	t.Run("drops message when channel full", func(t *testing.T) {
		session := makeTestSession()
		session.EventChan = make(chan MessageEvent, 1) // Very small buffer

		// Fill the channel
		session.EventChan <- MessageEvent{Type: "filler"}

		evt := &events.Message{
			Info: makeInfo("msg-drop"),
			Message: &waE2E.Message{
				Conversation: ptr("This should be dropped"),
			},
		}

		// Should not block
		session.handleEvent(evt)

		// Channel should still only have the filler
		if len(session.EventChan) != 1 {
			t.Errorf("expected 1 message in channel, got %d", len(session.EventChan))
		}
	})
}

// ==================== Mock Client Tests ====================

func TestMockClient(t *testing.T) {
	t.Run("NewMockClient creates disconnected client", func(t *testing.T) {
		m := NewMockClient()
		if m.IsConnected() {
			t.Error("expected disconnected")
		}
		if m.IsLoggedIn() {
			t.Error("expected not logged in")
		}
	})

	t.Run("NewConnectedMockClient creates connected client", func(t *testing.T) {
		m := NewConnectedMockClient()
		if !m.IsConnected() {
			t.Error("expected connected")
		}
		if m.IsLoggedIn() {
			t.Error("expected not logged in")
		}
	})

	t.Run("NewLoggedInMockClient creates fully connected client", func(t *testing.T) {
		m := NewLoggedInMockClient()
		if !m.IsConnected() {
			t.Error("expected connected")
		}
		if !m.IsLoggedIn() {
			t.Error("expected logged in")
		}
		if m.GetStore().GetID() == nil {
			t.Error("expected non-nil device ID")
		}
	})

	t.Run("Connect sets connected state", func(t *testing.T) {
		m := NewMockClient()
		if err := m.Connect(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !m.IsConnected() {
			t.Error("expected connected after Connect()")
		}
	})

	t.Run("Disconnect sets disconnected state", func(t *testing.T) {
		m := NewLoggedInMockClient()
		m.Disconnect()
		if m.IsConnected() {
			t.Error("expected disconnected after Disconnect()")
		}
	})

	t.Run("Call tracking works", func(t *testing.T) {
		m := NewMockClient()
		m.IsConnected()
		m.IsLoggedIn()
		m.Connect()

		calls := m.GetCalls()
		if len(calls) != 3 {
			t.Errorf("expected 3 calls, got %d", len(calls))
		}

		connectCalls := m.GetCallsByMethod("Connect")
		if len(connectCalls) != 1 {
			t.Errorf("expected 1 Connect call, got %d", len(connectCalls))
		}
	})
}

func TestConfigureDirectChatClient(t *testing.T) {
	client := &whatsmeow.Client{DisableManualHistorySyncReceipt: true}

	configureDirectChatClient(client)

	if !client.ManualHistorySyncDownload {
		t.Fatal("expected automatic history downloads to be disabled")
	}
	if client.DisableManualHistorySyncReceipt {
		t.Fatal("expected history receipts to remain enabled")
	}
}

func TestIsDirectChatJID(t *testing.T) {
	tests := []struct {
		name    string
		jid     types.JID
		allowed bool
	}{
		{name: "standard user", jid: types.JID{User: "15550000001", Server: types.DefaultUserServer}, allowed: true},
		{name: "hidden user", jid: types.JID{User: "15550000002", Server: types.HiddenUserServer}, allowed: true},
		{name: "legacy user", jid: types.JID{User: "15550000003", Server: types.LegacyUserServer}, allowed: true},
		{name: "group", jid: types.JID{User: "100000", Server: types.GroupServer}},
		{name: "broadcast", jid: types.JID{User: "status", Server: types.BroadcastServer}},
		{name: "newsletter", jid: types.JID{User: "100000", Server: types.NewsletterServer}},
		{name: "bot", jid: types.JID{User: "100000", Server: types.BotServer}},
		{name: "messenger", jid: types.JID{User: "100000", Server: types.MessengerServer}},
		{name: "interop", jid: types.JID{User: "100000", Server: types.InteropServer}},
		{name: "hosted", jid: types.JID{User: "100000", Server: types.HostedServer}},
		{name: "hosted lid", jid: types.JID{User: "100000", Server: types.HostedLIDServer}},
		{name: "empty user", jid: types.JID{Server: types.DefaultUserServer}},
		{name: "device address", jid: types.JID{User: "15550000004", Server: types.DefaultUserServer, Device: 1}},
		{name: "agent address", jid: types.JID{User: "15550000005", Server: types.DefaultUserServer, RawAgent: 1}},
		{name: "integrator address", jid: types.JID{User: "15550000006", Server: types.DefaultUserServer, Integrator: 1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDirectChatJID(tt.jid); got != tt.allowed {
				t.Fatalf("isDirectChatJID() = %v, want %v", got, tt.allowed)
			}
		})
	}
}

func TestParseDirectChatJID(t *testing.T) {
	for _, raw := range []string{
		"15550000001@s.whatsapp.net",
		"15550000002@lid",
		"15550000003@c.us",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseDirectChatJID(raw); err != nil {
				t.Fatalf("expected direct JID to pass: %v", err)
			}
		})
	}

	for _, raw := range []string{
		"not-a-jid",
		"100000@g.us",
		"status@broadcast",
		"100000@newsletter",
		"100000@bot",
		"100000@hosted",
		"100000@hosted.lid",
		"100000@interop",
		"100000@msgr",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseDirectChatJID(raw); err == nil {
				t.Fatal("expected non-direct JID to be rejected")
			}
		})
	}
}

func TestRegisterHandlersDirectChatSurface(t *testing.T) {
	mux := http.NewServeMux()
	registerHandlers(mux)

	for _, path := range []string{"/chats", "/groups/info", "/groups/participants"} {
		t.Run("disabled_"+strings.ReplaceAll(strings.TrimPrefix(path, "/"), "/", "_"), func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != http.StatusNotFound {
				t.Fatalf("%s returned %d, want 404", path, w.Code)
			}
		})
	}

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("health returned %d, want 200", w.Code)
	}
}

func TestHandleEventDropsNonDirectChats(t *testing.T) {
	for _, server := range []string{
		types.GroupServer,
		types.BroadcastServer,
		types.NewsletterServer,
		types.BotServer,
		types.MessengerServer,
		types.InteropServer,
		types.HostedServer,
		types.HostedLIDServer,
	} {
		t.Run(server, func(t *testing.T) {
			client := NewLoggedInMockClient()
			session := &UserSession{
				Client:    client,
				EventChan: make(chan MessageEvent, 1),
			}
			session.handleEvent(&events.Message{
				Info: types.MessageInfo{MessageSource: types.MessageSource{
					Chat:   types.JID{User: "100000", Server: server},
					Sender: types.JID{User: "15550000001", Server: types.DefaultUserServer},
				}},
				Message: &waE2E.Message{Conversation: proto.String("ignored")},
			})

			select {
			case <-session.EventChan:
				t.Fatal("non-direct message was published")
			default:
			}
			if len(client.GetCallsByMethod("Download")) != 0 {
				t.Fatal("non-direct message triggered media work")
			}
		})
	}
}

func TestOutboundHandlersRejectNonDirectChats(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		body    string
	}{
		{name: "text", handler: sendMessageHandler, body: `{"user_id":9001,"chat_jid":"100000@g.us","text":"hello"}`},
		{name: "reaction", handler: sendReactionHandler, body: `{"user_id":9001,"chat_jid":"100000@g.us","message_id":"msg-1","emoji":"x"}`},
		{name: "typing", handler: setTypingHandler, body: `{"user_id":9001,"chat_jid":"100000@g.us","typing":true}`},
		{name: "image", handler: sendImageHandler, body: `{"user_id":9001,"chat_jid":"100000@g.us","image_b64":"aGVsbG8=","mime_type":"image/png"}`},
		{name: "audio", handler: sendAudioHandler, body: `{"user_id":9001,"chat_jid":"100000@g.us","audio_b64":"aGVsbG8=","mime_type":"audio/ogg","ptt":true}`},
		{name: "document", handler: sendDocumentHandler, body: `{"user_id":9001,"chat_jid":"100000@g.us","doc_b64":"aGVsbG8=","mime_type":"text/plain","filename":"test.txt"}`},
		{name: "location", handler: sendLocationHandler, body: `{"user_id":9001,"chat_jid":"100000@g.us","latitude":1,"longitude":2}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := setupTestManager(t)
			client := NewLoggedInMockClient()
			injectMockSession(m, 9001, client)
			previousManager := manager
			manager = m
			t.Cleanup(func() { manager = previousManager })

			w := httptest.NewRecorder()
			tt.handler(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body)))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("returned %d, want 400: %s", w.Code, w.Body.String())
			}
			for _, method := range []string{"SendMessage", "SendChatPresence", "Upload"} {
				if calls := client.GetCallsByMethod(method); len(calls) != 0 {
					t.Fatalf("non-direct request called %s", method)
				}
			}
		})
	}
}
