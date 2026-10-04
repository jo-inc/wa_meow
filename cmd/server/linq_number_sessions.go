package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	"rsc.io/qr"
)

const (
	linqNumberRegistryFile = "linq_numbers.json"
	pairingTokenTTL        = 30 * time.Minute
	minAdminTokenLength    = 32
)

type linqNumberRecord struct {
	ID    int    `json:"id"`
	Phone string `json:"phone"`
}

type numberSessionState struct {
	linqNumberRecord
	session            *UserSession
	pairingStarted     bool
	eventsStreamActive bool
	pairingToken       [32]byte
	tokenExpiresAt     time.Time
}

func (m *SessionManager) loadNumberRegistry() {
	path := filepath.Join(m.dataDir, linqNumberRegistryFile)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		m.numberRegistryErr = fmt.Errorf("read number registry")
		return
	}
	var records []linqNumberRecord
	if err := json.Unmarshal(data, &records); err != nil {
		m.numberRegistryErr = fmt.Errorf("decode number registry")
		return
	}
	seenPhones := make(map[string]bool, len(records))
	for _, record := range records {
		phone, ok := normalizeE164(record.Phone)
		if record.ID <= 0 || !ok || seenPhones[phone] || m.numberSessions[record.ID] != nil {
			m.numberRegistryErr = fmt.Errorf("invalid number registry")
			m.numberSessions = make(map[int]*numberSessionState)
			return
		}
		seenPhones[phone] = true
		m.numberSessions[record.ID] = &numberSessionState{linqNumberRecord: linqNumberRecord{ID: record.ID, Phone: phone}}
	}
}

