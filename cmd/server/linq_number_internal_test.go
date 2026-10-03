package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const testInternalToken = "internal-routing-token-for-tests-only"

func internalNumberRequest(method, path, body, token string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("X-WhatsApp-Internal-Token", token)
	}
	return req
}

func setupInternalNumberManager(t *testing.T) (*SessionManager, *MockWhatsAppClient) {
	t.Helper()
	m, client := setupLinqSendManager(t, testPairingPhone)
	m.joBotInternalToken = testInternalToken
	return m, client
}

func TestInternalLinqNumberEndpointsRequireDedicatedToken(t *testing.T) {
	m, _ := setupInternalNumberManager(t)
	for _, path := range []string{
		"/internal/linq-numbers/2/messages/send",
		"/internal/linq-numbers/2/messages/image",
		"/internal/linq-numbers/2/messages/document",
	} {
		for _, token := range []string{"", "wrong"} {
			response := httptest.NewRecorder()
			linqNumberInternalHandler(response, internalNumberRequest(http.MethodPost, path, `{}`, token))
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("path %q token %q status = %d, want 401", path, token, response.Code)
			}
		}
	}
	m.joBotInternalToken = ""
	response := httptest.NewRecorder()
	linqNumberInternalHandler(response, internalNumberRequest(http.MethodPost, "/internal/linq-numbers/2/messages/image", `{}`, testInternalToken))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing configured token status = %d, want 503", response.Code)
	}
}

func TestInternalLinqNumberSendRequiresVerifiedConnectedAccount(t *testing.T) {
	m, client := setupInternalNumberManager(t)
	client.SetConnected(false)
	response := httptest.NewRecorder()
	linqNumberInternalHandler(response, internalNumberRequest(http.MethodPost, "/internal/linq-numbers/2/messages/send", `{"chat_jid":"15550001111@s.whatsapp.net","text":"test","delivery_key":"internal-1"}`, testInternalToken))
	if response.Code != http.StatusConflict {
		t.Fatalf("disconnected account status = %d, want 409", response.Code)
	}
	if countMockCalls(client.GetCalls(), "Connect") != 0 || countMockCalls(client.GetCalls(), "SendMessage") != 0 {
		t.Fatal("internal send attempted to connect or send an unverified account")
	}
	response = httptest.NewRecorder()
	linqNumberInternalHandler(response, internalNumberRequest(http.MethodPost, "/internal/linq-numbers/2/messages/image", `{"chat_jid":"15550001111@s.whatsapp.net","image_b64":"aW1hZ2U=","mime_type":"image/jpeg","delivery_key":"not-connected"}`, testInternalToken))
	if response.Code != http.StatusConflict || countMockCalls(client.GetCalls(), "Upload") != 0 {
		t.Fatalf("unverified media status=%d uploads=%d", response.Code, countMockCalls(client.GetCalls(), "Upload"))
	}
	m.numberSessions[2].session = nil
	response = httptest.NewRecorder()
	linqNumberInternalHandler(response, internalNumberRequest(http.MethodGet, "/internal/linq-numbers/2/events", "", testInternalToken))
	if response.Code != http.StatusConflict {
		t.Fatalf("unavailable registered session status = %d, want 409", response.Code)
	}
}

func TestInternalLinqNumberSendRepliesAndDeduplicates(t *testing.T) {
	_, client := setupInternalNumberManager(t)
	body := `{"chat_jid":"15550001111@s.whatsapp.net","text":"reply text","reply_to":"inbound-message-id","delivery_key":"internal-reply-1"}`
	path := "/internal/linq-numbers/2/messages/send"
	response := httptest.NewRecorder()
	linqNumberInternalHandler(response, internalNumberRequest(http.MethodPost, path, body, testInternalToken))
	if response.Code != http.StatusOK {
		t.Fatalf("send status = %d, body=%s", response.Code, response.Body.String())
	}
	for _, call := range client.GetCalls() {
		if call.Method != "SendMessage" {
			continue
		}
		message := call.Args[2].(*waE2E.Message)
		if message.GetExtendedTextMessage().GetText() != "reply text" || message.GetExtendedTextMessage().GetContextInfo().GetStanzaID() != "inbound-message-id" {
			t.Fatalf("reply context was not sent: %#v", message.GetExtendedTextMessage())
		}
		extra := call.Args[3].([]whatsmeow.SendRequestExtra)
		if len(extra) != 1 || extra[0].ID != stableLinqNumberMessageID(2, "internal-reply-1") {
			t.Fatalf("delivery key did not produce stable provider id: %#v", extra)
		}
	}
	response = httptest.NewRecorder()
	linqNumberInternalHandler(response, internalNumberRequest(http.MethodPost, path, body, testInternalToken))
	if response.Code != http.StatusOK || countMockCalls(client.GetCalls(), "SendMessage") != 1 {
		t.Fatalf("duplicate was not idempotent: status=%d calls=%d", response.Code, countMockCalls(client.GetCalls(), "SendMessage"))
	}
	var result map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["deduplicated"] != true {
		t.Fatalf("duplicate response = %#v", result)
	}
}

