package main

import (
	"os"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const providerTestNumberID = 41

func newProviderInboxSession(t *testing.T, notificationBuffer int) (*SessionManager, *UserSession) {
	t.Helper()
	manager := NewSessionManager(t.TempDir(), "", "")
	session := &UserSession{
		Kind:         LinqNumberSessionKind,
		LinqNumberID: providerTestNumberID,
		EventChan:    make(chan MessageEvent, notificationBuffer),
		Inbox:        &sharedInboxStore{manager: manager},
		MediaCache:   make(map[string]*mediaCacheEntry),
	}
	return manager, session
}

func providerTextEvent(id string) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:   types.JID{User: "15550000001", Server: types.DefaultUserServer},
				Sender: types.JID{User: "15550000002", Server: types.DefaultUserServer},
			},
			ID:        types.MessageID(id),
			Timestamp: time.Unix(1, 0),
		},
		Message: &waE2E.Message{Conversation: proto.String("test")},
	}
}

func providerContactEvent(id string) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:   types.JID{User: "15550000001", Server: types.DefaultUserServer},
				Sender: types.JID{User: "15550000002", Server: types.DefaultUserServer},
			},
			ID:        types.MessageID(id),
			Timestamp: time.Unix(1, 0),
		},
		Message: &waE2E.Message{ContactsArrayMessage: &waE2E.ContactsArrayMessage{Contacts: []*waE2E.ContactMessage{
			{DisplayName: proto.String("first"), Vcard: proto.String("first-vcard")},
			{DisplayName: proto.String("second"), Vcard: proto.String("second-vcard")},
		}}},
	}
}

func providerInboxEntries(t *testing.T, manager *SessionManager) []sharedInboxEntry {
	t.Helper()
	entries, err := (&sharedInboxStore{manager: manager}).list(providerTestNumberID)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestNumberProviderACKDurableAcceptanceAndDuplicate(t *testing.T) {
	manager, session := newProviderInboxSession(t, 2)
	event := providerTextEvent("provider-durable")

	if !session.handleNumberEvent(event) {
		t.Fatal("durable inbound event was not accepted")
	}
	entries := providerInboxEntries(t, manager)
	if len(entries) != 1 || entries[0].Event.InboxID == "" {
		t.Fatalf("durable entries = %#v", entries)
	}

	if !session.handleNumberEvent(event) {
		t.Fatal("duplicate durable inbound event was not accepted")
	}
	if entries := providerInboxEntries(t, manager); len(entries) != 1 {
		t.Fatalf("duplicate created %d entries", len(entries))
	}
}

func TestNumberProviderACKNotificationOverflowAfterDurability(t *testing.T) {
	manager, session := newProviderInboxSession(t, 1)
	session.EventChan <- MessageEvent{Type: "filler"}

	if !session.handleNumberEvent(providerTextEvent("provider-overflow")) {
		t.Fatal("durable event with deferred notification was not accepted")
	}
	if entries := providerInboxEntries(t, manager); len(entries) != 1 {
		t.Fatalf("deferred notification lost durable entry: %#v", entries)
	}
	if got := <-session.EventChan; got.Type != "filler" {
		t.Fatalf("notification channel was unexpectedly replaced: %#v", got)
	}
}

func TestNumberProviderACKFailsWithoutDurableStorage(t *testing.T) {
	manager, session := newProviderInboxSession(t, 1)
	if err := os.WriteFile(manager.dataDir+"/linq_number_inbox", []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}

	if session.handleNumberEvent(providerTextEvent("provider-storage-failure")) {
		t.Fatal("storage failure was acknowledged to provider")
	}
	if len(session.EventChan) != 0 {
		t.Fatal("storage failure published an in-memory notification")
	}
}

func TestNumberProviderACKFailsForClosedLifecycle(t *testing.T) {
	manager, session := newProviderInboxSession(t, 1)
	session.lifecycleMu.Lock()
	session.closed = true
	session.lifecycleMu.Unlock()

	if session.handleNumberEvent(providerTextEvent("provider-closed")) {
		t.Fatal("closed session was acknowledged to provider")
	}
	if entries := providerInboxEntries(t, manager); len(entries) != 0 {
		t.Fatalf("closed session persisted entries: %#v", entries)
	}
}

func TestNumberProviderACKContactRetryKeepsAcceptedPrefix(t *testing.T) {
	manager, session := newProviderInboxSession(t, 2)
	event := providerContactEvent("provider-contacts")
	second := 1
	secondPayload := MessagePayload{
		ID:           event.Info.ID,
		ChatJID:      event.Info.Chat.String(),
		SenderJID:    event.Info.Sender.String(),
		ContactIndex: &second,
	}
	blockedPath := session.Inbox.entryPath(providerTestNumberID, sharedInboxSourceKey(secondPayload))
	if err := os.MkdirAll(blockedPath, 0700); err != nil {
		t.Fatal(err)
	}

	if session.handleNumberEvent(event) {
		t.Fatal("partially persisted contact array was acknowledged to provider")
	}
	if err := os.Remove(blockedPath); err != nil {
		t.Fatal(err)
	}
	if entries := providerInboxEntries(t, manager); len(entries) != 1 {
		t.Fatalf("accepted contact prefix = %#v", entries)
	}

	if !session.handleNumberEvent(event) {
		t.Fatal("contact retry was not accepted after remaining storage became available")
	}
	if entries := providerInboxEntries(t, manager); len(entries) != 2 {
		t.Fatalf("contact retry did not retain prefix and store remainder: %#v", entries)
	}
}
