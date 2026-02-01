package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

type SessionManager struct {
	sessions   map[int]*UserSession
	mu         sync.RWMutex
	dataDir    string
	joBotURL   string
	encryptKey []byte
}

type UserSession struct {
	UserID    int
	Client    WhatsAppClient
	Container *sqlstore.Container
	DBPath    string
	LastUsed  time.Time
	QRChannel chan string
	LoginDone chan bool
	EventChan chan MessageEvent
}

type MessageEvent struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

type MessagePayload struct {
	ID         string `json:"id"`
	ChatJID    string `json:"chat_jid"`
	SenderJID  string `json:"sender_jid"`
	SenderName string `json:"sender_name"`
	Text       string `json:"text"`
	Timestamp  int64  `json:"timestamp"`
	IsFromMe   bool   `json:"is_from_me"`
	// Media fields
	MediaType string `json:"media_type,omitempty"` // "image", "location", etc.
	MediaURL  string `json:"media_url,omitempty"`
	MimeType  string `json:"mime_type,omitempty"`
	Caption   string `json:"caption,omitempty"`
	// Location fields
	Latitude  float64 `json:"latitude,omitempty"`
	Longitude float64 `json:"longitude,omitempty"`
	// Contact fields (vCard)
	ContactName  string `json:"contact_name,omitempty"`
	ContactVCard string `json:"contact_vcard,omitempty"`
	// Image download info (for downloading media)
	MediaKey      []byte `json:"media_key,omitempty"`
	DirectPath    string `json:"direct_path,omitempty"`
	FileEncSHA256 []byte `json:"file_enc_sha256,omitempty"`
	FileSHA256    []byte `json:"file_sha256,omitempty"`
	FileLength    uint64 `json:"file_length,omitempty"`
}

type ChatPayload struct {
	JID     string `json:"jid"`
	Name    string `json:"name"`
	IsGroup bool   `json:"is_group"`
}

var manager *SessionManager

func NewSessionManager(dataDir, joBotURL, encryptKeyB64 string) *SessionManager {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Printf("Warning: could not create data dir: %v", err)
	}
	
	var encryptKey []byte
	if encryptKeyB64 != "" {
		var err error
		encryptKey, err = base64.StdEncoding.DecodeString(encryptKeyB64)
		if err != nil || len(encryptKey) != 32 {
			log.Printf("Warning: invalid encryption key, session persistence disabled")
			encryptKey = nil
		}
	}
	
	return &SessionManager{
		sessions:   make(map[int]*UserSession),
		dataDir:    dataDir,
		joBotURL:   joBotURL,
		encryptKey: encryptKey,
	}
}

