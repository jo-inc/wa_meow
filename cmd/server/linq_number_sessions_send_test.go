package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

const (
	testAdminToken = "local-admin-test-token-long-enough-for-validation"
	testSendBody   = `{"recipient":"+15550001111","text":"test message","delivery_key":"test-delivery-1"}`
)

func setupLinqSendManager(t *testing.T, phone string) (*SessionManager, *MockWhatsAppClient) {
	t.Helper()
	manager = setupTestManager(t)
	manager.adminToken = testAdminToken
	client := NewLoggedInMockClient()
	client.store.ID = &types.JID{User: "15550001111", Server: types.DefaultUserServer}
	manager.numberSessions[2] = &numberSessionState{
		linqNumberRecord: linqNumberRecord{ID: 2, Phone: phone},
		session: &UserSession{
			Kind:         LinqNumberSessionKind,
			LinqNumberID: 2,
			Client:       client,
			DBPath:       filepath.Join(manager.dataDir, "linq_number_2.db"),
		},
	}
	return manager, client
}

func countMockCalls(calls []MockCall, method string) int {
	count := 0
	for _, call := range calls {
		if call.Method == method {
			count++
		}
	}
	return count
}

func numberSendRequest(body, authorization string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/admin/linq-numbers/2/messages/send", strings.NewReader(body))
	request.Header.Set("Authorization", authorization)
	return request
}

func TestLinqNumberTextSendRequiresAdminAndVerifiedNumber(t *testing.T) {
	m, client := setupLinqSendManager(t, testPairingPhone)

	unauthorized := httptest.NewRecorder()
	linqNumberAdminHandler(unauthorized, numberSendRequest(testSendBody, ""))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized send status = %d, want 401", unauthorized.Code)
	}
	if calls := client.GetCalls(); countMockCalls(calls, "SendMessage") != 0 {
		t.Fatal("unauthorized request sent a message")
	}

	client.store.ID.User = "15550002222"
	m.numberSessions[2].Phone = testPairingPhone
	mismatch := httptest.NewRecorder()
	linqNumberAdminHandler(mismatch, numberSendRequest(testSendBody, "Bearer "+testAdminToken))
	if mismatch.Code != http.StatusConflict {
		t.Fatalf("mismatched number status = %d, want 409", mismatch.Code)
	}
	if calls := client.GetCalls(); countMockCalls(calls, "SendMessage") != 0 {
		t.Fatal("mismatched paired number sent a message")
	}
}

func TestLinqNumberTextSendUsesNumberNamespaceAndDeduplicates(t *testing.T) {
	m, client := setupLinqSendManager(t, testPairingPhone)
	personal := injectMockSession(m, 2, NewLoggedInMockClient())

	request := numberSendRequest(testSendBody, "Bearer "+testAdminToken)
	response := httptest.NewRecorder()
	linqNumberAdminHandler(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("send status = %d, body=%s", response.Code, response.Body.String())
	}
	var first map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first["deduplicated"] != false || first["id"] != "mock-msg-id" {
		t.Fatalf("unexpected first send response: %#v", first)
	}

	calls := client.GetCalls()
	if countMockCalls(calls, "SendMessage") != 1 {
		t.Fatalf("SendMessage calls = %d, want 1", countMockCalls(calls, "SendMessage"))
	}
	for _, call := range calls {
		if call.Method != "SendMessage" {
			continue
		}
		jid := call.Args[1].(types.JID)
		message := call.Args[2].(*waE2E.Message)
		if jid.User != "15550001111" || jid.Server != types.DefaultUserServer || message.GetConversation() != "test message" {
			t.Fatalf("unexpected send target or content: jid=%v message=%v", jid, message.GetConversation())
		}
	}

	response = httptest.NewRecorder()
	linqNumberAdminHandler(response, numberSendRequest(testSendBody, "Bearer "+testAdminToken))
	if response.Code != http.StatusOK {
		t.Fatalf("duplicate send status = %d, body=%s", response.Code, response.Body.String())
	}
	var duplicate map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &duplicate); err != nil {
		t.Fatal(err)
	}
	if duplicate["deduplicated"] != true || countMockCalls(client.GetCalls(), "SendMessage") != 1 {
		t.Fatalf("duplicate request was not deduplicated: %#v", duplicate)
	}
	if m.sessions[2] != personal {
		t.Fatal("number-scoped send changed the personal user session")
	}
	path := linqNumberDeliveryReceiptPath(m, 2, "test-delivery-1")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("receipt permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestLinqNumberTextSendReconnectsPersistedDeviceBeforeSending(t *testing.T) {
	_, client := setupLinqSendManager(t, testPairingPhone)
	client.SetConnected(false)
	client.SetLoggedIn(false)
	go func() {
		time.Sleep(5 * time.Millisecond)
		client.SetLoggedIn(true)
	}()
	response := httptest.NewRecorder()
	linqNumberAdminHandler(response, numberSendRequest(testSendBody, "Bearer "+testAdminToken))
	if response.Code != http.StatusOK {
		t.Fatalf("send after reconnect status = %d, body=%s", response.Code, response.Body.String())
	}
	if countMockCalls(client.GetCalls(), "Connect") != 1 || countMockCalls(client.GetCalls(), "SendMessage") != 1 {
		t.Fatalf("expected one reconnect and one send, calls=%#v", client.GetCalls())
	}
}

func TestLinqNumberTextSendRejectsInvalidRecipientAndBody(t *testing.T) {
	_, client := setupLinqSendManager(t, testPairingPhone)
	for _, body := range []string{
		`{"recipient":"5550001111","text":"test","delivery_key":"key"}`,
		`{"recipient":"+15550001111","text":" ","delivery_key":"key"}`,
		`{"recipient":"+15550001111","text":"test"}`,
		`{"recipient":"+15550001111","text":"test","delivery_key":"key","user_id":2}`,
	} {
		response := httptest.NewRecorder()
		linqNumberAdminHandler(response, numberSendRequest(body, "Bearer "+testAdminToken))
		if response.Code != http.StatusBadRequest {
			t.Errorf("invalid request status = %d, want 400", response.Code)
		}
	}
	if countMockCalls(client.GetCalls(), "SendMessage") != 0 {
		t.Fatal("invalid request sent a message")
	}
}
