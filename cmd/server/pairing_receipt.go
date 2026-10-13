package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

const (
	pairingAttemptTTL       = 10 * time.Minute
	maxPersistedPairingLogs = 256
)

type pairingAttempt struct {
	token      string // An opaque browser capability. Never log or format it.
	tokenHash  [32]byte
	requestID  string
	userID     int
	session    *UserSession
	expiresAt  time.Time
	generation uint64

	qrConfirmed  bool // Only the QR channel identifies a particular local QR lifecycle.
	pairSuccess  bool // Candidate identity from the first valid provider PairSuccess for this attempt.
	connected    bool // A Connected event observed after that candidate.
	pairedPN     string
	pairedLID    string
	backupFailed bool
	recovered    bool // Provider proof is process-memory evidence and cannot survive restart.
}

type persistedPairingAttempt struct {
	Capability string    `json:"capability"`
	TokenHash  string    `json:"token_hash"`
	RequestID  string    `json:"request_id"`
	UserID     int       `json:"user_id"`
	ExpiresAt  time.Time `json:"expires_at"`
	Generation uint64    `json:"generation"`
	PairedPN   string    `json:"paired_pn,omitempty"`
	PairedLID  string    `json:"paired_lid,omitempty"`
}

func (m *SessionManager) pairingLedgerPath(userID int) string {
	return filepath.Join(m.dataDir, "pairing_attempts", strconv.Itoa(userID), "attempt.json")
}

// The capability is retained only inside the encrypted ledger, together with a
// hash for capability lookup. Keeping it lets an authenticated caller recover a
// response lost after the QR flow started, without minting another capability.
func (m *SessionManager) persistPairingAttempt(attempt *pairingAttempt) error {
	if m.encryptKey == nil {
		return errors.New("durable pairing ledger is not configured")
	}
	ledger := persistedPairingAttempt{
		Capability: attempt.token,
		TokenHash:  base64.RawURLEncoding.EncodeToString(attempt.tokenHash[:]),
		RequestID:  attempt.requestID,
		UserID:     attempt.userID,
		ExpiresAt:  attempt.expiresAt.UTC(),
		Generation: attempt.generation,
		PairedPN:   attempt.pairedPN,
		PairedLID:  attempt.pairedLID,
	}
	plain, err := json.Marshal(ledger)
	if err != nil {
		return err
	}
	ciphertext, err := m.encrypt(plain)
	if err != nil {
		return err
	}
	path := m.pairingLedgerPath(attempt.userID)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".attempt-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(ciphertext); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (m *SessionManager) readPairingLedger(userID int) (*persistedPairingAttempt, error) {
	data, err := os.ReadFile(m.pairingLedgerPath(userID))
	if err != nil {
		return nil, err
	}
	plain, err := m.decrypt(string(data))
	if err != nil {
		return nil, err
	}
	var ledger persistedPairingAttempt
	decoder := json.NewDecoder(strings.NewReader(string(plain)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ledger); err != nil {
		return nil, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	if ledger.UserID != userID || ledger.UserID <= 0 || ledger.Generation == 0 || ledger.ExpiresAt.IsZero() || !canonicalRequestID(ledger.RequestID) {
		return nil, errors.New("invalid pairing ledger")
	}
	raw, err := base64.RawURLEncoding.DecodeString(ledger.Capability)
	if err != nil || len(raw) != sha256.Size {
		return nil, errors.New("invalid pairing ledger capability")
	}
	hash, err := base64.RawURLEncoding.DecodeString(ledger.TokenHash)
	computedHash := sha256.Sum256(raw)
	if err != nil || len(hash) != sha256.Size || subtle.ConstantTimeCompare(hash, computedHash[:]) != 1 {
		return nil, errors.New("invalid pairing ledger capability")
	}
	return &ledger, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("unexpected trailing JSON data")
	}
	return nil
}

func canonicalRequestID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}