func (m *SessionManager) saveNumberRegistryLocked() error {
	records := make([]linqNumberRecord, 0, len(m.numberSessions))
	for _, state := range m.numberSessions {
		records = append(records, state.linqNumberRecord)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(m.dataDir, linqNumberRegistryFile)
	tmp, err := os.CreateTemp(m.dataDir, ".linq-numbers-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func normalizeE164(raw string) (string, bool) {
	value := strings.TrimSpace(raw)
	if !strings.HasPrefix(value, "+") {
		return "", false
	}
	var digits strings.Builder
	for _, char := range value[1:] {
		if char >= '0' && char <= '9' {
			digits.WriteRune(char)
			continue
		}
		switch char {
		case '-', ' ', '(', ')':
		default:
			return "", false
		}
	}
	if digits.Len() < 7 || digits.Len() > 15 {
		return "", false
	}
	return "+" + digits.String(), true
}

func (m *SessionManager) registerLinqNumber(record linqNumberRecord) (string, error) {
	phone, ok := normalizeE164(record.Phone)
	if record.ID <= 0 || !ok {
		return "", fmt.Errorf("number id and E.164 phone required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.numberRegistryErr != nil {
		return "", m.numberRegistryErr
	}
	if existing := m.numberSessions[record.ID]; existing != nil && existing.Phone != phone {
		return "", fmt.Errorf("registered phone cannot be changed")
	}
	for id, existing := range m.numberSessions {
		if id != record.ID && existing.Phone == phone {
			return "", fmt.Errorf("phone is already registered to another number id")
		}
	}
	if m.numberSessions[record.ID] == nil {
		m.numberSessions[record.ID] = &numberSessionState{linqNumberRecord: linqNumberRecord{ID: record.ID, Phone: phone}}
		if err := m.saveNumberRegistryLocked(); err != nil {
			delete(m.numberSessions, record.ID)
			return "", fmt.Errorf("persist registration")
		}
	}
	var token [32]byte
	if _, err := io.ReadFull(rand.Reader, token[:]); err != nil {
		return "", fmt.Errorf("create pairing token")
	}
	state := m.numberSessions[record.ID]
	state.pairingToken = sha256.Sum256(token[:])
	state.tokenExpiresAt = time.Now().Add(pairingTokenTTL)
	return base64.RawURLEncoding.EncodeToString(token[:]), nil
}

func (m *SessionManager) authorizePairingToken(numberID int, encoded string) *numberSessionState {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) != 32 {
		return nil
	}
	got := sha256.Sum256(raw)
	m.mu.RLock()
	defer m.mu.RUnlock()
	state := m.numberSessions[numberID]
	if state == nil || time.Now().After(state.tokenExpiresAt) || subtle.ConstantTimeCompare(got[:], state.pairingToken[:]) != 1 {
		return nil
	}
	return state
}

func (m *SessionManager) getOrCreateLinqNumberSession(numberID int) (*numberSessionState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.numberSessions[numberID]
	if state == nil {
		return nil, fmt.Errorf("number not registered")
	}
	if state.session != nil {
		return state, nil
	}
	dbPath := filepath.Join(m.dataDir, fmt.Sprintf("linq_number_%d.db", numberID))
	dbFile, err := os.OpenFile(dbPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("create number session file")
	}
	if err := dbFile.Chmod(0600); err != nil {
		_ = dbFile.Close()
		return nil, fmt.Errorf("protect number session file")
	}
	if err := dbFile.Close(); err != nil {
		return nil, fmt.Errorf("close number session file")
	}
	ctx := context.Background()
	container, err := sqlstore.New(ctx, "sqlite3", "file:"+dbPath+"?_foreign_keys=on", waLog.Stdout("Database", "ERROR", true))
	if err != nil {
		return nil, fmt.Errorf("create number session store")
	}
	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		_ = container.Close()
		return nil, fmt.Errorf("load number session device")
	}
	clientLog := waLog.Stdout("Client", "ERROR", true)
	clientTelemetry := &clientTelemetry{}
	rawClient := whatsmeow.NewClient(deviceStore, newTelemetryLogger(clientLog, clientTelemetry))
	configureDirectChatClient(rawClient)
	// Retain decrypted events and withhold provider receipts until our durable
	// handler succeeds; this also makes retries safe after ratchet advancement.
	rawClient.SynchronousAck = true
	rawClient.EnableDecryptedEventBuffer = true
	rawClient.SetMediaHTTPClient(sharedMediaHTTPClient)
	session := &UserSession{
		Kind:           LinqNumberSessionKind,
		LinqNumberID:   numberID,
		Client:         newRealClientWrapper(rawClient),
		Container:      container,
		DBPath:         dbPath,
		LastUsed:       time.Now(),
		QRChannel:      make(chan string, 10),
		LoginDone:      make(chan bool, 1),
		EventChan:      make(chan MessageEvent, 100),
		Inbox:          &sharedInboxStore{manager: m},
		MediaCache:     make(map[string]*mediaCacheEntry),
		PendingRetries: make(map[string]*PendingMediaRetry),
	}
	session.startMediaCacheEvictor()
	clientTelemetry.session = session
	rawClient.AddEventHandlerWithSuccessStatus(session.handleNumberEvent)
	state.session = session
	activeSessions.Inc()
	sessionLifecycleTotal.WithLabelValues("created").Inc()
	return state, nil
}

func (m *SessionManager) startNumberPairing(state *numberSessionState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state.session == nil {
		return fmt.Errorf("number session unavailable")
	}
	session := state.session
	if session.Client.IsLoggedIn() || state.pairingStarted {
		return nil
	}
	if session.Client.IsConnected() {
		session.Client.Disconnect()
	}
	for {
		select {
		case <-session.QRChannel:
		default:
			goto drained
		}
	}
drained:
	select {
	case <-session.LoginDone:
	default:
	}
	qrChan, err := session.Client.GetQRChannel(context.Background())
	if err != nil {
		return fmt.Errorf("create pairing QR")
	}
	if err := session.Client.Connect(); err != nil && !strings.Contains(err.Error(), "already connected") {
		return fmt.Errorf("connect number session")
	}
	state.pairingStarted = true
	go func() {
		for event := range qrChan {
			if event.Event == "code" {
				select {
				case session.QRChannel <- event.Code:
				default:
				}
			} else if event.Event == "success" {
				select {
				case session.LoginDone <- true:
				default:
				}
				return
			}
		}
	}()
	return nil
}

func (m *SessionManager) accountPhone(state *numberSessionState) (string, bool) {
	m.mu.RLock()
	session := state.session
	m.mu.RUnlock()
	if session == nil {
		return "", false
	}
	id := session.Client.GetStore().GetID()
	if id == nil {
		return "", false
	}
	phone, ok := normalizeE164("+" + id.User)
	return phone, ok
}

func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if len(manager.adminToken) < minAdminTokenLength {
		errorResponse(w, http.StatusServiceUnavailable, "admin registration is disabled")
		return false
	}
	authorization := r.Header.Get("Authorization")
	provided := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	if provided == authorization || subtle.ConstantTimeCompare([]byte(provided), []byte(manager.adminToken)) != 1 {
		errorResponse(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

func requireInternal(w http.ResponseWriter, r *http.Request) bool {
	token := manager.joBotInternalToken
	if token == "" {
		errorResponse(w, http.StatusServiceUnavailable, "internal routing is disabled")
		return false
	}
	provided := r.Header.Get("X-WhatsApp-Internal-Token")
	if provided == "" {
		errorResponse(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	// Compare fixed-length digests so an attacker cannot learn the configured
	// token length or matching prefix from this check.
	expectedDigest := sha256.Sum256([]byte(token))
	providedDigest := sha256.Sum256([]byte(provided))
	if subtle.ConstantTimeCompare(providedDigest[:], expectedDigest[:]) != 1 {
		errorResponse(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

func senderAltJID(jid types.JID) string {
	if jid.IsEmpty() {
		return ""
	}
	return jid.String()
}

func (m *SessionManager) verifiedConnectedNumberSession(numberID int) (*numberSessionState, *UserSession, bool) {
	m.mu.RLock()
	state := m.numberSessions[numberID]
	var session *UserSession
	if state != nil {
		session = state.session
	}
	m.mu.RUnlock()
	if state == nil || session == nil || session.Kind != LinqNumberSessionKind || session.LinqNumberID != numberID || !session.Client.IsConnected() || !session.Client.IsLoggedIn() {
		return nil, nil, false
	}
	actualPhone, hasIdentity := m.accountPhone(state)
	if !hasIdentity || actualPhone != state.Phone {
		return nil, nil, false
	}
	return state, session, true
}

func linqNumberInternalHandler(w http.ResponseWriter, r *http.Request) {
	if !requireInternal(w, r) {
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 || parts[0] != "internal" || parts[1] != "linq-numbers" {
		errorResponse(w, http.StatusNotFound, "not found")
		return
	}
	numberID, err := strconv.Atoi(parts[2])
	if err != nil || numberID <= 0 {
		errorResponse(w, http.StatusNotFound, "not found")
		return
	}
	switch {
	case r.Method == http.MethodGet && len(parts) == 4 && parts[3] == "events":
		streamLinqNumberEvents(w, r, numberID)
	case r.Method == http.MethodGet && len(parts) == 5 && parts[3] == "events" && parts[4] == "status":
		statusLinqNumberEventsHandler(w, numberID)
	case r.Method == http.MethodPost && len(parts) == 5 && parts[3] == "events" && parts[4] == "checkpoint":
		checkpointLinqNumberEventHandler(w, r, numberID)
	case r.Method == http.MethodPost && len(parts) == 5 && parts[3] == "events" && parts[4] == "ack":
		ackLinqNumberEventHandler(w, r, numberID)
	case r.Method == http.MethodPost && len(parts) == 5 && parts[3] == "messages" && parts[4] == "typing":
		sendInternalLinqNumberTypingHandler(w, r, numberID)
	case r.Method == http.MethodPost && len(parts) == 5 && parts[3] == "messages" && parts[4] == "react":
		sendInternalLinqNumberReactionHandler(w, r, numberID)
	case r.Method == http.MethodPost && len(parts) == 5 && parts[3] == "messages" && parts[4] == "send":
		sendInternalLinqNumberTextHandler(w, r, numberID)
	case r.Method == http.MethodPost && len(parts) == 5 && parts[3] == "messages" && parts[4] == "image":
		sendInternalLinqNumberMediaHandler(w, r, numberID, linqNumberMediaImage)
	case r.Method == http.MethodPost && len(parts) == 5 && parts[3] == "messages" && parts[4] == "document":
		sendInternalLinqNumberMediaHandler(w, r, numberID, linqNumberMediaDocument)
	case r.Method == http.MethodPost && len(parts) == 5 && parts[3] == "messages" && parts[4] == "audio":
		sendInternalLinqNumberMediaHandler(w, r, numberID, linqNumberMediaAudio)
	case r.Method == http.MethodPost && len(parts) == 5 && parts[3] == "media" && parts[4] == "download":
		downloadInternalLinqNumberMediaHandler(w, r, numberID)
	default:
		errorResponse(w, http.StatusNotFound, "not found")
	}
}

func streamLinqNumberEvents(w http.ResponseWriter, r *http.Request, numberID int) {
	state, err := manager.getOrCreateLinqNumberSession(numberID)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "number session unavailable")
		return
	}
	session := state.session
	if session == nil || session.Kind != LinqNumberSessionKind || session.LinqNumberID != numberID {
		errorResponse(w, http.StatusConflict, "registered number account is not connected and verified")
		return
	}
	actualPhone, hasIdentity := manager.accountPhone(state)
	// A saved paired device is not logged in until Connect completes after a
	// process restart. Verify its stored identity before connecting; require
	// live login and identity verification again before admitting the stream.
	if !hasIdentity || actualPhone != state.Phone {
		errorResponse(w, http.StatusConflict, "registered number account is not connected and verified")
		return
	}
	if !session.Client.IsConnected() {
		if err := session.Client.Connect(); err != nil && !strings.Contains(err.Error(), "already connected") {
			errorResponse(w, http.StatusServiceUnavailable, "number session connection failed")
			return
		}
	}
	if !waitForNumberConnection(session, manager.connectReadyWait, manager.connectReadyPoll) {
		errorResponse(w, http.StatusServiceUnavailable, "number session is not connected")
		return
	}
	if _, _, ok := manager.verifiedConnectedNumberSession(numberID); !ok {
		errorResponse(w, http.StatusConflict, "registered number account is not connected and verified")
		return
	}
	manager.mu.Lock()
	if state.eventsStreamActive {
		manager.mu.Unlock()
		errorResponse(w, http.StatusConflict, "number event stream is already active")
		return
	}
	state.eventsStreamActive = true
	manager.mu.Unlock()
	defer func() {
		manager.mu.Lock()
		state.eventsStreamActive = false
		manager.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		errorResponse(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	store := &sharedInboxStore{manager: manager}
	entries, err := store.references(numberID, nil)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "shared inbox unavailable")
		return
	}
	// Complete the authenticated SSE handshake even when the inbox is empty.
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	seen := make(map[string]bool, len(entries))
	writeEvent := func(event MessageEvent) bool {
		data, err := json.Marshal(event)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	writeStored := func(id string) bool {
		entry, err := store.byInboxID(numberID, id)
		if err == errInboxNotFound {
			return true
		}
		if err != nil {
			return false
		}
		seen[id] = true
		return writeEvent(entry.Event)
	}
	for _, entry := range entries {
		if !writeStored(entry.InboxID) {
			return
		}
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case event := <-session.EventChan:
			if event.InboxID != "" {
				if seen[event.InboxID] {
					continue
				}
				if !writeStored(event.InboxID) {
					return
				}
			} else if !writeEvent(event) {
				return
			}
		case <-ticker.C:
			// EventChan is a lossy notification only. Re-scan durable entries while
			// this stream is active, but never re-emit one already sent on it.
			entries, err := store.references(numberID, seen)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if !writeStored(entry.InboxID) {
					return
				}
			}
		case <-r.Context().Done():
			return
		}
	}
}

type linqInboxCheckpointRequest struct {
	InboxID            string          `json:"inbox_id"`
	ProcessingState    string          `json:"processing_state"`
	ResponseCheckpoint json.RawMessage `json:"response_checkpoint"`
	AttemptToken       string          `json:"attempt_token"`
}

type linqInboxAckRequest struct {
	InboxID      string `json:"inbox_id"`
	Outcome      string `json:"outcome"`
	AttemptToken string `json:"attempt_token"`
}

func validInboxID(value string) bool {
	if len(value) != 32 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func decodeInternalJSON(w http.ResponseWriter, r *http.Request, target interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, sharedInboxMaxEventBytes+4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid shared inbox request")
		return false
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		errorResponse(w, http.StatusBadRequest, "invalid shared inbox request")
		return false
	}
	return true
}

func canonicalJSONObject(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var object map[string]interface{}
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, false
	}
	canonical, err := json.Marshal(object)
	return canonical, err == nil && len(canonical) <= sharedInboxMaxEventBytes
}

func registeredLinqNumber(numberID int) bool {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.numberSessions[numberID] != nil && manager.numberRegistryErr == nil
}

// statusLinqNumberEventsHandler exposes reconciliation metadata only: no inbox
// IDs, owner tokens, payloads, checkpoints, or source identifiers.
func statusLinqNumberEventsHandler(w http.ResponseWriter, numberID int) {
	if !registeredLinqNumber(numberID) {
		errorResponse(w, http.StatusNotFound, "number session unavailable")
		return
	}
	status, err := (&sharedInboxStore{manager: manager}).status(numberID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "shared inbox unavailable")
		return
	}
	jsonResponse(w, status)
}

func checkpointLinqNumberEventHandler(w http.ResponseWriter, r *http.Request, numberID int) {
	if !registeredLinqNumber(numberID) {
		errorResponse(w, http.StatusNotFound, "number session unavailable")
		return
	}
	var request linqInboxCheckpointRequest
	if !decodeInternalJSON(w, r, &request) {
		return
	}
	if len(request.AttemptToken) > sharedInboxMaxTokenBytes || !validInboxID(request.InboxID) {
		errorResponse(w, http.StatusBadRequest, "invalid shared inbox request")
		return
	}
	if request.ProcessingState != "started" && request.ProcessingState != "ready" {
		errorResponse(w, http.StatusBadRequest, "invalid shared inbox request")
		return
	}
	if request.ProcessingState == "ready" {
		checkpoint, ok := canonicalJSONObject(request.ResponseCheckpoint)
		if !ok {
			errorResponse(w, http.StatusBadRequest, "ready checkpoint object required")
			return
		}
		request.ResponseCheckpoint = checkpoint
	} else if len(request.ResponseCheckpoint) != 0 {
		errorResponse(w, http.StatusBadRequest, "started checkpoint must be empty")
		return
	}
	entry, err := (&sharedInboxStore{manager: manager}).checkpoint(numberID, request.InboxID, request.ProcessingState, request.AttemptToken, request.ResponseCheckpoint)
	if err == nil {
		jsonResponse(w, entry.Event)
		return
	}
	if errors.Is(err, errInboxNotFound) {
		errorResponse(w, http.StatusNotFound, "shared inbox entry not found")
	} else if errors.Is(err, errInboxConflict) {
		errorResponse(w, http.StatusConflict, "shared inbox state conflict")
	} else {
		errorResponse(w, http.StatusInternalServerError, "shared inbox unavailable")
	}
}

func ackLinqNumberEventHandler(w http.ResponseWriter, r *http.Request, numberID int) {
	if !registeredLinqNumber(numberID) {
		errorResponse(w, http.StatusNotFound, "number session unavailable")
		return
	}
	var request linqInboxAckRequest
	if !decodeInternalJSON(w, r, &request) {
		return
	}
	if len(request.AttemptToken) > sharedInboxMaxTokenBytes || !validInboxID(request.InboxID) {
		errorResponse(w, http.StatusBadRequest, "invalid shared inbox request")
		return
	}
	if request.Outcome != "completed" && request.Outcome != "ignored" && request.Outcome != "superseded" {
		errorResponse(w, http.StatusBadRequest, "invalid shared inbox outcome")
		return
	}
	err := (&sharedInboxStore{manager: manager}).ack(numberID, request.InboxID, request.Outcome, request.AttemptToken)
	if err == nil {
		jsonResponse(w, map[string]string{"status": "acknowledged"})
		return
	}
	if errors.Is(err, errInboxNotFound) {
		errorResponse(w, http.StatusNotFound, "shared inbox entry not found")
	} else if errors.Is(err, errInboxConflict) {
		errorResponse(w, http.StatusConflict, "shared inbox state conflict")
	} else {
		errorResponse(w, http.StatusInternalServerError, "shared inbox unavailable")
	}
}

func linqNumberAdminHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin/linq-numbers" {
		if r.Method != http.MethodPost {
			errorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !requireAdmin(w, r) {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		var request linqNumberRecord
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid registration")
			return
		}
		var extra interface{}
		if err := decoder.Decode(&extra); err != io.EOF {
			errorResponse(w, http.StatusBadRequest, "invalid registration")
			return
		}
		token, err := manager.registerLinqNumber(request)
		if err != nil {
			status := http.StatusBadRequest
			if strings.Contains(err.Error(), "persist") || strings.Contains(err.Error(), "registry") {
				status = http.StatusInternalServerError
			}
			errorResponse(w, status, err.Error())
			return
		}
		jsonResponse(w, map[string]string{
			"status":      "registered",
			"pairing_url": fmt.Sprintf("/admin/linq-numbers/%d/pair?token=%s", request.ID, token),
		})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "admin" || parts[1] != "linq-numbers" {
		errorResponse(w, http.StatusNotFound, "not found")
		return
	}
	numberID, err := strconv.Atoi(parts[2])
	if err != nil || numberID <= 0 {
		errorResponse(w, http.StatusNotFound, "not found")
		return
	}
	if len(parts) == 5 && parts[3] == "messages" && parts[4] == "send" {
		if r.Method != http.MethodPost {
			errorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if requireAdmin(w, r) {
			sendLinqNumberTextHandler(w, r, numberID)
		}
		return
	}
	if r.Method != http.MethodGet || len(parts) != 4 {
		errorResponse(w, http.StatusNotFound, "not found")
		return
	}
	state := manager.authorizePairingToken(numberID, r.URL.Query().Get("token"))
	if state == nil {
		errorResponse(w, http.StatusUnauthorized, "invalid or expired pairing link")
		return
	}
	switch parts[3] {
	case "pair":
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src data:; connect-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, pairingPage)
	case "qr":
		streamNumberQR(w, r, state)
	case "status":
		statusLinqNumberPairing(w, state)
	default:
		errorResponse(w, http.StatusNotFound, "not found")
	}
}

func linqNumberDeliveryReceiptPath(m *SessionManager, numberID int, deliveryKey string) string {
	sum := sha256.Sum256([]byte(deliveryKey))
	return filepath.Join(m.dataDir, "linq_number_delivery_receipts", fmt.Sprintf("%d", numberID), fmt.Sprintf("%x.json", sum))
}

func readLinqNumberDeliveryReceipt(m *SessionManager, numberID int, deliveryKey string) (*deliveryReceipt, error) {
	data, err := os.ReadFile(linqNumberDeliveryReceiptPath(m, numberID, deliveryKey))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var receipt deliveryReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return nil, err
	}
	return &receipt, nil
}

func writeLinqNumberDeliveryReceipt(m *SessionManager, numberID int, deliveryKey string, receipt deliveryReceipt) error {
	path := linqNumberDeliveryReceiptPath(m, numberID, deliveryKey)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".receipt-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func stableLinqNumberMessageID(numberID int, deliveryKey string) types.MessageID {
	sum := sha256.Sum256([]byte(fmt.Sprintf("linq-number:%d:%s", numberID, deliveryKey)))
	return types.MessageID(strings.ToUpper(fmt.Sprintf("%x", sum[:10])))
}

func waitForNumberConnection(session *UserSession, timeout time.Duration, poll time.Duration) bool {
	if session.Client.IsConnected() && session.Client.IsLoggedIn() {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if session.Client.IsConnected() && session.Client.IsLoggedIn() {
				return true
			}
		case <-timer.C:
			return session.Client.IsConnected() && session.Client.IsLoggedIn()
		}
	}
}

type linqNumberTextRequest struct {
	ChatJID     string `json:"chat_jid"`
	Text        string `json:"text"`
	ReplyTo     string `json:"reply_to"`
	DeliveryKey string `json:"delivery_key"`
}

func decodeLinqNumberTextRequest(w http.ResponseWriter, r *http.Request, request *linqNumberTextRequest) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(request); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid message request")
		return false
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		errorResponse(w, http.StatusBadRequest, "invalid message request")
		return false
	}
	if strings.TrimSpace(request.Text) == "" || len(request.Text) > 4096 || strings.TrimSpace(request.DeliveryKey) == "" || len(request.DeliveryKey) > 200 || len(request.ReplyTo) > 512 {
		errorResponse(w, http.StatusBadRequest, "chat jid, text, and delivery key required")
		return false
	}
	return true
}

