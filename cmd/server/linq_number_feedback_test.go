package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

const (
	feedbackTestNumberID = 42
	feedbackTestPhone    = "+15550001111"
)

func setupVerifiedFeedbackNumber(t *testing.T) (*SessionManager, *MockWhatsAppClient, *MockWhatsAppClient) {
	t.Helper()
	m := setupTestManager(t)
	personal := NewLoggedInMockClient()
	injectMockSession(m, feedbackTestNumberID, personal)

	number := NewLoggedInMockClient()
	number.SetDeviceID(&types.JID{User: "15550001111", Server: types.DefaultUserServer})
	m.numberSessions[feedbackTestNumberID] = &numberSessionState{
		linqNumberRecord: linqNumberRecord{ID: feedbackTestNumberID, Phone: feedbackTestPhone},
		session: &UserSession{
			Kind:         LinqNumberSessionKind,
			LinqNumberID: feedbackTestNumberID,
			Client:       number,
		},
	}
	manager = m
	return m, number, personal
}

func invokeAuthenticatedFeedback(t *testing.T, handler func(http.ResponseWriter, *http.Request, int), body string, authorized bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/linq-numbers/42/messages/feedback", strings.NewReader(body))
	if authorized {
		req.Header.Set("X-WhatsApp-Internal-Token", manager.joBotInternalToken)
	}
	response := httptest.NewRecorder()
	if requireInternal(response, req) {
		handler(response, req, feedbackTestNumberID)
	}
	return response
}

func TestInternalLinqNumberFeedbackRequiresInternalAuth(t *testing.T) {
	_, number, personal := setupVerifiedFeedbackNumber(t)
	manager.joBotInternalToken = "internal-feedback-test-token"

	response := invokeAuthenticatedFeedback(t, sendInternalLinqNumberTypingHandler, `{"chat_jid":"15550002222@s.whatsapp.net","typing":true}`, false)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
	if len(number.GetCallsByMethod("SendChatPresence")) != 0 || len(personal.GetCallsByMethod("SendChatPresence")) != 0 {
		t.Fatal("unauthenticated request reached a transport")
	}
}

func TestInternalLinqNumberFeedbackRoutes(t *testing.T) {
	for _, item := range []struct{ path, body string }{
		{"typing", `{"chat_jid":"15550002222@s.whatsapp.net","typing":true}`},
		{"react", `{"chat_jid":"15550002222@s.whatsapp.net","message_id":"synthetic-inbound","emoji":"👍"}`},
	} {
		t.Run(item.path, func(t *testing.T) {
			setupVerifiedFeedbackNumber(t)
			manager.joBotInternalToken = "synthetic-feedback-routing-token"
			path := "/internal/linq-numbers/42/messages/" + item.path
			for _, token := range []string{"", manager.joBotInternalToken} {
				response := httptest.NewRecorder()
				linqNumberInternalHandler(response, internalNumberRequest(http.MethodPost, path, item.body, token))
				want := http.StatusOK
				if token == "" {
					want = http.StatusUnauthorized
				}
				if response.Code != want {
					t.Fatalf("route status=%d, want=%d", response.Code, want)
				}
			}
		})
	}
}

func TestInternalLinqNumberTypingUsesVerifiedNumberOnly(t *testing.T) {
	_, number, personal := setupVerifiedFeedbackNumber(t)

	for _, test := range []struct {
		name   string
		body   string
		status int
	}{
		{name: "composing", body: `{"chat_jid":"15550002222@s.whatsapp.net","typing":true}`, status: http.StatusOK},
		{name: "paused", body: `{"chat_jid":"15550002222@s.whatsapp.net","typing":false}`, status: http.StatusOK},
		{name: "group rejected", body: `{"chat_jid":"12345@g.us","typing":true}`, status: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			sendInternalLinqNumberTypingHandler(response, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.body)), feedbackTestNumberID)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
		})
	}
	calls := number.GetCallsByMethod("SendChatPresence")
	if len(calls) != 2 {
		t.Fatalf("number presence calls = %d, want 2", len(calls))
	}
	if got := calls[0].Args[2].(types.ChatPresence); got != types.ChatPresenceComposing {
		t.Fatalf("first presence = %v, want composing", got)
	}
	if got := calls[1].Args[2].(types.ChatPresence); got != types.ChatPresencePaused {
		t.Fatalf("second presence = %v, want paused", got)
	}
	if len(personal.GetCallsByMethod("SendChatPresence")) != 0 {
		t.Fatal("typing used the personal session")
	}
}

