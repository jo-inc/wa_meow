package main

import (
	"errors"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func TestHandleIncomingCall(t *testing.T) {
	makeSession := func(mock *MockWhatsAppClient) *UserSession {
		return &UserSession{
			UserID:        1,
			Client:        mock,
			EventChan:     make(chan MessageEvent, 10),
			MediaCache:    make(map[string]*mediaCacheEntry),
			callOffenders: make(map[string]int),
		}
	}
	caller := types.JID{User: "15551234567", Server: types.DefaultUserServer}

	t.Run("rejects incoming call and messages caller once", func(t *testing.T) {
		mock := NewLoggedInMockClient()
		session := makeSession(mock)

		session.handleIncomingCall(caller, "call-001", "video")

		if rejects := mock.GetCallsByMethod("RejectCall"); len(rejects) != 1 {
			t.Fatalf("expected 1 RejectCall, got %d", len(rejects))
		}
		if sends := mock.GetCallsByMethod("SendMessage"); len(sends) != 1 {
			t.Fatalf("expected 1 decline SendMessage, got %d", len(sends))
		}
	})

	t.Run("rejects repeat calls but only messages once", func(t *testing.T) {
		mock := NewLoggedInMockClient()
		session := makeSession(mock)

		for i := 0; i < 3; i++ {
			session.handleIncomingCall(caller, "call", "audio")
		}

		if rejects := mock.GetCallsByMethod("RejectCall"); len(rejects) != 3 {
			t.Errorf("expected 3 RejectCall, got %d", len(rejects))
		}
		if sends := mock.GetCallsByMethod("SendMessage"); len(sends) != 1 {
			t.Errorf("expected exactly 1 decline message across repeat calls, got %d", len(sends))
		}
	})

	t.Run("reject failure still sends decline message", func(t *testing.T) {
		mock := NewLoggedInMockClient()
		mock.RejectCallError = errors.New("network")
		session := makeSession(mock)

		session.handleIncomingCall(caller, "call", "")
		if sends := mock.GetCallsByMethod("SendMessage"); len(sends) != 1 {
			t.Errorf("expected decline message even when reject errors, got %d", len(sends))
		}
	})

	t.Run("lazy-inits offender map when nil", func(t *testing.T) {
		mock := NewLoggedInMockClient()
		session := &UserSession{UserID: 2, Client: mock}

		session.handleIncomingCall(caller, "call", "video")
		if sends := mock.GetCallsByMethod("SendMessage"); len(sends) != 1 {
			t.Errorf("expected 1 decline message with nil offender map, got %d", len(sends))
		}
	})
}
