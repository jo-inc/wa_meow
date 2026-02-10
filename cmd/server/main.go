package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waMmsRetry"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// baileysTransport wraps http.RoundTripper to mimic Baileys HTTP behavior
// Baileys only sends Origin header, not Referer - and no User-Agent
type baileysTransport struct {
	base http.RoundTripper
}

func (t *baileysTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Remove Referer header (Baileys doesn't send it)
	req.Header.Del("Referer")
	// Remove User-Agent (Baileys doesn't send it for media downloads)
	req.Header.Del("User-Agent")
	// Log what we're actually sending
	log.Printf("[media/http] Request: %s, Headers: Origin=%s", req.URL.Host, req.Header.Get("Origin"))
	return t.base.RoundTrip(req)
}

type SessionManager struct {
	sessions   map[int]*UserSession
	mu         sync.RWMutex
	dataDir    string
	joBotURL   string
	encryptKey []byte
}

// PendingMediaRetry stores info needed to complete a media retry download
type PendingMediaRetry struct {
	AudioMsg  *waE2E.AudioMessage
	MediaKey  []byte
	MessageID string
	IsPTT     bool
}

type UserSession struct {
	UserID     int
	Client     WhatsAppClient
	Container  *sqlstore.Container
	DBPath     string
	LastUsed   time.Time
	QRChannel  chan string
	LoginDone  chan bool
	EventChan  chan MessageEvent
	MediaCache map[string][]byte // Cache downloaded media by message ID
	MediaMu    sync.RWMutex
	// Pending media retries: message ID -> pending retry info
	PendingRetries   map[string]*PendingMediaRetry
	PendingRetriesMu sync.RWMutex
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
	// Media download info
	MediaKey      []byte `json:"media_key,omitempty"`
	DirectPath    string `json:"direct_path,omitempty"`
	FileEncSHA256 []byte `json:"file_enc_sha256,omitempty"`
	FileSHA256    []byte `json:"file_sha256,omitempty"`
	FileLength    uint64 `json:"file_length,omitempty"`
	IsPTT         bool   `json:"is_ptt,omitempty"` // Push-to-talk (voice note) - critical for download
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
	
	// Configure a custom HTTP client for media downloads that mimics Baileys:
	// 1. Remove Referer header (Baileys doesn't send it)
	// 2. Force HTTP/1.1 to avoid potential HTTP/2 fingerprinting issues
	// 3. TLSNextProto=empty map disables HTTP/2 negotiation
	baseTransport := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(authority string, c *tls.Conn) http.RoundTripper{},
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
	}
	customTransport := &baileysTransport{
		base: baseTransport,
	}
	rawClient.SetMediaHTTPClient(&http.Client{
		Transport: customTransport,
		Timeout:   60 * time.Second,
	})
	
	client := newRealClientWrapper(rawClient)

	session := &UserSession{
		UserID:         userID,
		Client:         client,
		Container:      container,
		DBPath:         dbPath,
		LastUsed:       time.Now(),
		QRChannel:      make(chan string, 10),
		LoginDone:      make(chan bool, 1),
		EventChan:      make(chan MessageEvent, 100),
		MediaCache:     make(map[string][]byte),
		PendingRetries: make(map[string]*PendingMediaRetry),
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
			
			// Test: download image immediately to compare with PTT download
			go func(msgID string, imgMsg *waE2E.ImageMessage) {
				data, err := s.Client.Download(context.Background(), imgMsg)
				if err != nil {
					log.Printf("[media/cache] Failed to download image %s: %v", msgID, err)
					return
				}
				s.MediaMu.Lock()
				s.MediaCache[msgID] = data
				s.MediaMu.Unlock()
				log.Printf("[media/cache] Cached image %s: %d bytes", msgID, len(data))
			}(v.Info.ID, img)
			
			hasContent = true
		}

		// Handle audio/voice messages (ptt = push-to-talk/voice note)
		if audio := v.Message.AudioMessage; audio != nil {
			payload.IsPTT = audio.GetPTT()
			if payload.IsPTT {
				payload.MediaType = "ptt"
			} else {
				payload.MediaType = "audio"
			}
			if audio.Mimetype != nil {
				payload.MimeType = *audio.Mimetype
			}
			if audio.URL != nil {
				payload.MediaURL = *audio.URL
			}
			if audio.DirectPath != nil {
				payload.DirectPath = *audio.DirectPath
			}
			payload.MediaKey = audio.MediaKey
			payload.FileEncSHA256 = audio.FileEncSHA256
			payload.FileSHA256 = audio.FileSHA256
			if audio.FileLength != nil {
				payload.FileLength = *audio.FileLength
			}
			
			// Download audio with retry loop for desktop-originated messages
			// Desktop (web) messages may arrive before media upload is complete (mediaStage != RESOLVED)
			// We retry with delays to wait for CDN, then fall back to MediaRetry for phone re-upload
			go func(msgID string, audioMsg *waE2E.AudioMessage, isPTT bool, msgInfo *types.MessageInfo) {
				// Log download parameters for debugging
				log.Printf("[media/cache] Audio download params for %s (ptt=%v): directPath=%s, mediaKeyLen=%d, encSHA256Len=%d, sha256Len=%d, fileLen=%d, url=%s",
					msgID, isPTT, audioMsg.GetDirectPath(), len(audioMsg.GetMediaKey()),
					len(audioMsg.GetFileEncSHA256()), len(audioMsg.GetFileSHA256()), audioMsg.GetFileLength(), audioMsg.GetURL())

				// Check if media is "resolved" - has the required fields for download
				// Analogous to whatsapp-web.js mediaStage === 'RESOLVED'
				isResolved := func() bool {
					hasPath := audioMsg.GetDirectPath() != "" || audioMsg.GetURL() != ""
					hasKey := len(audioMsg.GetMediaKey()) > 0
					hasHash := len(audioMsg.GetFileEncSHA256()) > 0
					return hasPath && hasKey && hasHash
				}

				var data []byte
				var err error

				// Retry loop: desktop messages may not be uploaded yet when event arrives
				// Wait up to ~12 seconds total for media to be resolved and available on CDN
				retryDelays := []time.Duration{0, 2 * time.Second, 3 * time.Second, 4 * time.Second, 3 * time.Second}
				for attempt, delay := range retryDelays {
					if delay > 0 {
						log.Printf("[media/cache] PTT %s: retry %d/%d after %v", msgID, attempt, len(retryDelays)-1, delay)
						time.Sleep(delay)
					}

					// Check if media is resolved before attempting download
					if !isResolved() {
						log.Printf("[media/cache] Audio %s attempt %d: media not resolved (missing directPath/mediaKey/hash)", msgID, attempt+1)
						continue
					}

					data, err = s.Client.Download(context.Background(), audioMsg)
					if err != nil {
						log.Printf("[media/cache] Audio %s attempt %d: Download error: %v", msgID, attempt+1, err)
						continue
					}

					if len(data) > 0 {
						log.Printf("[media/cache] Audio %s attempt %d: success, %d bytes", msgID, attempt+1, len(data))
						break
					}

					log.Printf("[media/cache] Audio %s attempt %d: 0 bytes (CDN not ready)", msgID, attempt+1)

					// On first 0-byte response, proactively send MediaRetryReceipt
					// This may trigger desktop/phone to complete/retry the upload
					if attempt == 0 && isPTT && msgInfo != nil {
						log.Printf("[media/retry] PTT %s: sending early MediaRetryReceipt to trigger re-upload", msgID)
						if retryErr := s.Client.SendMediaRetryReceipt(context.Background(), msgInfo, audioMsg.GetMediaKey()); retryErr != nil {
							log.Printf("[media/retry] Early MediaRetryReceipt failed for %s: %v", msgID, retryErr)
						}
					}
				}

				if len(data) > 0 {
					s.MediaMu.Lock()
					s.MediaCache[msgID] = data
					s.MediaMu.Unlock()
					log.Printf("[media/cache] Cached audio %s: %d bytes (ptt=%v)", msgID, len(data), isPTT)
					return
				}

				// All retries failed - for PTT, try MediaRetry as last resort (asks phone to re-upload)
				// This works for phone-originated messages but may not help desktop-originated ones
				if isPTT && msgInfo != nil {
					log.Printf("[media/retry] PTT %s: all download attempts failed, sending MediaRetryReceipt to phone", msgID)

					// Store pending retry info for when we receive events.MediaRetry
					s.PendingRetriesMu.Lock()
					s.PendingRetries[msgID] = &PendingMediaRetry{
						AudioMsg:  audioMsg,
						MediaKey:  audioMsg.GetMediaKey(),
						MessageID: msgID,
						IsPTT:     isPTT,
					}
					s.PendingRetriesMu.Unlock()

					if retryErr := s.Client.SendMediaRetryReceipt(context.Background(), msgInfo, audioMsg.GetMediaKey()); retryErr != nil {
						log.Printf("[media/retry] MediaRetryReceipt failed for %s: %v", msgID, retryErr)
						// Clean up pending retry on failure
						s.PendingRetriesMu.Lock()
						delete(s.PendingRetries, msgID)
						s.PendingRetriesMu.Unlock()
					} else {
						log.Printf("[media/retry] PTT %s: MediaRetryReceipt sent, waiting for events.MediaRetry response", msgID)
					}
				} else {
					log.Printf("[media/cache] WARNING: Audio %s download failed after all retries, 0 bytes (ptt=%v)", msgID, isPTT)
				}
			}(v.Info.ID, audio, payload.IsPTT, &v.Info)
			
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

	case *events.MediaRetry:
		// Handle MediaRetry response from phone after SendMediaRetryReceipt
		// This contains a new DirectPath for downloading media that was re-uploaded
		s.handleMediaRetry(v)
	}
}