func TestInternalLinqNumberMediaBuildsAndDeduplicatesWithoutReupload(t *testing.T) {
	for _, test := range []struct {
		name, endpoint, payload string
		assert                  func(*testing.T, *waE2E.Message)
	}{
		{
			name: "image", endpoint: "image",
			payload: `"image_b64":"` + base64.StdEncoding.EncodeToString([]byte("image bytes")) + `","mime_type":"image/jpeg","caption":"photo caption"`,
			assert: func(t *testing.T, message *waE2E.Message) {
				image := message.GetImageMessage()
				if image.GetMimetype() != "image/jpeg" || image.GetCaption() != "photo caption" || image.GetFileLength() != uint64(len("image bytes")) || image.GetContextInfo().GetStanzaID() != "quoted-id" {
					t.Fatalf("incorrect image message: %#v", image)
				}
			},
		},
		{
			name: "document", endpoint: "document",
			payload: `"doc_b64":"` + base64.StdEncoding.EncodeToString([]byte("document bytes")) + `","mime_type":"application/pdf","filename":"report.pdf","caption":"document caption"`,
			assert: func(t *testing.T, message *waE2E.Message) {
				document := message.GetDocumentMessage()
				if document.GetMimetype() != "application/pdf" || document.GetFileName() != "report.pdf" || document.GetCaption() != "document caption" || document.GetFileLength() != uint64(len("document bytes")) || document.GetContextInfo().GetStanzaID() != "quoted-id" {
					t.Fatalf("incorrect document message: %#v", document)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, numberClient := setupInternalNumberManager(t)
			personalClient := NewLoggedInMockClient()
			injectMockSession(m, 2, personalClient)
			path := "/internal/linq-numbers/2/messages/" + test.endpoint
			body := `{"chat_jid":"15550001111@s.whatsapp.net",` + test.payload + `,"reply_to":"quoted-id","delivery_key":"media-` + test.name + `"}`
			response := httptest.NewRecorder()
			linqNumberInternalHandler(response, internalNumberRequest(http.MethodPost, path, body, testInternalToken))
			if response.Code != http.StatusOK {
				t.Fatalf("send status=%d body=%s", response.Code, response.Body.String())
			}
			if len(personalClient.GetCallsByMethod("Upload")) != 0 || len(personalClient.GetCallsByMethod("SendMessage")) != 0 {
				t.Fatal("number-scoped media used the personal user session")
			}
			uploads := numberClient.GetCallsByMethod("Upload")
			sends := numberClient.GetCallsByMethod("SendMessage")
			if len(uploads) != 1 || len(sends) != 1 {
				t.Fatalf("calls: uploads=%d sends=%d", len(uploads), len(sends))
			}
			test.assert(t, sends[0].Args[2].(*waE2E.Message))
			extra := sends[0].Args[3].([]whatsmeow.SendRequestExtra)
			if len(extra) != 1 || extra[0].ID != stableLinqNumberMessageID(2, "media-"+test.name) {
				t.Fatalf("unstable provider ID: %#v", extra)
			}
			response = httptest.NewRecorder()
			linqNumberInternalHandler(response, internalNumberRequest(http.MethodPost, path, body, testInternalToken))
			if response.Code != http.StatusOK || len(numberClient.GetCallsByMethod("Upload")) != 1 || len(numberClient.GetCallsByMethod("SendMessage")) != 1 {
				t.Fatalf("duplicate reuploaded or resent: status=%d uploads=%d sends=%d", response.Code, len(numberClient.GetCallsByMethod("Upload")), len(numberClient.GetCallsByMethod("SendMessage")))
			}
		})
	}
}

func TestInternalLinqNumberMediaPersistenceFailureRetriesWithStableID(t *testing.T) {
	m, client := setupInternalNumberManager(t)
	m.numberReceiptWriter = func(*SessionManager, int, string, deliveryReceipt) error { return errors.New("receipt write failed") }
	body := `{"chat_jid":"15550001111@s.whatsapp.net","image_b64":"aW1hZ2U=","mime_type":"image/jpeg","delivery_key":"receipt-retry"}`
	path := "/internal/linq-numbers/2/messages/image"
	first := httptest.NewRecorder()
	linqNumberInternalHandler(first, internalNumberRequest(http.MethodPost, path, body, testInternalToken))
	if first.Code != http.StatusInternalServerError || len(client.GetCallsByMethod("Upload")) != 1 || len(client.GetCallsByMethod("SendMessage")) != 1 {
		t.Fatalf("persistence failure status=%d uploads=%d sends=%d", first.Code, len(client.GetCallsByMethod("Upload")), len(client.GetCallsByMethod("SendMessage")))
	}
	firstExtra := client.GetCallsByMethod("SendMessage")[0].Args[3].([]whatsmeow.SendRequestExtra)[0].ID
	m.numberReceiptWriter = writeLinqNumberDeliveryReceipt
	second := httptest.NewRecorder()
	linqNumberInternalHandler(second, internalNumberRequest(http.MethodPost, path, body, testInternalToken))
	if second.Code != http.StatusOK || len(client.GetCallsByMethod("Upload")) != 2 || len(client.GetCallsByMethod("SendMessage")) != 2 {
		t.Fatalf("retry status=%d uploads=%d sends=%d", second.Code, len(client.GetCallsByMethod("Upload")), len(client.GetCallsByMethod("SendMessage")))
	}
	secondExtra := client.GetCallsByMethod("SendMessage")[1].Args[3].([]whatsmeow.SendRequestExtra)[0].ID
	if firstExtra != stableLinqNumberMessageID(2, "receipt-retry") || secondExtra != firstExtra {
		t.Fatalf("retry provider IDs = %q, %q", firstExtra, secondExtra)
	}
}

func TestInternalLinqNumberMediaUsesNumberSessionCache(t *testing.T) {
	m, _ := setupInternalNumberManager(t)
	numberSession := m.numberSessions[2].session
	if !numberSession.mediaCachePut("synthetic-media-id", []byte("synthetic media")) {
		t.Fatal("could not prepare synthetic number-session media")
	}
	response := httptest.NewRecorder()
	request := internalNumberRequest(
		http.MethodPost,
		"/internal/linq-numbers/2/media/download",
		`{"message_id":"synthetic-media-id","mime_type":"image/jpeg"}`,
		testInternalToken,
	)
	linqNumberInternalHandler(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"data":"c3ludGhldGljIG1lZGlh"`) {
		t.Fatalf("number-scoped media download status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestInternalLinqNumberEventsStreamOnlyOwnSession(t *testing.T) {
	m, _ := setupInternalNumberManager(t)
	numberSession := m.numberSessions[2].session
	numberSession.EventChan = make(chan MessageEvent, 1)
	numberSession.EventChan <- MessageEvent{Type: "message", Payload: MessagePayload{ID: "number-only"}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := internalNumberRequest(http.MethodGet, "/internal/linq-numbers/2/events", "", testInternalToken).WithContext(ctx)
	response := &flushSignalRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		linqNumberInternalHandler(response, req)
		close(done)
	}()
	select {
	case <-response.flushed:
	case <-time.After(time.Second):
		t.Fatal("number event was not streamed")
	}
	cancel()
	<-done
	if !strings.Contains(response.Body.String(), `"id":"number-only"`) {
		t.Fatalf("stream did not contain the number session event: %s", response.Body.String())
	}
}

type flushSignalRecorder struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
	once    sync.Once
}

func (r *flushSignalRecorder) Flush() {
	// Existing event-delivery tests wait for a body, not the initial headers.
	if r.Body.Len() > 0 {
		r.once.Do(func() { close(r.flushed) })
	}
}
func TestNumberInboundPayloadIncludesSenderAltJID(t *testing.T) {
	session := &UserSession{
		Kind: LinqNumberSessionKind, LinqNumberID: 2, EventChan: make(chan MessageEvent, 1),
		Inbox: &sharedInboxStore{manager: NewSessionManager(t.TempDir(), "", "")},
	}
	if !session.handleNumberEvent(&events.Message{
		Info: types.MessageInfo{MessageSource: types.MessageSource{
			Chat:      types.JID{User: "lid-address", Server: types.HiddenUserServer},
			Sender:    types.JID{User: "lid-address", Server: types.HiddenUserServer},
			SenderAlt: types.JID{User: "15550001111", Server: types.DefaultUserServer},
		}, ID: "inbound", Timestamp: time.Now()},
		Message: &waE2E.Message{Conversation: proto.String("message")},
	}) {
		t.Fatal("durable inbound acceptance failed")
	}
	event := <-session.EventChan
	payload := event.Payload.(MessagePayload)
	if payload.SenderAltJID != "15550001111@s.whatsapp.net" {
		t.Fatalf("sender alternative = %q", payload.SenderAltJID)
	}
}
