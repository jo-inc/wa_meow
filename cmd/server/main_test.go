package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Test helper: create a session manager with a mock client injected
func setupTestManager(t *testing.T) *SessionManager {
	t.Helper()
	return NewSessionManager(t.TempDir(), "", "")
}

// Test helper: inject a mock session into the manager
func injectMockSession(m *SessionManager, userID int, client *MockWhatsAppClient) *UserSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := &UserSession{
		UserID:    userID,
		Client:    client,
		DBPath:    "",
		LastUsed:  time.Now(),
		QRChannel: make(chan string, 10),
		LoginDone: make(chan bool, 1),
		EventChan: make(chan MessageEvent, 100),
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

	t.Run("removes and disconnects session", func(t *testing.T) {
		mock := NewLoggedInMockClient()
		injectMockSession(m, 200, mock)

		m.RemoveSession(200)

		if m.GetSession(200) != nil {
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

		// Verify Disconnect was called
		calls := mock.GetCallsByMethod("Disconnect")
		if len(calls) == 0 {
			t.Error("expected Disconnect to be called")
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

func TestGetChatsHandler(t *testing.T) {
	t.Run("requires user_id parameter", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/chats", nil)
		w := httptest.NewRecorder()
		getChatsHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("returns 404 for unknown session", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/chats?user_id=99999", nil)
		w := httptest.NewRecorder()
		getChatsHandler(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})

	t.Run("returns 400 when not logged in", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewConnectedMockClient()
		injectMockSession(manager, 1100, mock)

		req := httptest.NewRequest(http.MethodGet, "/chats?user_id=1100", nil)
		w := httptest.NewRecorder()
		getChatsHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("returns chats successfully", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		mock.JoinedGroups = []*types.GroupInfo{
			{JID: types.JID{User: "group1", Server: types.GroupServer}, GroupName: types.GroupName{Name: "Test Group"}},
		}
		mock.SetContacts(map[types.JID]types.ContactInfo{
			{User: "123", Server: types.DefaultUserServer}: {PushName: "John Doe"},
		})
		injectMockSession(manager, 1101, mock)

		req := httptest.NewRequest(http.MethodGet, "/chats?user_id=1101", nil)
		w := httptest.NewRecorder()
		getChatsHandler(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		var chats []ChatPayload
		json.NewDecoder(w.Body).Decode(&chats)
		if len(chats) != 2 {
			t.Errorf("expected 2 chats (1 group + 1 contact), got %d", len(chats))
		}
	})

	t.Run("returns contacts with fallback names", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		// No groups
		mock.JoinedGroups = nil
		// Contact with FullName but no PushName
		mock.SetContacts(map[types.JID]types.ContactInfo{
			{User: "456", Server: types.DefaultUserServer}: {FullName: "Jane Smith"},
			{User: "789", Server: types.DefaultUserServer}: {}, // No name at all
		})
		injectMockSession(manager, 1102, mock)

		req := httptest.NewRequest(http.MethodGet, "/chats?user_id=1102", nil)
		w := httptest.NewRecorder()
		getChatsHandler(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		var chats []ChatPayload
		json.NewDecoder(w.Body).Decode(&chats)
		if len(chats) != 2 {
			t.Errorf("expected 2 chats, got %d", len(chats))
		}
	})

	t.Run("handles groups and contacts errors gracefully", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		mock.JoinedGroupsError = errors.New("groups failed")
		mock.store.Contacts.ContactsError = errors.New("contacts failed")
		injectMockSession(manager, 1103, mock)

		req := httptest.NewRequest(http.MethodGet, "/chats?user_id=1103", nil)
		w := httptest.NewRecorder()
		getChatsHandler(w, req)

		// Should still return 200, just empty chats
		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		var chats []ChatPayload
		json.NewDecoder(w.Body).Decode(&chats)
		if chats == nil {
			chats = []ChatPayload{}
		}
		// Should return empty or nil chats when both fail
	})
}

// ==================== Group Handler Tests ====================

func TestGetGroupInfoHandler(t *testing.T) {
	t.Run("requires user_id parameter", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/groups/info", nil)
		w := httptest.NewRecorder()
		getGroupInfoHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("requires group_jid parameter", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/groups/info?user_id=1", nil)
		w := httptest.NewRecorder()
		getGroupInfoHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("returns 404 for unknown session", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/groups/info?user_id=99999&group_jid=group@g.us", nil)
		w := httptest.NewRecorder()
		getGroupInfoHandler(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})

	t.Run("returns 400 when not logged in", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewConnectedMockClient()
		injectMockSession(manager, 1199, mock)

		req := httptest.NewRequest(http.MethodGet, "/groups/info?user_id=1199&group_jid=group@g.us", nil)
		w := httptest.NewRecorder()
		getGroupInfoHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("returns group info successfully", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		mock.GroupInfo = &types.GroupInfo{
			JID:       types.JID{User: "group123", Server: types.GroupServer},
			GroupName: types.GroupName{Name: "My Group"},
			GroupTopic: types.GroupTopic{Topic: "Group topic"},
			OwnerJID:  types.JID{User: "owner", Server: types.DefaultUserServer},
			Participants: []types.GroupParticipant{
				{JID: types.JID{User: "user1", Server: types.DefaultUserServer}, IsAdmin: true},
				{JID: types.JID{User: "user2", Server: types.DefaultUserServer}, IsAdmin: false},
			},
		}
		injectMockSession(manager, 1200, mock)

		req := httptest.NewRequest(http.MethodGet, "/groups/info?user_id=1200&group_jid=group123@g.us", nil)
		w := httptest.NewRecorder()
		getGroupInfoHandler(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		var info GroupInfoPayload
		json.NewDecoder(w.Body).Decode(&info)
		if info.Name != "My Group" {
			t.Errorf("expected name 'My Group', got %q", info.Name)
		}
		if len(info.Participants) != 2 {
			t.Errorf("expected 2 participants, got %d", len(info.Participants))
		}
	})

	t.Run("handles GetGroupInfo error", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		mock.GroupInfoError = errors.New("group not found")
		injectMockSession(manager, 1201, mock)

		req := httptest.NewRequest(http.MethodGet, "/groups/info?user_id=1201&group_jid=group123@g.us", nil)
		w := httptest.NewRecorder()
		getGroupInfoHandler(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", w.Code)
		}
	})
}

func TestListGroupParticipantsHandler(t *testing.T) {
	t.Run("requires user_id parameter", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/groups/participants", nil)
		w := httptest.NewRecorder()
		listGroupParticipantsHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("requires group_jid parameter", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/groups/participants?user_id=1", nil)
		w := httptest.NewRecorder()
		listGroupParticipantsHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("returns 404 for unknown session", func(t *testing.T) {
		manager = setupTestManager(t)
		req := httptest.NewRequest(http.MethodGet, "/groups/participants?user_id=99999&group_jid=group@g.us", nil)
		w := httptest.NewRecorder()
		listGroupParticipantsHandler(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})

	t.Run("returns 400 when not logged in", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewConnectedMockClient()
		injectMockSession(manager, 1300, mock)

		req := httptest.NewRequest(http.MethodGet, "/groups/participants?user_id=1300&group_jid=group@g.us", nil)
		w := httptest.NewRecorder()
		listGroupParticipantsHandler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("returns participants successfully", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		mock.GroupInfo = &types.GroupInfo{
			Participants: []types.GroupParticipant{
				{JID: types.JID{User: "admin", Server: types.DefaultUserServer}, IsAdmin: true, IsSuperAdmin: true},
				{JID: types.JID{User: "member", Server: types.DefaultUserServer}, IsAdmin: false},
			},
		}
		injectMockSession(manager, 1301, mock)

		req := httptest.NewRequest(http.MethodGet, "/groups/participants?user_id=1301&group_jid=group@g.us", nil)
		w := httptest.NewRecorder()
		listGroupParticipantsHandler(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		var participants []ParticipantInfo
		json.NewDecoder(w.Body).Decode(&participants)
		if len(participants) != 2 {
			t.Errorf("expected 2 participants, got %d", len(participants))
		}
	})

	t.Run("handles GetGroupInfo error", func(t *testing.T) {
		manager = setupTestManager(t)
		mock := NewLoggedInMockClient()
		mock.GroupInfoError = errors.New("group not found")
		injectMockSession(manager, 1302, mock)

		req := httptest.NewRequest(http.MethodGet, "/groups/participants?user_id=1302&group_jid=group@g.us", nil)
		w := httptest.NewRecorder()
		listGroupParticipantsHandler(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", w.Code)
		}
	})
}

// ==================== Media Handler Tests ====================

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

	t.Run("handles text message with Conversation", func(t *testing.T) {
		session := &UserSession{
			UserID:    1,
			EventChan: make(chan MessageEvent, 10),
		}

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
		session := &UserSession{
			UserID:    1,
			EventChan: make(chan MessageEvent, 10),
		}

		evt := &events.Message{
			Info:    makeInfo("msg-002"),
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
		session := &UserSession{
			UserID:    1,
			EventChan: make(chan MessageEvent, 10),
		}

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
		session := &UserSession{
			UserID:    1,
			EventChan: make(chan MessageEvent, 10),
		}

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
		session := &UserSession{
			UserID:    1,
			EventChan: make(chan MessageEvent, 10),
		}

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
		session := &UserSession{
			UserID:    1,
			EventChan: make(chan MessageEvent, 10),
		}

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
		session := &UserSession{
			UserID:    1,
			EventChan: make(chan MessageEvent, 10),
		}

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
		session := &UserSession{
			UserID:    1,
			EventChan: make(chan MessageEvent, 10),
		}

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
		session := &UserSession{
			UserID:    1,
			EventChan: make(chan MessageEvent, 10),
		}

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
		session := &UserSession{
			UserID:    1,
			EventChan: make(chan MessageEvent, 10),
		}

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
		session := &UserSession{
			UserID:    1,
			EventChan: make(chan MessageEvent, 1), // Very small buffer
		}

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
