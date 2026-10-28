package main

import (
	"context"
	"log"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// callDeclineMessage is sent once to anyone who tries to call, since Jo has no
// voice or video surface.
const callDeclineMessage = "Jo doesn't do audio or video calls. Send me a text message and I'll help from there."

// videoDeclineMessage is sent once to anyone who sends a video message, since
// the bridge has no video processing path.
const videoDeclineMessage = "I can't process videos yet. Send me a text message and I'll help from there."

// handleIncomingCall rejects an incoming WhatsApp call and, the first time a
// given number calls, replies explaining that Jo has no call surface. Jo has
// no voice/video surface, so accepting is never correct. It runs in its own
// goroutine so the network round-trips don't block the event handler.
// Subsequent calls from the same number are still rejected but not re-messaged,
// to avoid spamming a persistent caller. media is "audio"/"video" when known.
func (s *UserSession) handleIncomingCall(callFrom types.JID, callID, media string) {
	ctx := context.Background()
	caller := callFrom.ToNonAD()

	if err := s.Client.RejectCall(ctx, caller, callID); err != nil {
		log.Printf("[call] reject failed for %s (media=%q): %v", piiFingerprint(caller.String()), media, err)
	} else {
		log.Printf("[call] rejected incoming call from %s (media=%q)", piiFingerprint(caller.String()), media)
	}

	s.callOffendersMu.Lock()
	if s.callOffenders == nil {
		s.callOffenders = make(map[string]int)
	}
	s.callOffenders[caller.String()]++
	firstCall := s.callOffenders[caller.String()] == 1
	s.callOffendersMu.Unlock()

	if firstCall {
		msg := &waE2E.Message{Conversation: proto.String(callDeclineMessage)}
		if _, err := s.Client.SendMessage(ctx, caller, msg); err != nil {
			log.Printf("[call] decline message failed for %s: %v", piiFingerprint(caller.String()), err)
		} else {
			log.Printf("[call] sent call-decline message to %s", piiFingerprint(caller.String()))
		}
	}
}

// declineUnsupportedVideo tells a sender once that Jo can't process video, then
// drops repeat videos silently so a persistent sender can't spam Jo. The video
// is never forwarded to the model. It runs in its own goroutine. chat is the
// reply target; sender identifies the person for one-time dedupe.
func (s *UserSession) declineUnsupportedVideo(chat, sender types.JID) {
	ctx := context.Background()
	key := sender.ToNonAD().String()

	s.videoOffendersMu.Lock()
	if s.videoOffenders == nil {
		s.videoOffenders = make(map[string]int)
	}
	s.videoOffenders[key]++
	firstVideo := s.videoOffenders[key] == 1
	s.videoOffendersMu.Unlock()

	if !firstVideo {
		log.Printf("[video] dropped repeat video from %s (already notified)", piiFingerprint(key))
		return
	}

	msg := &waE2E.Message{Conversation: proto.String(videoDeclineMessage)}
	if _, err := s.Client.SendMessage(ctx, chat, msg); err != nil {
		log.Printf("[video] decline message failed for %s: %v", piiFingerprint(key), err)
	} else {
		log.Printf("[video] sent video-unsupported message to %s", piiFingerprint(key))
	}
}