func linqNumberMessage(text, replyTo string, chatJID types.JID) *waE2E.Message {
	if replyTo == "" {
		return &waE2E.Message{Conversation: proto.String(text)}
	}
	return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: proto.String(text),
		ContextInfo: &waE2E.ContextInfo{
			StanzaID:      proto.String(replyTo),
			Participant:   proto.String(chatJID.String()),
			QuotedMessage: &waE2E.Message{Conversation: proto.String("")},
		},
	}}
}

func sendVerifiedLinqNumberText(w http.ResponseWriter, r *http.Request, numberID int, request linqNumberTextRequest, requireConnected bool) {
	jid, err := parseDirectChatJID(request.ChatJID)
	if err != nil {
		errorResponse(w, http.StatusBadRequest, "direct chat jid required")
		return
	}

	var state *numberSessionState
	var session *UserSession
	if requireConnected {
		state, session, _ = manager.verifiedConnectedNumberSession(numberID)
		if state == nil {
			errorResponse(w, http.StatusConflict, "registered number account is not connected and verified")
			return
		}
	} else {
		state, err = manager.getOrCreateLinqNumberSession(numberID)
		if err != nil {
			errorResponse(w, http.StatusNotFound, "number session unavailable")
			return
		}
		session = state.session
		if session == nil || session.Kind != LinqNumberSessionKind || session.LinqNumberID != numberID {
			errorResponse(w, http.StatusNotFound, "number session unavailable")
			return
		}
		actualPhone, hasIdentity := manager.accountPhone(state)
		if !hasIdentity || actualPhone != state.Phone {
			errorResponse(w, http.StatusConflict, "registered number account is not verified")
			return
		}
	}

	session.SendMu.Lock()
	defer session.SendMu.Unlock()
	if receipt, err := readLinqNumberDeliveryReceipt(manager, numberID, request.DeliveryKey); err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to read message receipt")
		return
	} else if receipt != nil {
		jsonResponse(w, map[string]interface{}{"id": receipt.ID, "timestamp": receipt.Timestamp, "deduplicated": true})
		return
	}
	if !session.Client.IsConnected() {
		if requireConnected {
			errorResponse(w, http.StatusConflict, "registered number account is not connected and verified")
			return
		}
		if err := session.Client.Connect(); err != nil && !strings.Contains(err.Error(), "already connected") {
			errorResponse(w, http.StatusServiceUnavailable, "number session connection failed")
			return
		}
	}
	if !waitForNumberConnection(session, manager.connectReadyWait, manager.connectReadyPoll) {
		errorResponse(w, http.StatusServiceUnavailable, "number session is not connected")
		return
	}
	actualPhone, hasIdentity := manager.accountPhone(state)
	if !hasIdentity || actualPhone != state.Phone {
		errorResponse(w, http.StatusConflict, "registered number account is not verified")
		return
	}

	started := time.Now()
	response, err := session.Client.SendMessage(r.Context(), jid, linqNumberMessage(request.Text, request.ReplyTo, jid), whatsmeow.SendRequestExtra{ID: stableLinqNumberMessageID(numberID, request.DeliveryKey)})
	observeMessageProcessing("text", started)
	if err != nil {
		errorResponse(w, http.StatusBadGateway, "WhatsApp message delivery failed")
		return
	}
	if err := manager.numberReceiptWriter(manager, numberID, request.DeliveryKey, deliveryReceipt{ID: response.ID, Timestamp: response.Timestamp.Unix()}); err != nil {
		errorResponse(w, http.StatusInternalServerError, "message sent but receipt persistence failed")
		return
	}
	messagesTotal.WithLabelValues("outbound", "text").Inc()
	jsonResponse(w, map[string]interface{}{"id": response.ID, "timestamp": response.Timestamp.Unix(), "deduplicated": false})
}