func TestInternalLinqNumberFeedbackRejectsDisconnectedOrUnverifiedSession(t *testing.T) {
	m, number, _ := setupVerifiedFeedbackNumber(t)
	number.SetConnected(false)
	response := httptest.NewRecorder()
	sendInternalLinqNumberTypingHandler(response, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"chat_jid":"15550002222@s.whatsapp.net","typing":true}`)), feedbackTestNumberID)
	if response.Code != http.StatusConflict {
		t.Fatalf("disconnected status = %d, want 409", response.Code)
	}
	number.SetConnected(true)
	number.SetDeviceID(&types.JID{User: "15550009999", Server: types.DefaultUserServer})
	response = httptest.NewRecorder()
	sendInternalLinqNumberReactionHandler(response, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"chat_jid":"15550002222@s.whatsapp.net","message_id":"inbound-1","emoji":"👍"}`)), feedbackTestNumberID)
	if response.Code != http.StatusConflict {
		t.Fatalf("unverified status = %d, want 409", response.Code)
	}
	if len(number.GetCallsByMethod("SendMessage")) != 0 || m.sessions[feedbackTestNumberID] == nil {
		t.Fatal("unverified feedback used a transport or altered the personal session")
	}
}

func TestInternalLinqNumberReactionTargetsInboundMessageAndAcceptsCleanup(t *testing.T) {
	_, number, personal := setupVerifiedFeedbackNumber(t)

	missingID := httptest.NewRecorder()
	sendInternalLinqNumberReactionHandler(missingID, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"chat_jid":"15550002222@s.whatsapp.net","emoji":"👍"}`)), feedbackTestNumberID)
	if missingID.Code != http.StatusBadRequest {
		t.Fatalf("missing message id status = %d, want 400", missingID.Code)
	}

	for _, body := range []string{
		`{"chat_jid":"15550002222@s.whatsapp.net","message_id":"inbound-1","emoji":"👍"}`,
		`{"chat_jid":"15550002222@s.whatsapp.net","message_id":"inbound-1","emoji":""}`,
	} {
		response := httptest.NewRecorder()
		sendInternalLinqNumberReactionHandler(response, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), feedbackTestNumberID)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
		}
	}
	calls := number.GetCallsByMethod("SendMessage")
	if len(calls) != 2 {
		t.Fatalf("number reaction calls = %d, want 2", len(calls))
	}
	for i, call := range calls {
		reaction := call.Args[2].(*waE2E.Message).GetReactionMessage()
		if reaction.GetKey().GetFromMe() {
			t.Fatalf("reaction %d targets an outbound message, want inbound", i)
		}
		if reaction.GetKey().GetRemoteJID() != "15550002222@s.whatsapp.net" || reaction.GetKey().GetID() != "inbound-1" {
			t.Fatalf("reaction %d target = %#v", i, reaction.GetKey())
		}
	}
	if len(personal.GetCallsByMethod("SendMessage")) != 0 {
		t.Fatal("reaction used the personal session")
	}
}

func TestInternalLinqNumberReactionFailuresAreSanitizedAndNonSuccess(t *testing.T) {
	_, number, _ := setupVerifiedFeedbackNumber(t)
	number.SendMessageError = errors.New("provider leaked: 15550002222 secret")
	response := httptest.NewRecorder()
	sendInternalLinqNumberReactionHandler(response, httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"chat_jid":"15550002222@s.whatsapp.net","message_id":"inbound-1","emoji":"👍"}`)), feedbackTestNumberID)
	if response.Code < 400 {
		t.Fatalf("status = %d, want non-success", response.Code)
	}
	if strings.Contains(response.Body.String(), "provider leaked") || strings.Contains(response.Body.String(), "15550002222") {
		t.Fatalf("response leaked provider error or input: %s", response.Body.String())
	}
}

func TestPersonalReactionStillTargetsOutboundMessage(t *testing.T) {
	m := setupTestManager(t)
	manager = m
	client := NewLoggedInMockClient()
	injectMockSession(m, 7, client)
	response := httptest.NewRecorder()
	sendReactionHandler(response, httptest.NewRequest(http.MethodPost, "/messages/react", strings.NewReader(`{"user_id":7,"chat_jid":"15550002222@s.whatsapp.net","message_id":"outbound-1","emoji":"👍"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	calls := client.GetCallsByMethod("SendMessage")
	if len(calls) != 1 || !calls[0].Args[2].(*waE2E.Message).GetReactionMessage().GetKey().GetFromMe() {
		t.Fatal("personal reaction no longer targets an outbound message")
	}
}
