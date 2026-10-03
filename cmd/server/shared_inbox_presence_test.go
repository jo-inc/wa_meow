package main

import (
	"testing"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestSharedNumberTypingNeverCreatesOrRepeatsMessageEvents(t *testing.T) {
	manager, session := newProviderInboxSession(t, 2)
	presence := &events.ChatPresence{
		MessageSource: types.MessageSource{
			Chat:   types.JID{User: "15550000001", Server: types.DefaultUserServer},
			Sender: types.JID{User: "15550000002", Server: types.DefaultUserServer},
		},
		Media: types.ChatPresenceMediaText,
	}
	for _, state := range []types.ChatPresence{types.ChatPresenceComposing, types.ChatPresencePaused} {
		presence.State = state
		if !session.handleNumberEvent(presence) {
			t.Fatal("benign presence rejected")
		}
	}
	if len(providerInboxEntries(t, manager)) != 0 || len(session.EventChan) != 0 {
		t.Fatal("typing generated a durable message or SSE notification")
	}
	if !session.handleNumberEvent(providerTextEvent("synthetic-real-message")) {
		t.Fatal("control text message was not accepted")
	}
	for _, state := range []types.ChatPresence{types.ChatPresenceComposing, types.ChatPresencePaused} {
		presence.State = state
		if !session.handleNumberEvent(presence) {
			t.Fatal("benign presence rejected with existing work")
		}
	}
	if len(providerInboxEntries(t, manager)) != 1 || len(session.EventChan) != 1 {
		t.Fatal("typing repeated or altered an existing message")
	}
}
