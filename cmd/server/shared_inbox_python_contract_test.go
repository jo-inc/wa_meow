package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

// Opt-in, synthetic loopback fixture for the real Python consumer helpers.
// No WhatsApp socket is opened: the existing mock client captures all sends.
func TestSharedInboxPythonContractFixture(t *testing.T) {
	if os.Getenv("JO_WHATSAPP_INBOX_CONTRACT_TEST") != "1" {
		t.Skip("opt-in cross-language contract fixture")
	}
	m, client := setupInternalNumberManager(t)
	m.joBotInternalToken = "synthetic-inbox-contract-token"
	store := &sharedInboxStore{manager: m}
	entry, _, err := store.persistInbound(2, durableInboxEvent("contract-inbound"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(linqNumberInternalHandler))
	defer server.Close()
	metadata, _ := json.Marshal(map[string]string{"url": server.URL, "inbox_id": entry.InboxID})
	fmt.Printf("INBOX_CONTRACT_FIXTURE=%s\n", metadata)
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal("contract client did not finish")
	}
	status, err := store.status(2)
	if err != nil || status.Pending+status.Started+status.Ready != 0 {
		t.Fatal("contract left an unacknowledged event")
	}
	sends := client.GetCallsByMethod("SendMessage")
	if len(sends) != 5 {
		t.Fatalf("contract sends = %d, want frozen reply plus one deduplicated voice pair", len(sends))
	}
	if len(m.sessions) != 0 || len(m.numberSessions) != 1 || m.numberSessions[2].session == nil || m.numberSessions[2].session.Client != client {
		t.Fatal("contract did not remain on its one verified number session")
	}
	expectedJID := types.JID{User: "15550001111", Server: types.DefaultUserServer}
	for index, call := range sends {
		jid, ok := call.Args[1].(types.JID)
		if !ok || jid != expectedJID {
			t.Fatalf("send %d accepted an unexpected direct/group recipient: %#v", index, call.Args[1])
		}
	}
	audio := sends[3].Args[2].(*waE2E.Message).GetAudioMessage()
	if audio == nil || audio.GetMimetype() != "audio/ogg; codecs=opus" || !audio.GetPTT() || audio.GetSeconds() != 0 || audio.GetFileLength() != uint64(len("synthetic-contract-ogg-opus")) {
		t.Fatalf("fourth send is not the expected voice note: %#v", audio)
	}
	uploads := client.GetCallsByMethod("Upload")
	if len(uploads) != 3 || uploads[2].Args[2] != whatsmeow.MediaAudio {
		t.Fatalf("uploads = %#v, want original document/image plus voice audio", uploads)
	}
	payload, ok := uploads[2].Args[1].([]byte)
	if !ok || fmt.Sprintf("%x", sha256.Sum256(payload)) != fmt.Sprintf("%x", sha256.Sum256([]byte("synthetic-contract-ogg-opus"))) {
		t.Fatal("voice upload payload did not preserve the frozen synthetic bytes")
	}
	transcript := sends[4].Args[2].(*waE2E.Message)
	if transcript.GetConversation() != "📝 Synthetic voice transcript" {
		t.Fatalf("fifth send transcript=%q, want production transcript without a jo prefix", transcript.GetConversation())
	}
	receiptPaths, err := filepath.Glob(filepath.Join(m.dataDir, "linq_number_delivery_receipts", "2", "*.json"))
	if err != nil || len(receiptPaths) != 5 {
		t.Fatalf("receipt files = %d, err=%v, want 5 deduplicated deliveries", len(receiptPaths), err)
	}
	for _, path := range receiptPaths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var receipt deliveryReceipt
		if err := json.Unmarshal(data, &receipt); err != nil || receipt.ID == "" {
			t.Fatalf("invalid delivery receipt %q: %#v, %v", path, receipt, err)
		}
	}
}
