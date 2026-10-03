package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSharedInboxAckMakesCapacityForDurableTombstone(t *testing.T) {
	m := NewSessionManager(t.TempDir(), "", "")
	store := &sharedInboxStore{manager: m}
	entry, _, err := store.persistInbound(2, durableInboxEvent("ack-capacity"))
	if err != nil {
		t.Fatal(err)
	}
	readyInboxEntry(t, store, 2, entry.InboxID, "owner")

	tombstoneData, err := store.encode(&sharedInboxTombstone{InboxID: entry.InboxID, Outcome: "completed", CreatedAt: time.Now().UnixNano()})
	if err != nil {
		t.Fatal(err)
	}
	used, err := store.usedBytesLocked(2)
	if err != nil {
		t.Fatal(err)
	}
	// Leave one byte too little to add a tombstone alongside the payload, while
	// retaining enough capacity after the payload is removed.
	reserve := int64(sharedInboxMaxBytes) - used - int64(len(tombstoneData)) + 1
	if reserve <= 0 {
		t.Fatalf("invalid test reserve: %d", reserve)
	}
	for remaining, i := reserve, 0; remaining > 0; i++ {
		size := remaining
		if size > sharedInboxMaxDiskBytes {
			size = sharedInboxMaxDiskBytes
		}
		reservedPath := filepath.Join(store.numberDir(2), "entries", fmt.Sprintf("reserved-%d.json", i))
		if err := os.WriteFile(reservedPath, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(reservedPath, size); err != nil {
			t.Fatal(err)
		}
		remaining -= size
	}

	if err := store.ack(2, entry.InboxID, "completed", ""); err != nil {
		t.Fatalf("terminal ack lost to temporary tombstone capacity: %v", err)
	}
	if tombstone, err := store.readTombstone(2, entry.InboxID); err != nil || tombstone == nil {
		t.Fatalf("terminal tombstone = %#v, %v", tombstone, err)
	}
	if _, err := os.Stat(store.entryPath(2, entry.SourceKey)); !os.IsNotExist(err) {
		t.Fatalf("terminal payload was retained: %v", err)
	}
	if used, err := store.usedBytesLocked(2); err != nil || used > sharedInboxMaxBytes {
		t.Fatalf("terminal ack capacity = %d, %v", used, err)
	}
	if _, duplicate, err := store.persistInbound(2, durableInboxEvent("ack-capacity")); err != nil || !duplicate {
		t.Fatalf("terminal duplicate suppression = %v, %v", duplicate, err)
	}
}

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
