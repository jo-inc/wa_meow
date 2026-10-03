package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type loginOnNumberConnect struct{ *MockWhatsAppClient }

type idleNumberHandshakeRecorder struct{ *flushSignalRecorder }

func (r *idleNumberHandshakeRecorder) Flush() {
	r.once.Do(func() { close(r.flushed) })
}

func TestNumberStreamFlushesEmptyInboxHandshake(t *testing.T) {
	setupInternalNumberManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response := &idleNumberHandshakeRecorder{&flushSignalRecorder{
		ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{}),
	}}
	request := internalNumberRequest(http.MethodGet, "/internal/linq-numbers/2/events", "", testInternalToken).WithContext(ctx)
	done := make(chan struct{})
	go func() { linqNumberInternalHandler(response, request); close(done) }()
	select {
	case <-response.flushed:
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("empty inbox did not flush its handshake")
	}
	cancel()
	<-done
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatal("invalid authenticated SSE handshake")
	}
}

func (client *loginOnNumberConnect) Connect() error {
	if err := client.MockWhatsAppClient.Connect(); err != nil {
		return err
	}
	client.SetLoggedIn(true)
	return nil
}

func TestNumberStreamReconnectsSavedPairingAfterRestart(t *testing.T) {
	manager, client := setupInternalNumberManager(t)
	client.SetConnected(false)
	client.SetLoggedIn(false)
	session := manager.numberSessions[2].session
	session.Client = &loginOnNumberConnect{client}
	session.EventChan = make(chan MessageEvent, 1)
	session.EventChan <- MessageEvent{Type: "message", Payload: MessagePayload{ID: "synthetic-reconnect"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := internalNumberRequest(http.MethodGet, "/internal/linq-numbers/2/events", "", testInternalToken).WithContext(ctx)
	response := &flushSignalRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
	done := make(chan struct{})
	go func() { linqNumberInternalHandler(response, request); close(done) }()
	select {
	case <-response.flushed:
	case <-done:
		t.Fatalf("saved pairing rejected before reconnect: status=%d", response.Code)
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("saved pairing did not reconnect")
	}
	cancel()
	<-done
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "synthetic-reconnect") {
		t.Fatal("reconnected stream did not deliver its event")
	}
	if !client.IsConnected() || !client.IsLoggedIn() {
		t.Fatal("stream admitted without live authenticated connection")
	}
}

func TestNumberStreamRejectsMismatchedSavedPairingBeforeConnecting(t *testing.T) {
	manager, client := setupInternalNumberManager(t)
	client.SetConnected(false)
	client.SetLoggedIn(false)
	manager.numberSessions[2].Phone = "+15550002222"
	response := httptest.NewRecorder()
	linqNumberInternalHandler(response, internalNumberRequest(http.MethodGet, "/internal/linq-numbers/2/events", "", testInternalToken))
	if response.Code != http.StatusConflict || countMockCalls(client.GetCalls(), "Connect") != 0 {
		t.Fatal("mismatched saved pairing must not connect or stream")
	}
}

func TestNumberStreamRequiresLoginAfterConnecting(t *testing.T) {
	manager, client := setupInternalNumberManager(t)
	client.SetConnected(false)
	client.SetLoggedIn(false)
	manager.connectReadyWait = time.Millisecond
	manager.connectReadyPoll = time.Millisecond
	response := httptest.NewRecorder()
	linqNumberInternalHandler(response, internalNumberRequest(http.MethodGet, "/internal/linq-numbers/2/events", "", testInternalToken))
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "data:") {
		t.Fatal("connected but unauthenticated pairing must not stream")
	}
}