func (m *SessionManager) encrypt(data []byte) (string, error) {
	if m.encryptKey == nil {
		return "", fmt.Errorf("no encryption key")
	}
	
	block, err := aes.NewCipher(m.encryptKey)
	if err != nil {
		return "", err
	}
	
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	
	ciphertext := gcm.Seal(nonce, nonce, data, nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func (m *SessionManager) decrypt(encoded string) ([]byte, error) {
	if m.encryptKey == nil {
		return nil, fmt.Errorf("no encryption key")
	}
	
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	
	block, err := aes.NewCipher(m.encryptKey)
	if err != nil {
		return nil, err
	}
	
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	
	if len(data) < gcm.NonceSize() {
		return nil, fmt.Errorf("ciphertext too short")
	}
	
	nonce, ciphertext := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func (m *SessionManager) fetchSessionFromJoBot(userID int) error {
	if m.joBotURL == "" || m.encryptKey == nil {
		return nil
	}
	
	url := fmt.Sprintf("%s/api/whatsapp/session?user_id=%d", m.joBotURL, userID)
	resp, err := http.Get(url)
	if err != nil {
		log.Printf("Failed to fetch session from jo_bot: %v", err)
		return nil
	}
	defer resp.Body.Close()
	
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	
	var result struct {
		Data string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}
	
	if result.Data == "" {
		return nil
	}
	
	dbData, err := m.decrypt(result.Data)
	if err != nil {
		log.Printf("Failed to decrypt session: %v", err)
		return nil
	}
	
	dbPath := filepath.Join(m.dataDir, fmt.Sprintf("user_%d.db", userID))
	if err := os.WriteFile(dbPath, dbData, 0600); err != nil {
		log.Printf("Failed to write session db: %v", err)
		return err
	}
	
	log.Printf("✅ Restored session for user %d from jo_bot", userID)
	return nil
}

func (m *SessionManager) saveSessionToJoBot(userID int) error {
	if m.joBotURL == "" || m.encryptKey == nil {
		return nil
	}
	
	dbPath := filepath.Join(m.dataDir, fmt.Sprintf("user_%d.db", userID))
	dbData, err := os.ReadFile(dbPath)
	if err != nil {
		return err
	}
	
	encrypted, err := m.encrypt(dbData)
	if err != nil {
		return err
	}
	
	payload := map[string]interface{}{
		"user_id": userID,
		"data":    encrypted,
	}
	jsonData, _ := json.Marshal(payload)
	
	url := fmt.Sprintf("%s/api/whatsapp/session", m.joBotURL)
	resp, err := http.Post(url, "application/json", bytes.NewReader(jsonData))
	if err != nil {
		log.Printf("Failed to save session to jo_bot: %v", err)
		return err
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		log.Printf("Failed to save session: status %d", resp.StatusCode)
		return fmt.Errorf("save failed: %d", resp.StatusCode)
	}
	
	log.Printf("✅ Saved session for user %d to jo_bot", userID)
	return nil
}

func (m *SessionManager) GetOrCreateSession(userID int) (*UserSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if session, ok := m.sessions[userID]; ok {
		session.LastUsed = time.Now()
		return session, nil
	}

	// Try to restore session from jo_bot
	m.fetchSessionFromJoBot(userID)

	ctx := context.Background()
	dbPath := filepath.Join(m.dataDir, fmt.Sprintf("user_%d.db", userID))

	dbLog := waLog.Stdout("Database", "ERROR", true)
	container, err := sqlstore.New(ctx, "sqlite3", "file:"+dbPath+"?_foreign_keys=on", dbLog)
	if err != nil {
		return nil, fmt.Errorf("failed to create sqlstore: %w", err)
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get device: %w", err)
	}

	clientLog := waLog.Stdout("Client", "ERROR", true)
	rawClient := whatsmeow.NewClient(deviceStore, clientLog)
	client := newRealClientWrapper(rawClient)

	session := &UserSession{
		UserID:    userID,
		Client:    client,
		Container: container,
		DBPath:    dbPath,
		LastUsed:  time.Now(),
		QRChannel: make(chan string, 10),
		LoginDone: make(chan bool, 1),
		EventChan: make(chan MessageEvent, 100),
	}

	rawClient.AddEventHandler(func(evt interface{}) {
		session.handleEvent(evt)
	})

	m.sessions[userID] = session
	return session, nil
}

func (m *SessionManager) GetSession(userID int) *UserSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if session, ok := m.sessions[userID]; ok {
		session.LastUsed = time.Now()
		return session
	}
	return nil
}

func (m *SessionManager) RemoveSession(userID int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if session, ok := m.sessions[userID]; ok {
		session.Client.Disconnect()
		// Save session before removing
		m.saveSessionToJoBot(userID)
		delete(m.sessions, userID)
	}
}

func (m *SessionManager) SaveSession(userID int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.sessions[userID]; ok {
		m.saveSessionToJoBot(userID)
	}
}

func (s *UserSession) handleEvent(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		payload := MessagePayload{
			ID:         v.Info.ID,
			ChatJID:    v.Info.Chat.String(),
			SenderJID:  v.Info.Sender.String(),
			SenderName: v.Info.PushName,
			Timestamp:  v.Info.Timestamp.Unix(),
			IsFromMe:   v.Info.IsFromMe,
		}

		hasContent := false

		// Handle text messages
		if v.Message.Conversation != nil {
			payload.Text = *v.Message.Conversation
			hasContent = true
		} else if v.Message.ExtendedTextMessage != nil && v.Message.ExtendedTextMessage.Text != nil {
			payload.Text = *v.Message.ExtendedTextMessage.Text
			hasContent = true
		}

		// Handle image messages
		if img := v.Message.ImageMessage; img != nil {
			payload.MediaType = "image"
			if img.Caption != nil {
				payload.Caption = *img.Caption
			}
			if img.Mimetype != nil {
				payload.MimeType = *img.Mimetype
			}
			if img.URL != nil {
				payload.MediaURL = *img.URL
			}
			if img.DirectPath != nil {
				payload.DirectPath = *img.DirectPath
			}
			payload.MediaKey = img.MediaKey
			payload.FileEncSHA256 = img.FileEncSHA256
			payload.FileSHA256 = img.FileSHA256
			if img.FileLength != nil {
				payload.FileLength = *img.FileLength
			}
			hasContent = true
		}

		// Handle location messages
		if loc := v.Message.LocationMessage; loc != nil {
			payload.MediaType = "location"
			if loc.DegreesLatitude != nil {
				payload.Latitude = *loc.DegreesLatitude
			}
			if loc.DegreesLongitude != nil {
				payload.Longitude = *loc.DegreesLongitude
			}
			if loc.Name != nil {
				payload.Text = *loc.Name
			}
			if loc.Address != nil {
				if payload.Text != "" {
					payload.Text += " - " + *loc.Address
				} else {
					payload.Text = *loc.Address
				}
			}
			hasContent = true
		}

		// Handle live location messages
		if loc := v.Message.LiveLocationMessage; loc != nil {
			payload.MediaType = "live_location"
			if loc.DegreesLatitude != nil {
				payload.Latitude = *loc.DegreesLatitude
			}
			if loc.DegreesLongitude != nil {
				payload.Longitude = *loc.DegreesLongitude
			}
			if loc.Caption != nil {
				payload.Caption = *loc.Caption
			}
			hasContent = true
		}

		// Handle contact messages (single contact)
		if contact := v.Message.ContactMessage; contact != nil {
			payload.MediaType = "contact"
			if contact.DisplayName != nil {
				payload.ContactName = *contact.DisplayName
			}
			if contact.Vcard != nil {
				payload.ContactVCard = *contact.Vcard
			}
			hasContent = true
		}

		// Handle contact array messages (multiple contacts)
		if contacts := v.Message.ContactsArrayMessage; contacts != nil {
			// For multiple contacts, we'll send separate events for each
			for _, contact := range contacts.Contacts {
				contactPayload := MessagePayload{
					ID:         v.Info.ID,
					ChatJID:    v.Info.Chat.String(),
					SenderJID:  v.Info.Sender.String(),
					SenderName: v.Info.PushName,
					Timestamp:  v.Info.Timestamp.Unix(),
					IsFromMe:   v.Info.IsFromMe,
					MediaType:  "contact",
				}
				if contact.DisplayName != nil {
					contactPayload.ContactName = *contact.DisplayName
				}
				if contact.Vcard != nil {
					contactPayload.ContactVCard = *contact.Vcard
				}
				select {
				case s.EventChan <- MessageEvent{Type: "message", Payload: contactPayload}:
				default:
					log.Printf("Event channel full for user %d, dropping contact", s.UserID)
				}
			}
			// Don't set hasContent since we've already sent the events
		}

		if hasContent {
			select {
			case s.EventChan <- MessageEvent{Type: "message", Payload: payload}:
			default:
				log.Printf("Event channel full for user %d, dropping message", s.UserID)
			}
		}
	}
}

func jsonResponse(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func errorResponse(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, map[string]string{"status": "ok"})
}

func createSessionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req struct {
		UserID int `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid json")
		return
	}

	session, err := manager.GetOrCreateSession(req.UserID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	if session.Client.GetStore().GetID() == nil {
		qrChan, _ := session.Client.GetQRChannel(context.Background())
		err := session.Client.Connect()
		if err != nil {
			errorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}

		go func() {
			for evt := range qrChan {
				if evt.Event == "code" {
					select {
					case session.QRChannel <- evt.Code:
					default:
					}
				} else if evt.Event == "success" {
					select {
					case session.LoginDone <- true:
					default:
					}
					return
				}
			}
		}()

		jsonResponse(w, map[string]interface{}{
			"status":  "needs_qr",
			"user_id": req.UserID,
		})
		return
	}

	if !session.Client.IsConnected() {
		err := session.Client.Connect()
		if err != nil {
			errorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	jsonResponse(w, map[string]interface{}{
		"status":  "connected",
		"user_id": req.UserID,
		"phone":   session.Client.GetStore().GetID().User,
	})
}

func getQRHandler(w http.ResponseWriter, r *http.Request) {
	userID := 0
	fmt.Sscanf(r.URL.Query().Get("user_id"), "%d", &userID)
	if userID == 0 {
		errorResponse(w, http.StatusBadRequest, "user_id required")
		return
	}

	session := manager.GetSession(userID)
	if session == nil {
		errorResponse(w, http.StatusNotFound, "session not found")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		errorResponse(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	timeout := time.After(2 * time.Minute)
	for {
		select {
		case code := <-session.QRChannel:
			fmt.Fprintf(w, "event: qr\ndata: %s\n\n", code)
			flusher.Flush()
			log.Printf("📱 QR code generated for user %d (length: %d)", userID, len(code))

		case <-session.LoginDone:
			fmt.Fprintf(w, "event: success\ndata: logged_in\n\n")
			flusher.Flush()
			return

		case <-timeout:
			fmt.Fprintf(w, "event: timeout\ndata: qr_expired\n\n")
			flusher.Flush()
			return

		case <-r.Context().Done():
			return
		}
	}
}

func getStatusHandler(w http.ResponseWriter, r *http.Request) {
	userID := 0
	fmt.Sscanf(r.URL.Query().Get("user_id"), "%d", &userID)
	if userID == 0 {
		errorResponse(w, http.StatusBadRequest, "user_id required")
		return
	}

	session := manager.GetSession(userID)
	if session == nil {
		jsonResponse(w, map[string]interface{}{
			"connected": false,
			"logged_in": false,
		})
		return
	}

	resp := map[string]interface{}{
		"connected": session.Client.IsConnected(),
		"logged_in": session.Client.IsLoggedIn(),
	}

	if session.Client.GetStore().GetID() != nil {
		resp["phone"] = session.Client.GetStore().GetID().User
	}

	jsonResponse(w, resp)
}

func deleteSessionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		errorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	userID := 0
	fmt.Sscanf(r.URL.Query().Get("user_id"), "%d", &userID)
	if userID == 0 {
		errorResponse(w, http.StatusBadRequest, "user_id required")
		return
	}

	manager.RemoveSession(userID)
	jsonResponse(w, map[string]string{"status": "disconnected"})
}

func getChatsHandler(w http.ResponseWriter, r *http.Request) {
	userID := 0
	fmt.Sscanf(r.URL.Query().Get("user_id"), "%d", &userID)
	if userID == 0 {
		errorResponse(w, http.StatusBadRequest, "user_id required")
		return
	}

	session := manager.GetSession(userID)
	if session == nil {
		errorResponse(w, http.StatusNotFound, "session not found")
		return
	}

	if !session.Client.IsLoggedIn() {
		errorResponse(w, http.StatusBadRequest, "not logged in")
		return
	}

	ctx := context.Background()
	var chats []ChatPayload

	groups, err := session.Client.GetJoinedGroups(ctx)
	if err == nil {
		for _, group := range groups {
			chats = append(chats, ChatPayload{
				JID:     group.JID.String(),
				Name:    group.Name,
				IsGroup: true,
			})
		}
	}

	contacts, err := session.Client.GetStore().GetContacts().GetAllContacts(ctx)
	if err == nil {
		for jid, contact := range contacts {
			name := contact.PushName
			if name == "" {
				name = contact.FullName
			}
			if name == "" {
				name = jid.User
			}
			chats = append(chats, ChatPayload{
				JID:     jid.String(),
				Name:    name,
				IsGroup: false,
			})
		}
	}

	jsonResponse(w, chats)
}

func sendMessageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req struct {
		UserID  int    `json:"user_id"`
		ChatJID string `json:"chat_jid"`
		Text    string `json:"text"`
		ReplyTo string `json:"reply_to,omitempty"` // Optional message ID to reply to
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid json")
		return
	}

	session := manager.GetSession(req.UserID)
	if session == nil {
		errorResponse(w, http.StatusNotFound, "session not found")
		return
	}

	if !session.Client.IsLoggedIn() {
		errorResponse(w, http.StatusBadRequest, "not logged in")
		return
	}

	jid, err := types.ParseJID(req.ChatJID)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid jid")
		return
	}

	var msg *waE2E.Message
	if req.ReplyTo != "" {
		// Use ExtendedTextMessage with ContextInfo for reply
		msg = &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String(req.Text),
				ContextInfo: &waE2E.ContextInfo{
					StanzaID:      proto.String(req.ReplyTo),
					Participant:   proto.String(jid.String()),
					QuotedMessage: &waE2E.Message{Conversation: proto.String("")},
				},
			},
		}
	} else {
		msg = &waE2E.Message{
			Conversation: proto.String(req.Text),
		}
	}

	resp, err := session.Client.SendMessage(context.Background(), jid, msg)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonResponse(w, map[string]interface{}{
		"id":        resp.ID,
		"timestamp": resp.Timestamp.Unix(),
	})
}

func sendReactionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req struct {
		UserID    int    `json:"user_id"`
		ChatJID   string `json:"chat_jid"`
		MessageID string `json:"message_id"`
		Emoji     string `json:"emoji"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid json")
		return
	}

	session := manager.GetSession(req.UserID)
	if session == nil {
		errorResponse(w, http.StatusNotFound, "session not found")
		return
	}

	if !session.Client.IsLoggedIn() {
		errorResponse(w, http.StatusBadRequest, "not logged in")
		return
	}

	jid, err := types.ParseJID(req.ChatJID)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid jid")
		return
	}

	// Build reaction message
	msg := &waE2E.Message{
		ReactionMessage: &waE2E.ReactionMessage{
			Key: &waCommon.MessageKey{
				RemoteJID:   proto.String(req.ChatJID),
				FromMe:      proto.Bool(true),
				ID:          proto.String(req.MessageID),
			},
			Text:              proto.String(req.Emoji),
			SenderTimestampMS: proto.Int64(time.Now().UnixMilli()),
		},
	}

	resp, err := session.Client.SendMessage(context.Background(), jid, msg)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonResponse(w, map[string]interface{}{
		"id":        resp.ID,
		"timestamp": resp.Timestamp.Unix(),
	})
}

func setTypingHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req struct {
		UserID  int    `json:"user_id"`
		ChatJID string `json:"chat_jid"`
		Typing  bool   `json:"typing"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid json")
		return
	}

	session := manager.GetSession(req.UserID)
	if session == nil {
		errorResponse(w, http.StatusNotFound, "session not found")
		return
	}

	if !session.Client.IsLoggedIn() {
		errorResponse(w, http.StatusBadRequest, "not logged in")
		return
	}

	jid, err := types.ParseJID(req.ChatJID)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid jid")
		return
	}

	var presence types.ChatPresence
	if req.Typing {
		presence = types.ChatPresenceComposing
	} else {
		presence = types.ChatPresencePaused
	}

	err = session.Client.SendChatPresence(context.Background(), jid, presence, types.ChatPresenceMediaText)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonResponse(w, map[string]string{"status": "ok"})
}

func eventsHandler(w http.ResponseWriter, r *http.Request) {
	userID := 0
	fmt.Sscanf(r.URL.Query().Get("user_id"), "%d", &userID)
	if userID == 0 {
		errorResponse(w, http.StatusBadRequest, "user_id required")
		return
	}

	session := manager.GetSession(userID)
	if session == nil {
		errorResponse(w, http.StatusNotFound, "session not found")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		errorResponse(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	for {
		select {
		case evt := <-session.EventChan:
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
			flusher.Flush()

		case <-r.Context().Done():
			return
		}
	}
}

func saveSessionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	userID := 0
	fmt.Sscanf(r.URL.Query().Get("user_id"), "%d", &userID)
	if userID == 0 {
		errorResponse(w, http.StatusBadRequest, "user_id required")
		return
	}

	manager.SaveSession(userID)
	jsonResponse(w, map[string]string{"status": "saved"})
}

func sendImageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req struct {
		UserID   int    `json:"user_id"`
		ChatJID  string `json:"chat_jid"`
		ImageB64 string `json:"image_b64"` // Base64 encoded image
		MimeType string `json:"mime_type"` // e.g. "image/jpeg"
		Caption  string `json:"caption"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid json")
		return
	}

	session := manager.GetSession(req.UserID)
	if session == nil {
		errorResponse(w, http.StatusNotFound, "session not found")
		return
	}

	if !session.Client.IsLoggedIn() {
		errorResponse(w, http.StatusBadRequest, "not logged in")
		return
	}

	jid, err := types.ParseJID(req.ChatJID)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid jid")
		return
	}

	// Decode base64 image
	imageData, err := base64.StdEncoding.DecodeString(req.ImageB64)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid base64 image")
		return
	}

	// Upload to WhatsApp servers
	uploaded, err := session.Client.Upload(context.Background(), imageData, whatsmeow.MediaImage)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to upload image: "+err.Error())
		return
	}

	// Build and send image message
	msg := &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{
			Caption:       proto.String(req.Caption),
			URL:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			Mimetype:      proto.String(req.MimeType),
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(imageData))),
		},
	}

	resp, err := session.Client.SendMessage(context.Background(), jid, msg)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonResponse(w, map[string]interface{}{
		"id":        resp.ID,
		"timestamp": resp.Timestamp.Unix(),
	})
}

func sendLocationHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req struct {
		UserID    int     `json:"user_id"`
		ChatJID   string  `json:"chat_jid"`
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		Name      string  `json:"name"`
		Address   string  `json:"address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid json")
		return
	}

	session := manager.GetSession(req.UserID)
	if session == nil {
		errorResponse(w, http.StatusNotFound, "session not found")
		return
	}

	if !session.Client.IsLoggedIn() {
		errorResponse(w, http.StatusBadRequest, "not logged in")
		return
	}

	jid, err := types.ParseJID(req.ChatJID)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid jid")
		return
	}

	msg := &waE2E.Message{
		LocationMessage: &waE2E.LocationMessage{
			DegreesLatitude:  proto.Float64(req.Latitude),
			DegreesLongitude: proto.Float64(req.Longitude),
			Name:             proto.String(req.Name),
			Address:          proto.String(req.Address),
		},
	}

	resp, err := session.Client.SendMessage(context.Background(), jid, msg)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonResponse(w, map[string]interface{}{
		"id":        resp.ID,
		"timestamp": resp.Timestamp.Unix(),
	})
}

type GroupInfoPayload struct {
	JID          string              `json:"jid"`
	Name         string              `json:"name"`
	Topic        string              `json:"topic"`
	Created      int64               `json:"created"`
	CreatorJID   string              `json:"creator_jid"`
	Participants []ParticipantInfo   `json:"participants"`
	IsAnnounce   bool                `json:"is_announce"`
	IsLocked     bool                `json:"is_locked"`
}

type ParticipantInfo struct {
	JID     string `json:"jid"`
	IsAdmin bool   `json:"is_admin"`
	IsSuperAdmin bool `json:"is_super_admin"`
}

func getGroupInfoHandler(w http.ResponseWriter, r *http.Request) {
	userID := 0
	fmt.Sscanf(r.URL.Query().Get("user_id"), "%d", &userID)
	if userID == 0 {
		errorResponse(w, http.StatusBadRequest, "user_id required")
		return
	}

	groupJID := r.URL.Query().Get("group_jid")
	if groupJID == "" {
		errorResponse(w, http.StatusBadRequest, "group_jid required")
		return
	}

	session := manager.GetSession(userID)
	if session == nil {
		errorResponse(w, http.StatusNotFound, "session not found")
		return
	}

	if !session.Client.IsLoggedIn() {
		errorResponse(w, http.StatusBadRequest, "not logged in")
		return
	}

	jid, err := types.ParseJID(groupJID)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid jid")
		return
	}

	info, err := session.Client.GetGroupInfo(context.Background(), jid)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to get group info: "+err.Error())
		return
	}

	participants := make([]ParticipantInfo, 0, len(info.Participants))
	for _, p := range info.Participants {
		participants = append(participants, ParticipantInfo{
			JID:          p.JID.String(),
			IsAdmin:      p.IsAdmin,
			IsSuperAdmin: p.IsSuperAdmin,
		})
	}

	payload := GroupInfoPayload{
		JID:          info.JID.String(),
		Name:         info.Name,
		Topic:        info.Topic,
		Created:      info.GroupCreated.Unix(),
		CreatorJID:   info.OwnerJID.String(),
		Participants: participants,
		IsAnnounce:   info.IsAnnounce,
		IsLocked:     info.IsLocked,
	}

	jsonResponse(w, payload)
}

func listGroupParticipantsHandler(w http.ResponseWriter, r *http.Request) {
	userID := 0
	fmt.Sscanf(r.URL.Query().Get("user_id"), "%d", &userID)
	if userID == 0 {
		errorResponse(w, http.StatusBadRequest, "user_id required")
		return
	}

	groupJID := r.URL.Query().Get("group_jid")
	if groupJID == "" {
		errorResponse(w, http.StatusBadRequest, "group_jid required")
		return
	}

	session := manager.GetSession(userID)
	if session == nil {
		errorResponse(w, http.StatusNotFound, "session not found")
		return
	}

	if !session.Client.IsLoggedIn() {
		errorResponse(w, http.StatusBadRequest, "not logged in")
		return
	}

	jid, err := types.ParseJID(groupJID)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid jid")
		return
	}

	info, err := session.Client.GetGroupInfo(context.Background(), jid)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to get group info: "+err.Error())
		return
	}

	participants := make([]ParticipantInfo, 0, len(info.Participants))
	for _, p := range info.Participants {
		participants = append(participants, ParticipantInfo{
			JID:          p.JID.String(),
			IsAdmin:      p.IsAdmin,
			IsSuperAdmin: p.IsSuperAdmin,
		})
	}

	jsonResponse(w, participants)
}

func downloadMediaHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req struct {
		UserID        int    `json:"user_id"`
		URL           string `json:"url"`
		DirectPath    string `json:"direct_path"`
		MediaKey      []byte `json:"media_key"`
		FileEncSHA256 []byte `json:"file_enc_sha256"`
		FileSHA256    []byte `json:"file_sha256"`
		FileLength    uint64 `json:"file_length"`
		MimeType      string `json:"mime_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid json")
		return
	}

	session := manager.GetSession(req.UserID)
	if session == nil {
		errorResponse(w, http.StatusNotFound, "session not found")
		return
	}

	if !session.Client.IsLoggedIn() {
		errorResponse(w, http.StatusBadRequest, "not logged in")
		return
	}

	// Build a minimal ImageMessage to use for download
	imgMsg := &waE2E.ImageMessage{
		URL:           proto.String(req.URL),
		DirectPath:    proto.String(req.DirectPath),
		MediaKey:      req.MediaKey,
		FileEncSHA256: req.FileEncSHA256,
		FileSHA256:    req.FileSHA256,
		FileLength:    proto.Uint64(req.FileLength),
		Mimetype:      proto.String(req.MimeType),
	}

	// Download the media
	data, err := session.Client.Download(context.Background(), imgMsg)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to download: "+err.Error())
		return
	}

	// Return as base64
	jsonResponse(w, map[string]interface{}{
		"data":      base64.StdEncoding.EncodeToString(data),
		"mime_type": req.MimeType,
		"size":      len(data),
	})
}