func sendInternalLinqNumberTextHandler(w http.ResponseWriter, r *http.Request, numberID int) {
	var request linqNumberTextRequest
	if !decodeLinqNumberTextRequest(w, r, &request) {
		return
	}
	sendVerifiedLinqNumberText(w, r, numberID, request, true)
}

const (
	maxLinqNumberDecodedMediaBytes = 25 * 1024 * 1024
	maxLinqNumberMediaRequestBytes = 34 * 1024 * 1024
	maxLinqNumberAudioSeconds      = 24 * 60 * 60
)

type linqNumberMediaKind string

const (
	linqNumberMediaImage    linqNumberMediaKind = "image"
	linqNumberMediaDocument linqNumberMediaKind = "document"
	linqNumberMediaAudio    linqNumberMediaKind = "audio"
)

type linqNumberMediaRequest struct {
	ChatJID     string `json:"chat_jid"`
	ImageB64    string `json:"image_b64"`
	DocB64      string `json:"doc_b64"`
	AudioB64    string `json:"audio_b64"`
	MimeType    string `json:"mime_type"`
	Caption     string `json:"caption"`
	Filename    string `json:"filename"`
	PTT         bool   `json:"ptt"`
	Seconds     uint32 `json:"seconds"`
	ReplyTo     string `json:"reply_to"`
	DeliveryKey string `json:"delivery_key"`
}