// handleMediaRetry processes the events.MediaRetry response after we sent SendMediaRetryReceipt
// It decrypts the notification to get the new DirectPath and downloads the media
func (s *UserSession) handleMediaRetry(evt *events.MediaRetry) {
	msgID := string(evt.MessageID)
	log.Printf("[media/retry] Received MediaRetry event for message %s (chat=%s, fromMe=%v)",
		msgID, evt.ChatID.String(), evt.FromMe)

	// Look up pending retry
	s.PendingRetriesMu.RLock()
	pending, ok := s.PendingRetries[msgID]
	s.PendingRetriesMu.RUnlock()

	if !ok {
		log.Printf("[media/retry] No pending retry found for message %s, ignoring", msgID)
		return
	}

	// Clean up pending retry (we'll only try once)
	defer func() {
		s.PendingRetriesMu.Lock()
		delete(s.PendingRetries, msgID)
		s.PendingRetriesMu.Unlock()
	}()

	// Decrypt the notification to get the new DirectPath
	retryData, err := whatsmeow.DecryptMediaRetryNotification(evt, pending.MediaKey)
	if err != nil {
		log.Printf("[media/retry] Failed to decrypt MediaRetry notification for %s: %v", msgID, err)
		return
	}

	// Check result
	if retryData.GetResult() != waMmsRetry.MediaRetryNotification_SUCCESS {
		log.Printf("[media/retry] MediaRetry failed for %s: result=%v", msgID, retryData.GetResult())
		return
	}

	newDirectPath := retryData.GetDirectPath()
	if newDirectPath == "" {
		log.Printf("[media/retry] MediaRetry for %s succeeded but no DirectPath in response", msgID)
		return
	}

	log.Printf("[media/retry] Got new DirectPath for %s: %s", msgID, newDirectPath)

	// Download using the new DirectPath
	data, err := s.Client.DownloadMediaWithPath(
		context.Background(),
		newDirectPath,
		pending.AudioMsg.GetFileEncSHA256(),
		pending.AudioMsg.GetFileSHA256(),
		pending.MediaKey,
		-1,
		whatsmeow.MediaAudio,
		"audio",
	)

	if err != nil {
		log.Printf("[media/retry] Download with new DirectPath failed for %s: %v", msgID, err)
		return
	}

	if len(data) == 0 {
		log.Printf("[media/retry] Download with new DirectPath returned 0 bytes for %s", msgID)
		return
	}

	// Cache the downloaded media
	s.MediaMu.Lock()
	s.MediaCache[msgID] = data
	s.MediaMu.Unlock()
	log.Printf("[media/retry] SUCCESS: Cached audio %s: %d bytes (ptt=%v) via MediaRetry", msgID, len(data), pending.IsPTT)
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
		if err != nil && !strings.Contains(err.Error(), "already connected") {
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
		if err != nil && !strings.Contains(err.Error(), "already connected") {
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

func sendAudioHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req struct {
		UserID     int    `json:"user_id"`
		ChatJID    string `json:"chat_jid"`
		AudioB64   string `json:"audio_b64"`   // Base64 encoded audio
		MimeType   string `json:"mime_type"`   // e.g. "audio/ogg; codecs=opus"
		PTT        bool   `json:"ptt"`         // Push-to-talk (voice note mode)
		Seconds    uint32 `json:"seconds"`     // Duration in seconds
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

	// Decode base64 audio
	audioData, err := base64.StdEncoding.DecodeString(req.AudioB64)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid base64 audio")
		return
	}

	// Upload to WhatsApp servers
	uploaded, err := session.Client.Upload(context.Background(), audioData, whatsmeow.MediaAudio)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to upload audio: "+err.Error())
		return
	}

	// Build and send audio message
	audioMsg := &waE2E.AudioMessage{
		URL:           proto.String(uploaded.URL),
		DirectPath:    proto.String(uploaded.DirectPath),
		MediaKey:      uploaded.MediaKey,
		Mimetype:      proto.String(req.MimeType),
		FileEncSHA256: uploaded.FileEncSHA256,
		FileSHA256:    uploaded.FileSHA256,
		FileLength:    proto.Uint64(uint64(len(audioData))),
		PTT:           proto.Bool(req.PTT),
	}
	
	// Set duration if provided
	if req.Seconds > 0 {
		audioMsg.Seconds = proto.Uint32(req.Seconds)
	}
	
	msg := &waE2E.Message{
		AudioMessage: audioMsg,
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
		MessageID     string `json:"message_id"` // For cache lookup
		URL           string `json:"url"`
		DirectPath    string `json:"direct_path"`
		MediaKey      []byte `json:"media_key"`
		FileEncSHA256 []byte `json:"file_enc_sha256"`
		FileSHA256    []byte `json:"file_sha256"`
		FileLength    uint64 `json:"file_length"`
		MimeType      string `json:"mime_type"`
		IsPTT         bool   `json:"is_ptt"`
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

	// Check cache first (media downloaded immediately on receive)
	if req.MessageID != "" {
		session.MediaMu.RLock()
		cachedData, found := session.MediaCache[req.MessageID]
		session.MediaMu.RUnlock()
		if found {
			log.Printf("[media/download] Cache hit for %s: %d bytes", req.MessageID, len(cachedData))
			// Remove from cache after serving
			session.MediaMu.Lock()
			delete(session.MediaCache, req.MessageID)
			session.MediaMu.Unlock()
			jsonResponse(w, map[string]interface{}{
				"data":      base64.StdEncoding.EncodeToString(cachedData),
				"mime_type": req.MimeType,
				"size":      len(cachedData),
			})
			return
		}
		log.Printf("[media/download] Cache miss for %s, trying direct download", req.MessageID)
	}

	// Fallback: try to reconstruct and download
	// Use DownloadMediaWithPath which internally refreshes mediaConn for fresh auth tokens
	log.Printf("[media/download] Downloading %s (ptt=%v) for user %d, fileLen=%d", 
		req.MimeType, req.IsPTT, req.UserID, req.FileLength)
	
	var data []byte
	var err error
	
	// Determine media type and mmsType based on mime
	// Note: PTT uses mmsType="audio" same as regular audio (Baileys has no 'ptt' in MEDIA_PATH_MAP)
	var mediaType whatsmeow.MediaType
	var mmsType string
	if strings.HasPrefix(req.MimeType, "audio/") {
		mediaType = whatsmeow.MediaAudio
		mmsType = "audio" // PTT and regular audio both use "audio"
	} else if strings.HasPrefix(req.MimeType, "video/") {
		mediaType = whatsmeow.MediaVideo
		mmsType = "video"
	} else if strings.HasPrefix(req.MimeType, "image/") {
		mediaType = whatsmeow.MediaImage
		mmsType = "image"
	} else {
		mediaType = whatsmeow.MediaDocument
		mmsType = "document"
	}
	
	// Retry with exponential backoff - CDN returns 26-byte empty stub for stale auth
	maxRetries := 4
	backoffs := []time.Duration{500 * time.Millisecond, 1 * time.Second, 2 * time.Second, 4 * time.Second}
	
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := backoffs[attempt-1]
			log.Printf("[media/download] Retry %d/%d after %v", attempt, maxRetries, backoff)
			time.Sleep(backoff)
		}
		
		data, err = session.Client.DownloadMediaWithPath(
			context.Background(),
			req.DirectPath,
			req.FileEncSHA256,
			req.FileSHA256,
			req.MediaKey,
			-1,
			mediaType,
			mmsType,
		)
		
		log.Printf("[media/download] Attempt %d: dataLen=%d, err=%v", attempt+1, len(data), err)
		
		if err != nil {
			continue
		}
		
		if len(data) > 0 {
			break
		}
		
		log.Printf("[media/download] Attempt %d: got 0 bytes (stale auth, will retry)", attempt+1)
	}
	
	if err != nil {
		log.Printf("[media/download] All attempts failed: %v", err)
		errorResponse(w, http.StatusInternalServerError, "failed to download: "+err.Error())
		return
	}
	if len(data) == 0 {
		log.Printf("[media/download] All attempts returned 0 bytes")
		errorResponse(w, http.StatusInternalServerError, "media download returned empty content after retries")
		return
	}
	log.Printf("[media/download] Success: %d bytes", len(data))

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
	http.HandleFunc("/messages/audio", sendAudioHandler)
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