func main() {
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "/data/whatsapp"
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8090"
	}

	joBotURL := os.Getenv("JO_BOT_URL")
	encryptKey := os.Getenv("WHATSAPP_SESSION_KEY")

	manager = NewSessionManager(dataDir, joBotURL, encryptKey)

	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/sessions", createSessionHandler)
	http.HandleFunc("/sessions/qr", getQRHandler)
	http.HandleFunc("/sessions/status", getStatusHandler)
	http.HandleFunc("/sessions/delete", deleteSessionHandler)
	http.HandleFunc("/sessions/save", saveSessionHandler)
	http.HandleFunc("/chats", getChatsHandler)
	http.HandleFunc("/groups/info", getGroupInfoHandler)
	http.HandleFunc("/groups/participants", listGroupParticipantsHandler)
	http.HandleFunc("/messages/send", sendMessageHandler)
	http.HandleFunc("/messages/typing", setTypingHandler)
	http.HandleFunc("/messages/react", sendReactionHandler)
	http.HandleFunc("/messages/image", sendImageHandler)
	http.HandleFunc("/messages/location", sendLocationHandler)
	http.HandleFunc("/media/download", downloadMediaHandler)
	http.HandleFunc("/events", eventsHandler)

	log.Printf("🚀 WhatsApp server starting on port %s", port)
	log.Printf("📁 Data directory: %s", dataDir)
	if joBotURL != "" {
		log.Printf("🔗 Jo Bot URL: %s", joBotURL)
	}
	if encryptKey != "" {
		log.Printf("🔐 Session persistence enabled")
	}

	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}