func pairingAttemptFromLedger(ledger *persistedPairingAttempt, session *UserSession) *pairingAttempt {
	raw, _ := base64.RawURLEncoding.DecodeString(ledger.Capability)
	attempt := &pairingAttempt{token: ledger.Capability, requestID: ledger.RequestID, userID: ledger.UserID, session: session, expiresAt: ledger.ExpiresAt, generation: ledger.Generation, recovered: true, pairedPN: ledger.PairedPN, pairedLID: ledger.PairedLID}
	tokenHash := sha256.Sum256(raw)
	copy(attempt.tokenHash[:], tokenHash[:])
	return attempt
}

// pairingAttemptForUserLocked only restores a non-expired attempt. Restoring
// intentionally creates no provider lifecycle: post-restart proof is unknown.
func (m *SessionManager) pairingAttemptForUserLocked(userID int) *pairingAttempt {
	m.mu.RLock()
	session := m.sessions[userID]
	m.mu.RUnlock()
	if session != nil {
		session.pairingMu.Lock()
		attempt := session.pairingAttempt
		session.pairingMu.Unlock()
		if attempt != nil && time.Now().Before(attempt.expiresAt) {
			return attempt
		}
	}
	ledger, err := m.readPairingLedger(userID)
	if err != nil || !time.Now().Before(ledger.ExpiresAt) {
		return nil
	}
	restored, err := m.GetOrCreateSession(userID)
	if err != nil {
		return nil
	}
	restored.pairingMu.Lock()
	defer restored.pairingMu.Unlock()
	if current := restored.pairingAttempt; current != nil && time.Now().Before(current.expiresAt) {
		return current
	}
	attempt := pairingAttemptFromLedger(ledger, restored)
	restored.pairingAttempt = attempt
	return attempt
}

func (m *SessionManager) pairingRequestOwnerLocked(requestID string) (int, bool) {
	entries, err := os.ReadDir(filepath.Join(m.dataDir, "pairing_attempts"))
	if err != nil {
		return 0, false
	}
	seen := 0
	for _, entry := range entries {
		if seen >= maxPersistedPairingLogs {
			break
		}
		if !entry.IsDir() {
			continue
		}
		userID, err := strconv.Atoi(entry.Name())
		if err != nil || userID <= 0 {
			continue
		}
		seen++
		ledger, err := m.readPairingLedger(userID)
		if err == nil && ledger.RequestID == requestID {
			return userID, true
		}
	}
	return 0, false
}

// beginPairingAttempt is the durable idempotency boundary for an authenticated
// backend request. Its lock spans ledger lookup, session acquisition, persistence
// and QR start so two concurrent requests cannot create two provider flows.
func (m *SessionManager) beginPairingAttempt(userID int, requestID string) (*pairingAttempt, error) {
	if userID <= 0 || !canonicalRequestID(requestID) {
		return nil, errors.New("invalid pairing attempt")
	}
	if m.encryptKey == nil {
		return nil, errors.New("durable pairing ledger is not configured")
	}
	m.pairingAttemptMu.Lock()
	defer m.pairingAttemptMu.Unlock()

	if owner, found := m.pairingRequestOwnerLocked(requestID); found && owner != userID {
		return nil, errors.New("pairing request belongs to a different owner")
	}
	ledger, ledgerErr := m.readPairingLedger(userID)
	if ledgerErr == nil {
		if ledger.RequestID == requestID {
			if !time.Now().Before(ledger.ExpiresAt) {
				return nil, errors.New("pairing attempt expired")
			}
			attempt := m.pairingAttemptForUserLocked(userID)
			if attempt == nil {
				return nil, errors.New("pairing ledger is unavailable")
			}
			return attempt, nil
		}
		if time.Now().Before(ledger.ExpiresAt) {
			return nil, errors.New("pairing attempt already pending")
		}
	} else if !os.IsNotExist(ledgerErr) {
		return nil, errors.New("pairing ledger is unavailable")
	}

	session, err := m.GetOrCreateSession(userID)
	if err != nil {
		return nil, err
	}
	if session.Client.GetStore().GetID() != nil {
		return nil, errors.New("session already has a linked device")
	}
	var raw [32]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return nil, fmt.Errorf("create pairing attempt: %w", err)
	}
	session.pairingMu.Lock()
	session.pairingGeneration++
	attempt := &pairingAttempt{token: base64.RawURLEncoding.EncodeToString(raw[:]), tokenHash: sha256.Sum256(raw[:]), requestID: requestID, userID: userID, session: session, expiresAt: time.Now().Add(pairingAttemptTTL), generation: session.pairingGeneration}
	session.pairingAttempt = attempt
	session.pairingMu.Unlock()
	if err := m.persistPairingAttempt(attempt); err != nil {
		session.pairingMu.Lock()
		if session.pairingAttempt == attempt {
			session.pairingAttempt = nil
		}
		session.pairingMu.Unlock()
		return nil, fmt.Errorf("persist pairing attempt: %w", err)
	}
	if err := startPersonalQR(session, attempt.generation); err != nil {
		session.pairingMu.Lock()
		if session.pairingAttempt == attempt {
			session.pairingAttempt = nil
		}
		session.pairingMu.Unlock()
		return nil, err
	}
	return attempt, nil
}

