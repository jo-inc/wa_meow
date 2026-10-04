package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
)

const linqNumberAudioPath = "/internal/linq-numbers/2/messages/audio"

func linqNumberAudioBody(audioB64, mimeType, deliveryKey string) string {
	return `{"chat_jid":"15550001111@s.whatsapp.net","audio_b64":"` + audioB64 + `","mime_type":"` + mimeType + `","delivery_key":"` + deliveryKey + `"}`
}

func sendInternalLinqNumberAudio(t *testing.T, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	linqNumberInternalHandler(response, internalNumberRequest(http.MethodPost, linqNumberAudioPath, body, token))
	return response
}

func TestInternalLinqNumberAudioRequiresInternalAuthentication(t *testing.T) {
	m, client := setupInternalNumberManager(t)
	body := linqNumberAudioBody(base64.StdEncoding.EncodeToString([]byte("synthetic audio")), "audio/ogg", "audio-auth")
	for _, token := range []string{"", "wrong"} {
		response := sendInternalLinqNumberAudio(t, body, token)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("token %q status=%d, want 401", token, response.Code)
		}
	}
	m.joBotInternalToken = ""
	if response := sendInternalLinqNumberAudio(t, body, testInternalToken); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled internal gate status=%d, want 503", response.Code)
	}
	if len(client.GetCalls()) != 0 {
		t.Fatal("unauthenticated audio attempted provider calls")
	}
}

func TestInternalLinqNumberAudioBuildsScopedMessageAndDeduplicates(t *testing.T) {
	m, numberClient := setupInternalNumberManager(t)
	personalClient := NewLoggedInMockClient()
	injectMockSession(m, 2, personalClient)
	body := `{"chat_jid":"15550001111@s.whatsapp.net","audio_b64":"` + base64.StdEncoding.EncodeToString([]byte("synthetic audio bytes")) + `","mime_type":"audio/ogg; codecs=opus","ptt":true,"seconds":7,"reply_to":"quoted-synthetic-id","delivery_key":"audio-delivery-1"}`

	response := sendInternalLinqNumberAudio(t, body, testInternalToken)
	if response.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", response.Code, response.Body.String())
	}
	if len(personalClient.GetCalls()) != 0 {
		t.Fatal("number audio used a personal session")
	}
	uploads := numberClient.GetCallsByMethod("Upload")
	sends := numberClient.GetCallsByMethod("SendMessage")
	if len(uploads) != 1 || len(sends) != 1 || uploads[0].Args[2] != whatsmeow.MediaAudio {
		t.Fatalf("provider calls uploads=%d sends=%d", len(uploads), len(sends))
	}
	audio := sends[0].Args[2].(*waE2E.Message).GetAudioMessage()
	if audio.GetMimetype() != "audio/ogg; codecs=opus" || !audio.GetPTT() || audio.GetSeconds() != 7 || audio.GetFileLength() != uint64(len("synthetic audio bytes")) || audio.GetContextInfo().GetStanzaID() != "quoted-synthetic-id" {
		t.Fatalf("unexpected audio message: %#v", audio)
	}
	extra := sends[0].Args[3].([]whatsmeow.SendRequestExtra)
	if len(extra) != 1 || extra[0].ID != stableLinqNumberMessageID(2, "audio-delivery-1") {
		t.Fatalf("provider ID=%#v", extra)
	}

	response = sendInternalLinqNumberAudio(t, body, testInternalToken)
	if response.Code != http.StatusOK || len(numberClient.GetCallsByMethod("Upload")) != 1 || len(numberClient.GetCallsByMethod("SendMessage")) != 1 {
		t.Fatalf("duplicate status=%d uploads=%d sends=%d", response.Code, len(numberClient.GetCallsByMethod("Upload")), len(numberClient.GetCallsByMethod("SendMessage")))
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result["deduplicated"] != true {
		t.Fatalf("duplicate response=%s err=%v", response.Body.String(), err)
	}

	defaultBody := linqNumberAudioBody(base64.StdEncoding.EncodeToString([]byte("default audio")), "audio/mpeg", "audio-defaults")
	response = sendInternalLinqNumberAudio(t, defaultBody, testInternalToken)
	if response.Code != http.StatusOK {
		t.Fatalf("default send status=%d", response.Code)
	}
	defaults := numberClient.GetCallsByMethod("SendMessage")[1].Args[2].(*waE2E.Message).GetAudioMessage()
	if defaults.GetPTT() || defaults.GetSeconds() != 0 {
		t.Fatalf("audio defaults ptt=%t seconds=%d", defaults.GetPTT(), defaults.GetSeconds())
	}
}

