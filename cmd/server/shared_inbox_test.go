package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func durableInboxEvent(id string) MessageEvent {
	return MessageEvent{Type: "message", Payload: MessagePayload{
		ID: id, ChatJID: "15550001111@s.whatsapp.net", SenderJID: "15550002222@s.whatsapp.net",
		Text: "test", MediaType: "image", MimeType: "image/jpeg", DirectPath: "/media/path", MediaKey: []byte{1, 2},
	}}
}

func readyInboxEntry(t *testing.T, store *sharedInboxStore, numberID int, inboxID, owner string) {
	t.Helper()
	if _, err := store.checkpoint(numberID, inboxID, "started", owner, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.checkpoint(numberID, inboxID, "ready", owner, json.RawMessage(`{"delivery_key":"test"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestSharedInboxRestartReplayDuplicateAndAck(t *testing.T) {
	dataDir := t.TempDir()
	m := NewSessionManager(dataDir, "", "")
	store := &sharedInboxStore{manager: m}
	entry, duplicate, err := store.persistInbound(2, durableInboxEvent("provider-message"))
	if err != nil || duplicate || entry.Event.ProcessingState != "pending" || entry.Event.InboxID == "" {
		t.Fatalf("persist = %#v duplicate=%v err=%v", entry, duplicate, err)
	}
	duplicateEntry, duplicate, err := store.persistInbound(2, durableInboxEvent("provider-message"))
	if err != nil || !duplicate || duplicateEntry.InboxID != entry.InboxID {
		t.Fatalf("duplicate = %#v %v %v", duplicateEntry, duplicate, err)
	}

	// A fresh manager has no channel history; replay comes only from disk.
	restarted := NewSessionManager(dataDir, "", "")
	entries, err := (&sharedInboxStore{manager: restarted}).list(2)
	if err != nil || len(entries) != 1 || entries[0].InboxID != entry.InboxID {
		t.Fatalf("restart replay = %#v, %v", entries, err)
	}
	payload, ok := entries[0].Event.Payload.(map[string]interface{})
	if !ok || payload["media_type"] != "image" || payload["mime_type"] != "image/jpeg" {
		t.Fatalf("media metadata lost: %#v", entries[0].Event.Payload)
	}

	readyInboxEntry(t, &sharedInboxStore{manager: restarted}, 2, entry.InboxID, "owner")
	if err := (&sharedInboxStore{manager: restarted}).ack(2, entry.InboxID, "completed", ""); err != nil {
		t.Fatal(err)
	}
	if err := (&sharedInboxStore{manager: restarted}).ack(2, entry.InboxID, "completed", ""); err != nil {
		t.Fatalf("lost ack retry: %v", err)
	}
	if entries, err := (&sharedInboxStore{manager: restarted}).list(2); err != nil || len(entries) != 0 {
		t.Fatalf("acked replay = %#v, %v", entries, err)
	}
	if _, duplicate, err := (&sharedInboxStore{manager: restarted}).persistInbound(2, durableInboxEvent("provider-message")); err != nil || !duplicate {
		t.Fatalf("tombstone duplicate=%v err=%v", duplicate, err)
	}
}

func TestSharedInboxSSEReplayAndFreshLoad(t *testing.T) {
	m, _ := setupInternalNumberManager(t)
	session := m.numberSessions[2].session
	session.EventChan = make(chan MessageEvent, 2)
	store := &sharedInboxStore{manager: m}
	replayed, _, err := store.persistInbound(2, durableInboxEvent("replay"))
	if err != nil {
		t.Fatal(err)
	}

	stream := func() (*flushSignalRecorder, func(), <-chan struct{}) {
		ctx, cancel := context.WithCancel(context.Background())
		response := &flushSignalRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
		done := make(chan struct{})
		go func() {
			linqNumberInternalHandler(response, internalNumberRequest(http.MethodGet, "/internal/linq-numbers/2/events", "", testInternalToken).WithContext(ctx))
			close(done)
		}()
		return response, cancel, done
	}
	response, cancel, done := stream()
	select {
	case <-response.flushed:
	case <-time.After(time.Second):
		t.Fatal("replay did not flush")
	}
	if !strings.Contains(response.Body.String(), replayed.InboxID) {
		t.Fatalf("replay missing durable entry: %s", response.Body.String())
	}
	cancel()
	<-done
	readyInboxEntry(t, store, 2, replayed.InboxID, "owner")
	if err := store.ack(2, replayed.InboxID, "completed", ""); err != nil {
		t.Fatal(err)
	}

	response, cancel, done = stream()
	fresh, _, err := store.persistInbound(2, durableInboxEvent("fresh"))
	if err != nil {
		t.Fatal(err)
	}
	// The channel notification has only an inbox ID. The stream must read the
	// durable entry, rather than trusting an in-memory payload.
	session.EventChan <- MessageEvent{Type: "message", InboxID: fresh.InboxID}
	select {
	case <-response.flushed:
	case <-time.After(time.Second):
		t.Fatal("fresh event did not flush")
	}
	cancel()
	<-done
	if !strings.Contains(response.Body.String(), fresh.InboxID) || !strings.Contains(response.Body.String(), `"media_type":"image"`) {
		t.Fatalf("fresh event was not loaded from inbox: %s", response.Body.String())
	}
}

func TestSharedInboxCheckpointHTTPStateAndAuth(t *testing.T) {
	m, _ := setupInternalNumberManager(t)
	store := &sharedInboxStore{manager: m}
	entry, _, err := store.persistInbound(2, durableInboxEvent("checkpoint-message"))
	if err != nil {
		t.Fatal(err)
	}
	path := "/internal/linq-numbers/2/events/checkpoint"
	body := `{"inbox_id":"` + entry.InboxID + `","processing_state":"started","attempt_token":"owner"}`
	for _, token := range []string{"", "wrong"} {
		w := httptest.NewRecorder()
		linqNumberInternalHandler(w, internalNumberRequest(http.MethodPost, path, body, token))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("auth status = %d", w.Code)
		}
		w = httptest.NewRecorder()
		linqNumberInternalHandler(w, internalNumberRequest(http.MethodPost, "/internal/linq-numbers/2/events/ack", `{"inbox_id":"`+entry.InboxID+`","outcome":"completed"}`, token))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("ack auth status = %d", w.Code)
		}
	}
	w := httptest.NewRecorder()
	linqNumberInternalHandler(w, internalNumberRequest(http.MethodPost, path, body, testInternalToken))
	if w.Code != http.StatusOK {
		t.Fatalf("started=%d %s", w.Code, w.Body.String())
	}
	ready := `{"inbox_id":"` + entry.InboxID + `","processing_state":"ready","attempt_token":"owner","response_checkpoint":{"delivery_key":"k","text":"ok"}}`
	w = httptest.NewRecorder()
	linqNumberInternalHandler(w, internalNumberRequest(http.MethodPost, path, ready, testInternalToken))
	if w.Code != http.StatusOK {
		t.Fatalf("ready=%d %s", w.Code, w.Body.String())
	}
	// Whitespace/key-order changes are the same JSON checkpoint and are idempotent.
	readyRetry := `{"inbox_id":"` + entry.InboxID + `","processing_state":"ready","attempt_token":"owner","response_checkpoint":{"text":"ok", "delivery_key":"k"}}`
	w = httptest.NewRecorder()
	linqNumberInternalHandler(w, internalNumberRequest(http.MethodPost, path, readyRetry, testInternalToken))
	if w.Code != http.StatusOK {
		t.Fatalf("ready retry=%d %s", w.Code, w.Body.String())
	}
	changed := strings.Replace(ready, `"ok"`, `"changed"`, 1)
	w = httptest.NewRecorder()
	linqNumberInternalHandler(w, internalNumberRequest(http.MethodPost, path, changed, testInternalToken))
	if w.Code != http.StatusConflict {
		t.Fatalf("immutable ready=%d", w.Code)
	}

	entry2, _, err := store.persistInbound(2, durableInboxEvent("pending-ready"))
	if err != nil {
		t.Fatal(err)
	}
	pendingReady := `{"inbox_id":"` + entry2.InboxID + `","processing_state":"ready","attempt_token":"owner","response_checkpoint":{}}`
	w = httptest.NewRecorder()
	linqNumberInternalHandler(w, internalNumberRequest(http.MethodPost, path, pendingReady, testInternalToken))
	if w.Code != http.StatusConflict {
		t.Fatalf("pending->ready=%d", w.Code)
	}
}

func TestSharedInboxNumberIsolationEncryptionFailureAndCap(t *testing.T) {
	m := NewSessionManager(t.TempDir(), "", "")
	store := &sharedInboxStore{manager: m}
	entry, _, err := store.persistInbound(2, durableInboxEvent("isolated"))
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := store.list(3); err != nil || len(entries) != 0 {
		t.Fatalf("number isolation entries=%#v err=%v", entries, err)
	}
	if err := store.ack(3, entry.InboxID, "completed", ""); err != errInboxNotFound {
		t.Fatalf("cross-number ack = %v", err)
	}

	if err := os.WriteFile(filepath.Join(m.dataDir, "linq_number_inbox"), []byte("not a directory"), 0600); err == nil {
		t.Fatal("expected existing inbox directory")
	}
	// A separate manager exercises a directory creation failure before any notification.
	brokenDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(brokenDir, "linq_number_inbox"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	broken := &sharedInboxStore{manager: NewSessionManager(brokenDir, "", "")}
	if _, _, err := broken.persistInbound(2, durableInboxEvent("write-failure")); err == nil {
		t.Fatal("persistence failure accepted an event")
	}

	tooLarge := durableInboxEvent("too-large")
	payload := tooLarge.Payload.(MessagePayload)
	payload.Text = strings.Repeat("x", sharedInboxMaxEventBytes)
	tooLarge.Payload = payload
	if _, err := store.encode(tooLarge); err == nil {
		t.Fatal("oversized event bypassed cap")
	}
}

func TestSharedInboxEncryptsWhenKeyConfigured(t *testing.T) {
	key := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" // 32 zero bytes, test-only
	m := NewSessionManager(t.TempDir(), "", key)
	store := &sharedInboxStore{manager: m}
	entry, _, err := store.persistInbound(2, durableInboxEvent("encrypted"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.entryPath(2, sharedInboxSourceKey(entry.Event.Payload.(MessagePayload))))
	if err != nil {
		t.Fatal(err)
	}
	if json.Valid(data) && strings.Contains(string(data), `"text":"test"`) {
		t.Fatal("encrypted inbox body was plaintext")
	}
}