func (m *SessionManager) pairingAttempt(token string) *pairingAttempt {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != sha256.Size {
		return nil
	}
	got := sha256.Sum256(raw)
	m.pairingAttemptMu.Lock()
	defer m.pairingAttemptMu.Unlock()
	m.mu.RLock()
	userIDs := make([]int, 0, len(m.sessions))
	for userID := range m.sessions {
		if len(userIDs) >= maxPersistedPairingLogs {
			break
		}
		userIDs = append(userIDs, userID)
	}
	m.mu.RUnlock()
	entries, _ := os.ReadDir(filepath.Join(m.dataDir, "pairing_attempts"))
	for _, entry := range entries {
		if len(userIDs) >= maxPersistedPairingLogs {
			break
		}
		if entry.IsDir() {
			if userID, err := strconv.Atoi(entry.Name()); err == nil && userID > 0 {
				seen := false
				for _, current := range userIDs {
					if current == userID {
						seen = true
						break
					}
				}
				if !seen {
					userIDs = append(userIDs, userID)
				}
			}
		}
	}
	for _, userID := range userIDs {
		attempt := m.pairingAttemptForUserLocked(userID)
		if attempt != nil && subtle.ConstantTimeCompare(got[:], attempt.tokenHash[:]) == 1 {
			return attempt
		}
	}
	return nil
}