func decodeLinqNumberMediaRequest(w http.ResponseWriter, r *http.Request, kind linqNumberMediaKind, request *linqNumberMediaRequest) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxLinqNumberMediaRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(request); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid media request")
		return nil, false
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		errorResponse(w, http.StatusBadRequest, "invalid media request")
		return nil, false
	}
	encoded := request.ImageB64
	if kind == linqNumberMediaDocument {
		encoded = request.DocB64
	} else if kind == linqNumberMediaAudio {
		encoded = request.AudioB64
	}
	if strings.TrimSpace(request.ChatJID) == "" || strings.TrimSpace(encoded) == "" || strings.TrimSpace(request.MimeType) == "" || strings.TrimSpace(request.DeliveryKey) == "" || len(request.MimeType) > 255 || len(request.Caption) > 4096 || len(request.Filename) > 255 || len(request.ReplyTo) > 512 || len(request.DeliveryKey) > 200 || base64.StdEncoding.DecodedLen(len(encoded)) > maxLinqNumberDecodedMediaBytes {
		errorResponse(w, http.StatusBadRequest, "chat jid, media, mime type, and delivery key required")
		return nil, false
	}
	if kind == linqNumberMediaDocument && strings.TrimSpace(request.Filename) == "" {
		errorResponse(w, http.StatusBadRequest, "filename required")
		return nil, false
	}
	if kind == linqNumberMediaAudio {
		parsedMime, _, err := mime.ParseMediaType(request.MimeType)
		if err != nil || !strings.HasPrefix(strings.ToLower(parsedMime), "audio/") || len(parsedMime) == len("audio/") || request.Seconds > maxLinqNumberAudioSeconds {
			errorResponse(w, http.StatusBadRequest, "invalid audio request")
			return nil, false
		}
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > maxLinqNumberDecodedMediaBytes || base64.StdEncoding.EncodeToString(data) != encoded {
		errorResponse(w, http.StatusBadRequest, "invalid media encoding")
		return nil, false
	}
	return data, true
}

