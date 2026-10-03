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
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const testPairingPhone = "+15550001111"

func TestNormalizeE164(t *testing.T) {
	for _, input := range []string{testPairingPhone, "+1 (555) 000-1111"} {
		got, ok := normalizeE164(input)
		if !ok || got != testPairingPhone {
			t.Errorf("normalizeE164(%q) = %q, %v", input, got, ok)
		}
	}
	for _, input := range []string{"15550001111", "+123", "+1555000111x", "+1234567890123456"} {
		if _, ok := normalizeE164(input); ok {
			t.Errorf("normalizeE164(%q) unexpectedly succeeded", input)
		}
	}
}

func TestLinqNumberRegistrationIsSeparateAndPersistent(t *testing.T) {
	m := setupTestManager(t)
	personal := injectMockSession(m, 2, NewLoggedInMockClient())
	m.numberSessions = make(map[int]*numberSessionState)

	firstToken, err := m.registerLinqNumber(linqNumberRecord{ID: 2, Phone: testPairingPhone})
	if err != nil {
		t.Fatal(err)
	}
	secondToken, err := m.registerLinqNumber(linqNumberRecord{ID: 2, Phone: "+1 (555) 000-1111"})
	if err != nil {
		t.Fatal(err)
	}
	if firstToken == secondToken {
		t.Fatal("registration must issue a fresh temporary pairing capability")
	}
	if m.sessions[2] != personal {
		t.Fatal("number registration modified the personal user session")
	}
	if m.numberSessions[2] == nil || m.numberSessions[2].session != nil {
		t.Fatal("number registration must occupy its typed namespace without creating a user session")
	}
	if _, err := m.registerLinqNumber(linqNumberRecord{ID: 2, Phone: "+15550002222"}); err == nil {
		t.Fatal("registered number phone was unexpectedly changed")
	}
	if _, err := m.registerLinqNumber(linqNumberRecord{ID: 3, Phone: testPairingPhone}); err == nil {
		t.Fatal("same phone was unexpectedly registered under multiple number IDs")
	}

	numberState, err := m.getOrCreateLinqNumberSession(2)
	if err != nil {
		t.Fatal(err)
	}
	if numberState.session.Kind != LinqNumberSessionKind || numberState.session.LinqNumberID != 2 || numberState.session.UserID != 0 {
		t.Fatalf("unexpected typed number-session identity: %#v", numberState.session)
	}
	if numberState.session.DBPath != filepath.Join(m.dataDir, "linq_number_2.db") || m.sessions[2] != personal {
		t.Fatal("number session collided with the personal-user session or database")
	}
	dbInfo, err := os.Stat(numberState.session.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if dbInfo.Mode().Perm() != 0600 {
		t.Fatalf("number session permissions = %o, want 600", dbInfo.Mode().Perm())
	}
	numberState.session.close(false)
	activeSessions.Dec()

	path := filepath.Join(m.dataDir, linqNumberRegistryFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("registry permissions = %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "user_id") || !strings.Contains(string(data), testPairingPhone) {
		t.Fatalf("registry did not contain only number-scoped registration data")
	}

	reloaded := NewSessionManager(m.dataDir, "", "")
	if reloaded.numberRegistryErr != nil {
		t.Fatal(reloaded.numberRegistryErr)
	}
	if got := reloaded.numberSessions[2]; got == nil || got.Phone != testPairingPhone {
		t.Fatal("registered number was not restored from the local registry")
	}
}

func TestLinqNumberAdminRequiresTokenAndReturnsTemporaryPairingLink(t *testing.T) {
	manager = setupTestManager(t)
	manager.adminToken = "local-admin-test-token-long-enough-for-validation"
	body := `{"id":2,"phone":"` + testPairingPhone + `"}`

	unauthorized := httptest.NewRecorder()
	linqNumberAdminHandler(unauthorized, httptest.NewRequest(http.MethodPost, "/admin/linq-numbers", strings.NewReader(body)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401", unauthorized.Code)
	}
	if manager.numberSessions[2] != nil {
		t.Fatal("unauthorized registration changed state")
	}

	request := httptest.NewRequest(http.MethodPost, "/admin/linq-numbers", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer local-admin-test-token-long-enough-for-validation")
	response := httptest.NewRecorder()
	linqNumberAdminHandler(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("registration status = %d, body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Status string `json:"status"`
		URL    string `json:"pairing_url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "registered" || !strings.HasPrefix(result.URL, "/admin/linq-numbers/2/pair?token=") {
		t.Fatalf("unexpected registration response: %#v", result)
	}
	if strings.Contains(response.Body.String(), testPairingPhone) {
		t.Fatal("registration response exposed the configured phone")
	}

	pairRequest := httptest.NewRequest(http.MethodGet, result.URL, nil)
	pairResponse := httptest.NewRecorder()
	linqNumberAdminHandler(pairResponse, pairRequest)
	if pairResponse.Code != http.StatusOK || !strings.Contains(pairResponse.Body.String(), "Link Jo WhatsApp number") {
		t.Fatalf("pairing page response = %d", pairResponse.Code)
	}
	if pairResponse.Header().Get("Cache-Control") != "no-store" || pairResponse.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("pairing page is missing secret-link protections")
	}
	badRequest := httptest.NewRecorder()
	linqNumberAdminHandler(badRequest, httptest.NewRequest(http.MethodGet, "/admin/linq-numbers/2/pair?token=invalid", nil))
	if badRequest.Code != http.StatusUnauthorized {
		t.Fatalf("invalid pairing link status = %d, want 401", badRequest.Code)
	}
}

func TestLinqNumberRegistrationDisabledWithoutAdminSecret(t *testing.T) {
	manager = setupTestManager(t)
	response := httptest.NewRecorder()
	linqNumberAdminHandler(response, httptest.NewRequest(http.MethodPost, "/admin/linq-numbers", strings.NewReader(`{"id":2,"phone":"`+testPairingPhone+`"}`)))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
}

func TestLinqNumberPairingVerifiesExactPhone(t *testing.T) {
	manager = setupTestManager(t)
	manager.numberSessions[2] = &numberSessionState{linqNumberRecord: linqNumberRecord{ID: 2, Phone: testPairingPhone}}
	client := NewLoggedInMockClient()
	client.GetStore().(*MockDeviceStore).ID = &types.JID{User: "15550001111", Server: types.DefaultUserServer}
	manager.numberSessions[2].session = &UserSession{Kind: LinqNumberSessionKind, LinqNumberID: 2, Client: client}

	response := httptest.NewRecorder()
	statusLinqNumberPairing(response, manager.numberSessions[2])
	var result map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["verified"] != true || result["phone_matches"] != true {
		t.Fatalf("matching number not verified: %#v", result)
	}

	client.GetStore().(*MockDeviceStore).ID.User = "15550002222"
	response = httptest.NewRecorder()
	statusLinqNumberPairing(response, manager.numberSessions[2])
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["verified"] != false || result["phone_matches"] != false {
		t.Fatalf("mismatching account was verified: %#v", result)
	}
	if strings.Contains(response.Body.String(), "15550002222") || strings.Contains(response.Body.String(), testPairingPhone) {
		t.Fatal("pairing status exposed a phone number")
	}
}

func TestNumberScopedEventsUseTheirOwnSessionChannel(t *testing.T) {
	m := setupTestManager(t)
	personal := injectMockSession(m, 2, NewLoggedInMockClient())
	number := &UserSession{
		Kind: LinqNumberSessionKind, LinqNumberID: 2, EventChan: make(chan MessageEvent, 1),
		Inbox: &sharedInboxStore{manager: m},
	}
	if !number.handleNumberEvent(&events.Message{
		Info: types.MessageInfo{MessageSource: types.MessageSource{
			Chat:   types.JID{User: "15550001111", Server: types.DefaultUserServer},
			Sender: types.JID{User: "15550001111", Server: types.DefaultUserServer},
		}, ID: "number-message", Timestamp: time.Now()},
		Message: &waE2E.Message{Conversation: proto.String("test")},
	}) {
		t.Fatal("number event was not durably accepted")
	}
	select {
	case event := <-number.EventChan:
		if event.Payload.(MessagePayload).ID != "number-message" {
			t.Fatalf("unexpected number event: %#v", event)
		}
	default:
		t.Fatal("number event was not emitted to its own channel")
	}
	select {
	case event := <-personal.EventChan:
		t.Fatalf("number-scoped event leaked to personal stream: %#v", event)
	default:
	}
}

func TestPairingTokenExpires(t *testing.T) {
	m := setupTestManager(t)
	token, err := m.registerLinqNumber(linqNumberRecord{ID: 2, Phone: testPairingPhone})
	if err != nil {
		t.Fatal(err)
	}
	state := m.numberSessions[2]
	state.tokenExpiresAt = time.Now().Add(-time.Second)
	if m.authorizePairingToken(2, token) != nil {
		t.Fatal("expired pairing token was accepted")
	}
}
