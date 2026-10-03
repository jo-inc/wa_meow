package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

const maxLinqNumberFeedbackRequestBytes = 4096

type linqNumberTypingRequest struct {
	ChatJID *string `json:"chat_jid"`
	Typing  *bool   `json:"typing"`
}

type linqNumberReactionRequest struct {
	ChatJID   *string `json:"chat_jid"`
	MessageID *string `json:"message_id"`
	Emoji     *string `json:"emoji"`
}

func decodeLinqNumberFeedbackRequest(w http.ResponseWriter, r *http.Request, request interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxLinqNumberFeedbackRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(request); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid feedback request")
		return false
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		errorResponse(w, http.StatusBadRequest, "invalid feedback request")
		return false
	}
	return true
}

func validLinqNumberFeedbackChatJID(value *string) (string, bool) {
	if value == nil || len(*value) > 512 {
		return "", false
	}
	chatJID := strings.TrimSpace(*value)
	if chatJID == "" {
		return "", false
	}
	return chatJID, true
}

// sendInternalLinqNumberTypingHandler is called only after linqNumberInternalHandler
// has authenticated the internal caller and resolved a positive number ID.
func sendInternalLinqNumberTypingHandler(w http.ResponseWriter, r *http.Request, numberID int) {
	if r.Method != http.MethodPost {
		errorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request linqNumberTypingRequest
	if !decodeLinqNumberFeedbackRequest(w, r, &request) {
		return
	}
	chatJID, valid := validLinqNumberFeedbackChatJID(request.ChatJID)
	if !valid || request.Typing == nil {
		errorResponse(w, http.StatusBadRequest, "chat jid and typing required")
		return
	}
	jid, err := parseDirectChatJID(chatJID)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "direct chat jid required")
		return
	}

	_, session, verified := manager.verifiedConnectedNumberSession(numberID)
	if !verified {
		errorResponse(w, http.StatusConflict, "registered number account is not connected and verified")
		return
	}
	session.SendMu.Lock()
	defer session.SendMu.Unlock()
	_, current, verified := manager.verifiedConnectedNumberSession(numberID)
	if !verified || current != session {
		errorResponse(w, http.StatusConflict, "registered number account is not connected and verified")
		return
	}
	if err := sendTyping(r.Context(), session, jid, *request.Typing); err != nil {
		errorResponse(w, http.StatusBadGateway, "WhatsApp typing delivery failed")
		return
	}
	jsonResponse(w, map[string]string{"status": "accepted"})
}

// sendInternalLinqNumberReactionHandler is called only after linqNumberInternalHandler
// has authenticated the internal caller and resolved a positive number ID.
func sendInternalLinqNumberReactionHandler(w http.ResponseWriter, r *http.Request, numberID int) {
	if r.Method != http.MethodPost {
		errorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request linqNumberReactionRequest
	if !decodeLinqNumberFeedbackRequest(w, r, &request) {
		return
	}
	chatJID, valid := validLinqNumberFeedbackChatJID(request.ChatJID)
	if !valid || request.MessageID == nil || strings.TrimSpace(*request.MessageID) == "" || len(*request.MessageID) > 512 || request.Emoji == nil || len(*request.Emoji) > 256 {
		errorResponse(w, http.StatusBadRequest, "chat jid, message id, and emoji required")
		return
	}
	jid, err := parseDirectChatJID(chatJID)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "direct chat jid required")
		return
	}

	_, session, verified := manager.verifiedConnectedNumberSession(numberID)
	if !verified {
		errorResponse(w, http.StatusConflict, "registered number account is not connected and verified")
		return
	}
	session.SendMu.Lock()
	defer session.SendMu.Unlock()
	_, current, verified := manager.verifiedConnectedNumberSession(numberID)
	if !verified || current != session {
		errorResponse(w, http.StatusConflict, "registered number account is not connected and verified")
		return
	}
	// An empty emoji clears this account's reaction. Repeating the same desired
	// emoji is intentionally safe: reactions are provider state, not receipts.
	if _, err := sendReaction(r.Context(), session, jid, *request.MessageID, *request.Emoji, false); err != nil {
		errorResponse(w, http.StatusBadGateway, "WhatsApp reaction delivery failed")
		return
	}
	jsonResponse(w, map[string]string{"status": "accepted"})
}
