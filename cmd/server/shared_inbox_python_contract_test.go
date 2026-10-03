package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
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
	if len(client.GetCallsByMethod("SendMessage")) != 3 {
		t.Fatal("contract did not deduplicate text, document, and image delivery")
	}
}