func startPersonalQR(session *UserSession, generation uint64) error {
	if session.Client.IsConnected() {
		return errors.New("pairing session is already connected")
	}
	for {
		select {
		case <-session.QRChannel:
		default:
			goto qrDrained
		}
	}
qrDrained:
	select {
	case <-session.LoginDone:
	default:
	}
	qrChan, err := session.Client.GetQRChannel(context.Background())
	if err != nil {
		return fmt.Errorf("create QR channel: %w", err)
	}
	if err := session.Client.Connect(); err != nil && !strings.Contains(err.Error(), "already connected") {
		return fmt.Errorf("connect for QR: %w", err)
	}
	go func() {
		for event := range qrChan {
			switch event.Event {
			case "code":
				select {
				case session.QRChannel <- event.Code:
				default:
				}
			case "success":
				session.recordQRSuccess(generation)
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

func (s *UserSession) recordQRSuccess(generation uint64) {
	s.pairingMu.Lock()
	defer s.pairingMu.Unlock()
	if attempt := s.pairingAttempt; attempt != nil && attempt.session == s && !attempt.recovered && attempt.generation == generation && attempt.generation == s.pairingGeneration && time.Now().Before(attempt.expiresAt) {
		attempt.qrConfirmed = true
	}
}

func (s *UserSession) recordPairSuccess(event *events.PairSuccess) {
	if event == nil || event.ID.User == "" || event.ID.Server != types.DefaultUserServer || event.LID.User == "" || event.LID.Server != types.HiddenUserServer {
		return
	}
	pn, lid := event.ID.ToNonAD().String(), event.LID.ToNonAD().String()
	if pn == "" || lid == "" {
		return
	}
	s.pairingMu.Lock()
	attempt := s.pairingAttempt
	if attempt == nil || attempt.session != s || attempt.recovered || attempt.generation != s.pairingGeneration || time.Now().After(attempt.expiresAt) || attempt.pairSuccess {
		s.pairingMu.Unlock()
		return
	}
	attempt.pairSuccess, attempt.pairedPN, attempt.pairedLID = true, pn, lid
	s.pairingMu.Unlock()
	_ = manager.persistPairingAttempt(attempt)
}

func (s *UserSession) recordPairConnected() {
	s.pairingMu.Lock()
	defer s.pairingMu.Unlock()
	attempt := s.pairingAttempt
	if attempt != nil && attempt.session == s && !attempt.recovered && attempt.generation == s.pairingGeneration && attempt.pairSuccess && time.Now().Before(attempt.expiresAt) {
		attempt.connected = true
	}
}

func createPairingAttemptHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !requireInternal(w, r) {
		return
	}
	var request struct {
		UserID    int    `json:"user_id"`
		RequestID string `json:"request_id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || ensureJSONEOF(decoder) != nil || request.UserID <= 0 || !canonicalRequestID(request.RequestID) {
		errorResponse(w, http.StatusBadRequest, "invalid pairing attempt")
		return
	}
	attempt, err := manager.beginPairingAttempt(request.UserID, request.RequestID)
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "linked device"), strings.Contains(err.Error(), "already pending"), strings.Contains(err.Error(), "expired"), strings.Contains(err.Error(), "different owner"):
			errorResponse(w, http.StatusConflict, "pairing attempt conflicts with existing state")
		case strings.Contains(err.Error(), "durable pairing ledger"), strings.Contains(err.Error(), "pairing ledger"):
			errorResponse(w, http.StatusServiceUnavailable, "durable pairing is unavailable")
		default:
			errorResponse(w, http.StatusInternalServerError, "pairing attempt could not be started")
		}
		return
	}
	jsonResponse(w, map[string]string{"attempt_id": attempt.token, "qr_url": "/sessions/qr?attempt=" + attempt.token})
}

func pairingReceiptHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		errorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !requireInternal(w, r) {
		return
	}
	var request struct {
		AttemptID string `json:"attempt_id"`
		UserID    int    `json:"user_id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || ensureJSONEOF(decoder) != nil || strings.TrimSpace(request.AttemptID) == "" || request.UserID <= 0 {
		errorResponse(w, http.StatusBadRequest, "invalid pairing receipt request")
		return
	}
	attempt := manager.pairingAttempt(request.AttemptID)
	if attempt == nil {
		jsonResponse(w, map[string]string{"status": "missing"})
		return
	}
	attempt.session.pairingMu.Lock()
	if request.UserID != attempt.userID {
		attempt.session.pairingMu.Unlock()
		errorResponse(w, http.StatusForbidden, "pairing receipt owner does not match")
		return
	}
	if attempt.recovered {
		attempt.session.pairingMu.Unlock()
		jsonResponse(w, map[string]string{"status": "recovery_required"})
		return
	}
	ready := attempt.qrConfirmed && attempt.pairSuccess && attempt.connected && attempt.session.Client.IsConnected() && attempt.pairedPN != "" && attempt.pairedLID != ""
	attempt.session.pairingMu.Unlock()
	if !ready {
		jsonResponse(w, map[string]string{"status": "pending"})
		return
	}
	if err := manager.saveSessionToJoBot(attempt.userID); err != nil {
		attempt.session.pairingMu.Lock()
		attempt.backupFailed = true
		attempt.session.pairingMu.Unlock()
		jsonResponse(w, map[string]string{"status": "backup_failed"})
		return
	}
	attempt.session.pairingMu.Lock()
	attempt.backupFailed = false
	pairedPN, pairedLID := attempt.pairedPN, attempt.pairedLID
	attempt.session.pairingMu.Unlock()
	if err := manager.persistPairingAttempt(attempt); err != nil {
		jsonResponse(w, map[string]string{"status": "backup_failed"})
		return
	}
	jsonResponse(w, map[string]any{"status": "paired", "user_id": attempt.userID, "paired_pn": pairedPN, "paired_lid": pairedLID})
}
