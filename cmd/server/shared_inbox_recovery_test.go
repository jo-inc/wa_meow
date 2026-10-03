package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestSharedInboxAckCrashResidueIsRemovedWithoutReplay(t *testing.T) {
	m := NewSessionManager(t.TempDir(), "", "")
	store := &sharedInboxStore{manager: m}
	entry, _, err := store.persistInbound(2, durableInboxEvent("ack-crash"))
	if err != nil {
		t.Fatal(err)
	}
	readyInboxEntry(t, store, 2, entry.InboxID, "owner")
	path := store.entryPath(2, entry.SourceKey)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ack(2, entry.InboxID, "completed", ""); err != nil {
		t.Fatal(err)
	}
	// Model a crash after durable tombstone creation but before body removal.
	if err := atomicPrivateWrite(path, body); err != nil {
		t.Fatal(err)
	}
	restarted := &sharedInboxStore{manager: NewSessionManager(m.dataDir, "", "")}
	if entries, err := restarted.references(2, nil); err != nil || len(entries) != 0 {
		t.Fatal("acknowledged content was replayed after a crash")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("acknowledged content residue was retained")
	}
	if err := restarted.ack(2, entry.InboxID, "completed", ""); err != nil {
		t.Fatal("lost ack was not idempotent")
	}
}

func TestSharedInboxReferencesDoNotRetainFrozenResponseBodies(t *testing.T) {
	m := NewSessionManager(t.TempDir(), "", "")
	store := &sharedInboxStore{manager: m}
	entry, _, err := store.persistInbound(2, durableInboxEvent("large-response"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.checkpoint(2, entry.InboxID, "started", "owner", nil); err != nil {
		t.Fatal(err)
	}
	checkpoint := json.RawMessage(`{"text":"frozen response"}`)
	if _, err := store.checkpoint(2, entry.InboxID, "ready", "owner", checkpoint); err != nil {
		t.Fatal(err)
	}
	refs, err := store.references(2, nil)
	if err != nil || len(refs) != 1 || refs[0].Event.Payload != nil || len(refs[0].Event.ResponseCheckpoint) != 0 || refs[0].AttemptToken != "" {
		t.Fatal("replay references retained sensitive response/ownership content")
	}
	if refs, err := store.references(2, map[string]bool{entry.InboxID: true}); err != nil || len(refs) != 0 {
		t.Fatal("periodic scan reread an already-emitted record")
	}
	loaded, err := store.byInboxID(2, entry.InboxID)
	if err != nil || string(loaded.Event.ResponseCheckpoint) != string(checkpoint) {
		t.Fatal("keyed replay lost frozen response")
	}
}

func TestSharedNumberClientUsesDurableProviderAckOptions(t *testing.T) {
	m := NewSessionManager(t.TempDir(), "", "")
	if _, err := m.registerLinqNumber(linqNumberRecord{ID: 2, Phone: testPairingPhone}); err != nil {
		t.Fatal(err)
	}
	state, err := m.getOrCreateLinqNumberSession(2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.session.close(false) })
	client := state.session.Client.(*realClientWrapper).client
	if !client.SynchronousAck || !client.EnableDecryptedEventBuffer || state.session.Inbox == nil {
		t.Fatal("shared session lacks provider-ack durability configuration")
	}
}