func TestInternalLinqNumberAudioReceiptFailureRetriesWithStableID(t *testing.T) {
	m, client := setupInternalNumberManager(t)
	body := linqNumberAudioBody(base64.StdEncoding.EncodeToString([]byte("receipt audio")), "audio/ogg", "audio-receipt")
	m.numberReceiptWriter = func(*SessionManager, int, string, deliveryReceipt) error {
		return errors.New("synthetic receipt failure")
	}
	first := sendInternalLinqNumberAudio(t, body, testInternalToken)
	if first.Code != http.StatusInternalServerError || len(client.GetCallsByMethod("Upload")) != 1 || len(client.GetCallsByMethod("SendMessage")) != 1 {
		t.Fatalf("partial result status=%d uploads=%d sends=%d", first.Code, len(client.GetCallsByMethod("Upload")), len(client.GetCallsByMethod("SendMessage")))
	}
	firstID := client.GetCallsByMethod("SendMessage")[0].Args[3].([]whatsmeow.SendRequestExtra)[0].ID
	m.numberReceiptWriter = writeLinqNumberDeliveryReceipt
	second := sendInternalLinqNumberAudio(t, body, testInternalToken)
	if second.Code != http.StatusOK || len(client.GetCallsByMethod("Upload")) != 2 || len(client.GetCallsByMethod("SendMessage")) != 2 {
		t.Fatalf("recovery status=%d uploads=%d sends=%d", second.Code, len(client.GetCallsByMethod("Upload")), len(client.GetCallsByMethod("SendMessage")))
	}
	secondID := client.GetCallsByMethod("SendMessage")[1].Args[3].([]whatsmeow.SendRequestExtra)[0].ID
	if firstID != stableLinqNumberMessageID(2, "audio-receipt") || firstID != secondID {
		t.Fatalf("provider IDs %q and %q are not deterministic", firstID, secondID)
	}
}

func TestInternalLinqNumberAudioRejectsUnsafeInputAndUnavailableNumber(t *testing.T) {
	_, client := setupInternalNumberManager(t)
	valid := base64.StdEncoding.EncodeToString([]byte("synthetic audio"))
	for _, test := range []struct{ name, body string }{
		{"group", `{"chat_jid":"123@g.us","audio_b64":"` + valid + `","mime_type":"audio/ogg","delivery_key":"group"}`},
		{"noncanonical encoding", linqNumberAudioBody(valid+"\n", "audio/ogg", "encoding")},
		{"invalid mime", linqNumberAudioBody(valid, "image/jpeg", "mime")},
		{"malformed mime", linqNumberAudioBody(valid, "audio/ogg; bad", "mime-malformed")},
		{"seconds cap", `{"chat_jid":"15550001111@s.whatsapp.net","audio_b64":"` + valid + `","mime_type":"audio/ogg","seconds":86401,"delivery_key":"seconds"}`},
		{"unknown field", linqNumberAudioBody(valid, "audio/ogg", "unknown")[:len(linqNumberAudioBody(valid, "audio/ogg", "unknown"))-1] + `,"unexpected":true}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := sendInternalLinqNumberAudio(t, test.body, testInternalToken)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	tooLarge := strings.Repeat("A", base64.StdEncoding.EncodedLen(maxLinqNumberDecodedMediaBytes+1))
	response := sendInternalLinqNumberAudio(t, linqNumberAudioBody(tooLarge, "audio/ogg", "too-large"), testInternalToken)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("decoded media cap status=%d body=%s", response.Code, response.Body.String())
	}
	if countMockCalls(client.GetCalls(), "Upload") != 0 || countMockCalls(client.GetCalls(), "SendMessage") != 0 {
		t.Fatal("invalid audio reached provider")
	}

	client.SetConnected(false)
	response = sendInternalLinqNumberAudio(t, linqNumberAudioBody(valid, "audio/ogg", "disconnected"), testInternalToken)
	if response.Code != http.StatusConflict || countMockCalls(client.GetCalls(), "Upload") != 0 || countMockCalls(client.GetCalls(), "SendMessage") != 0 {
		t.Fatalf("disconnected status=%d uploads=%d sends=%d", response.Code, countMockCalls(client.GetCalls(), "Upload"), countMockCalls(client.GetCalls(), "SendMessage"))
	}
	client.SetConnected(true)
	client.store.ID.User = "15550009999"
	response = sendInternalLinqNumberAudio(t, linqNumberAudioBody(valid, "audio/ogg", "mismatch"), testInternalToken)
	if response.Code != http.StatusConflict || countMockCalls(client.GetCalls(), "Upload") != 0 || countMockCalls(client.GetCalls(), "SendMessage") != 0 {
		t.Fatalf("identity mismatch status=%d uploads=%d sends=%d", response.Code, countMockCalls(client.GetCalls(), "Upload"), countMockCalls(client.GetCalls(), "SendMessage"))
	}
}

func TestInternalLinqNumberAudioProviderFailuresAreSanitized(t *testing.T) {
	_, client := setupInternalNumberManager(t)
	body := linqNumberAudioBody(base64.StdEncoding.EncodeToString([]byte("synthetic audio")), "audio/ogg", "audio-provider")
	for _, setFailure := range []func(){
		func() { client.UploadError = errors.New("provider secret synthetic media") },
		func() {
			client.UploadError = nil
			client.SendMessageError = errors.New("provider secret synthetic send")
		},
	} {
		setFailure()
		response := sendInternalLinqNumberAudio(t, body, testInternalToken)
		if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), "provider secret") || strings.Contains(response.Body.String(), "synthetic audio") {
			t.Fatalf("unsafe failure status=%d body=%s", response.Code, response.Body.String())
		}
	}
}