func sendInternalLinqNumberMediaHandler(w http.ResponseWriter, r *http.Request, numberID int, kind linqNumberMediaKind) {
	request := linqNumberMediaRequest{}
	data, ok := decodeLinqNumberMediaRequest(w, r, kind, &request)
	if !ok {
		return
	}
	jid, err := parseDirectChatJID(request.ChatJID)
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
	receipt, err := readLinqNumberDeliveryReceipt(manager, numberID, request.DeliveryKey)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "failed to read message receipt")
		return
	}
	if receipt != nil {
		jsonResponse(w, map[string]interface{}{"id": receipt.ID, "timestamp": receipt.Timestamp, "deduplicated": true})
		return
	}
	// Revalidate immediately before upload/send. A stale number state must never
	// select a personal session or an account paired to a different phone.
	_, current, verified := manager.verifiedConnectedNumberSession(numberID)
	if !verified || current != session {
		errorResponse(w, http.StatusConflict, "registered number account is not connected and verified")
		return
	}

	extra := whatsmeow.SendRequestExtra{ID: stableLinqNumberMessageID(numberID, request.DeliveryKey)}
	var response whatsmeow.SendResponse
	if kind == linqNumberMediaImage {
		response, err = sendUploadedImage(r.Context(), session, jid, data, request.MimeType, request.Caption, request.ReplyTo, extra)
	} else if kind == linqNumberMediaDocument {
		response, err = sendUploadedDocument(r.Context(), session, jid, data, request.MimeType, request.Filename, request.Caption, request.ReplyTo, extra)
	} else {
		response, err = sendUploadedAudio(r.Context(), session, jid, data, request.MimeType, request.PTT, request.Seconds, request.ReplyTo, extra)
	}
	if err != nil {
		errorResponse(w, http.StatusBadGateway, "WhatsApp media delivery failed")
		return
	}
	if err := manager.numberReceiptWriter(manager, numberID, request.DeliveryKey, deliveryReceipt{ID: response.ID, Timestamp: response.Timestamp.Unix()}); err != nil {
		// The provider ID is deterministic, so retrying after this partial result
		// cannot create a second provider message even though upload is repeated.
		errorResponse(w, http.StatusInternalServerError, "media sent but receipt persistence failed")
		return
	}
	if kind == linqNumberMediaAudio && request.PTT {
		messagesTotal.WithLabelValues("outbound", "voice").Inc()
	} else {
		messagesTotal.WithLabelValues("outbound", "media").Inc()
	}
	jsonResponse(w, map[string]interface{}{"id": response.ID, "timestamp": response.Timestamp.Unix(), "deduplicated": false})
}

