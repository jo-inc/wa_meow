package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSharedInboxOwnerCASAndAckContract(t *testing.T) {
	store := &sharedInboxStore{manager: NewSessionManager(t.TempDir(), "", "")}
	entry, _, err := store.persistInbound(2, durableInboxEvent("owner-cas"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.checkpoint(2, entry.InboxID, "started", "", nil); !errors.Is(err, errInboxConflict) {
		t.Fatalf("tokenless start: %v", err)
	}
	if _, err := store.checkpoint(2, entry.InboxID, "started", "owner-a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.checkpoint(2, entry.InboxID, "started", "owner-a", nil); err != nil {
		t.Fatalf("same owner retry: %v", err)
	}
	if _, err := store.checkpoint(2, entry.InboxID, "started", "owner-b", nil); !errors.Is(err, errInboxConflict) {
		t.Fatalf("wrong owner start: %v", err)
	}
	if err := store.ack(2, entry.InboxID, "ignored", ""); !errors.Is(err, errInboxConflict) {
		t.Fatalf("tokenless started ignore: %v", err)
	}
	if err := store.ack(2, entry.InboxID, "ignored", "owner-b"); !errors.Is(err, errInboxConflict) {
		t.Fatalf("wrong owner ignore: %v", err)
	}
	checkpoint := json.RawMessage(`{"delivery_key":"contract"}`)
	if _, err := store.checkpoint(2, entry.InboxID, "ready", "owner-b", checkpoint); !errors.Is(err, errInboxConflict) {
		t.Fatalf("wrong owner ready: %v", err)
	}
	if _, err := store.checkpoint(2, entry.InboxID, "ready", "owner-a", checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := store.ack(2, entry.InboxID, "superseded", "owner-b"); !errors.Is(err, errInboxConflict) {
		t.Fatalf("wrong owner supersede: %v", err)
	}
	if err := store.ack(2, entry.InboxID, "completed", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.ack(2, entry.InboxID, "completed", "other"); err != nil {
		t.Fatalf("tombstone retry: %v", err)
	}
}

func TestSharedInboxContactArrayKeysAndTotalCap(t *testing.T) {
	m := NewSessionManager(t.TempDir(), "", "")
	store := &sharedInboxStore{manager: m}
	index0, index1 := 0, 1
	first := durableInboxEvent("contacts")
	p := first.Payload.(MessagePayload)
	p.MediaType, p.ContactIndex, p.ContactName, p.ContactVCard = "contact", &index0, "", ""
	first.Payload = p
	second := first
	p.ContactIndex = &index1
	second.Payload = p
	one, duplicate, err := store.persistInbound(2, first)
	if err != nil || duplicate {
		t.Fatalf("first contact: duplicate=%v err=%v", duplicate, err)
	}
	two, duplicate, err := store.persistInbound(2, second)
	if err != nil || duplicate || one.InboxID == two.InboxID {
		t.Fatalf("duplicate contact array members collapsed: duplicate=%v err=%v", duplicate, err)
	}

	fullDir := filepath.Join(m.dataDir, "linq_number_inbox", "3", "entries")
	if err := os.MkdirAll(fullDir, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		reserved := filepath.Join(fullDir, fmt.Sprintf("reserved-%d.json", i))
		if err := os.WriteFile(reserved, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(reserved, sharedInboxMaxDiskBytes); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := store.persistInbound(3, durableInboxEvent("capacity")); !errors.Is(err, errInboxFull) {
		t.Fatalf("total artifact cap: %v", err)
	}
}

func TestSharedInboxStatusAuthIsolationAndDroppedNotificationReplay(t *testing.T) {
	m, _ := setupInternalNumberManager(t)
	store := &sharedInboxStore{manager: m}
	entry, _, err := store.persistInbound(2, durableInboxEvent("status"))
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "wrong"} {
		w := httptest.NewRecorder()
		linqNumberInternalHandler(w, internalNumberRequest(http.MethodGet, "/internal/linq-numbers/2/events/status", "", token))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status auth=%d", w.Code)
		}
	}
	w := httptest.NewRecorder()
	linqNumberInternalHandler(w, internalNumberRequest(http.MethodGet, "/internal/linq-numbers/2/events/status", "", testInternalToken))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), entry.InboxID) || !strings.Contains(w.Body.String(), `"pending":1`) {
		t.Fatalf("status disclosure/body: %d %s", w.Code, w.Body.String())
	}
	if _, err := store.status(3); err != nil {
		t.Fatal(err)
	}

	// Fill the notification channel before publishing. The active stream must
	// discover the durable entry on its periodic scan without reconnecting.
	session := m.numberSessions[2].session
	session.Inbox = store
	session.EventChan = make(chan MessageEvent, 1)
	session.EventChan <- MessageEvent{Type: "notice"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response := &flushSignalRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		linqNumberInternalHandler(response, internalNumberRequest(http.MethodGet, "/internal/linq-numbers/2/events", "", testInternalToken).WithContext(ctx))
		close(done)
	}()
	select {
	case <-response.flushed:
	case <-time.After(time.Second):
		t.Fatal("stream did not start")
	}
	// Existing status entry was emitted at stream start; use a new one whose
	// EventChan notification is deliberately dropped after the channel refills.
	session.EventChan <- MessageEvent{Type: "notice"}
	session.publishMessageEvent(durableInboxEvent("dropped-notification"))
	time.Sleep(2300 * time.Millisecond)
	cancel()
	<-done
	if !strings.Contains(response.Body.String(), "dropped-notification") {
		t.Fatalf("periodic replay missed deferred notification: %s", response.Body.String())
	}
}