func downloadInternalLinqNumberMediaHandler(w http.ResponseWriter, r *http.Request, numberID int) {
	if r.Method != http.MethodPost {
		errorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
	var request mediaDownloadRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid media request")
		return
	}
	_, session, ok := manager.verifiedConnectedNumberSession(numberID)
	if !ok {
		errorResponse(w, http.StatusConflict, "registered number account is not connected and verified")
		return
	}
	downloadMediaForSession(w, session, request)
}

func sendLinqNumberTextHandler(w http.ResponseWriter, r *http.Request, numberID int) {
	var request struct {
		Recipient   string `json:"recipient"`
		Text        string `json:"text"`
		DeliveryKey string `json:"delivery_key"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid message request")
		return
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		errorResponse(w, http.StatusBadRequest, "invalid message request")
		return
	}
	recipient, validRecipient := normalizeE164(request.Recipient)
	if !validRecipient || strings.TrimSpace(request.Text) == "" || strings.TrimSpace(request.DeliveryKey) == "" || len(request.Text) > 4096 || len(request.DeliveryKey) > 200 {
		errorResponse(w, http.StatusBadRequest, "recipient, text, and delivery key required")
		return
	}
	sendVerifiedLinqNumberText(w, r, numberID, linqNumberTextRequest{
		ChatJID:     strings.TrimPrefix(recipient, "+") + "@" + types.DefaultUserServer,
		Text:        request.Text,
		DeliveryKey: request.DeliveryKey,
	}, false)
}

func streamNumberQR(w http.ResponseWriter, r *http.Request, state *numberSessionState) {
	sessionState, err := manager.getOrCreateLinqNumberSession(state.ID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "number session unavailable")
		return
	}
	if err := manager.startNumberPairing(sessionState); err != nil {
		errorResponse(w, http.StatusInternalServerError, "pairing could not be started")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		errorResponse(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	if sessionState.session.Client.IsLoggedIn() {
		fmt.Fprint(w, "event: complete\ndata: paired\n\n")
		flusher.Flush()
		return
	}
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	for {
		select {
		case code := <-sessionState.session.QRChannel:
			image, err := qr.Encode(code, qr.M)
			if err != nil {
				fmt.Fprint(w, "event: error\ndata: qr_unavailable\n\n")
				flusher.Flush()
				return
			}
			fmt.Fprintf(w, "event: qr\ndata: %s\n\n", base64.StdEncoding.EncodeToString(image.PNG()))
			flusher.Flush()
		case <-sessionState.session.LoginDone:
			fmt.Fprint(w, "event: complete\ndata: paired\n\n")
			flusher.Flush()
			return
		case <-timer.C:
			fmt.Fprint(w, "event: timeout\ndata: qr_expired\n\n")
			flusher.Flush()
			return
		case <-r.Context().Done():
			return
		}
	}
}

func statusLinqNumberPairing(w http.ResponseWriter, state *numberSessionState) {
	pairingState, err := manager.getOrCreateLinqNumberSession(state.ID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "number session unavailable")
		return
	}
	if err := manager.startNumberPairing(pairingState); err != nil {
		errorResponse(w, http.StatusInternalServerError, "pairing could not be started")
		return
	}
	actualPhone, hasIdentity := manager.accountPhone(pairingState)
	matches := hasIdentity && actualPhone == pairingState.Phone
	jsonResponse(w, map[string]interface{}{
		"registered":    true,
		"connected":     pairingState.session.Client.IsConnected(),
		"logged_in":     pairingState.session.Client.IsLoggedIn(),
		"phone_matches": matches,
		"verified":      matches && pairingState.session.Client.IsLoggedIn(),
	})
}

const pairingPage = `<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="referrer" content="no-referrer"><title>Link Jo WhatsApp number</title>
<style>body{font:16px system-ui,sans-serif;max-width:34rem;margin:3rem auto;padding:0 1rem;color:#17202a}img{width:min(80vw,360px);image-rendering:pixelated}#qr{min-height:2rem}small{color:#52616b}</style>
<h1>Link Jo WhatsApp number</h1><p>On the phone for this Jo number, open WhatsApp → Linked devices → Link a device, then scan this QR code.</p>
<div id="qr">Waiting for a QR code…</div><p id="state" role="status">Connecting securely…</p><small>Keep this page private. It grants temporary access to this pairing session.</small>
<script>
const params=new URLSearchParams(location.search), token=params.get('token');
const base=location.pathname.replace(/\/pair$/, '');
const state=document.getElementById('state'), box=document.getElementById('qr');
const source=new EventSource(base+'/qr?token='+encodeURIComponent(token||''));
source.addEventListener('qr', event=>{const image=document.createElement('img');image.alt='WhatsApp device-linking QR code';image.src='data:image/png;base64,'+event.data;box.replaceChildren(image);state.textContent='Scan this QR code with the configured phone.';});
source.addEventListener('complete', async ()=>{source.close();state.textContent='Checking the paired account…';try{const response=await fetch(base+'/status?token='+encodeURIComponent(token||''),{cache:'no-store'});const status=await response.json();box.replaceChildren();state.textContent=status.verified?'Account verified. Number pairing is complete; messaging is not enabled by this flow.':'Paired account does not match the registered phone. Outbound use remains disabled.';}catch{state.textContent='Could not verify the paired account.';}});
source.addEventListener('timeout',()=>{source.close();state.textContent='QR code expired. Register again for a new pairing link.';});
source.addEventListener('error',()=>{state.textContent='Pairing connection interrupted. Reload this page to retry.';});
</script></html>`
