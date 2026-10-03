package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	sharedInboxMaxEventBytes = 64 * 1024 * 1024
	sharedInboxMaxBytes      = 512 * 1024 * 1024 // entries and tombstones together
	sharedInboxMaxDiskBytes  = sharedInboxMaxEventBytes * 2
	sharedInboxTombstoneTTL  = 90 * 24 * time.Hour
	sharedInboxMaxTokenBytes = 128
)

var (
	errInboxFull     = errors.New("shared inbox capacity reached")
	errInboxNotFound = errors.New("shared inbox entry not found")
	errInboxConflict = errors.New("shared inbox state conflict")
)

type sharedInboxEntry struct {
	InboxID      string       `json:"inbox_id"`
	SourceKey    string       `json:"source_key"`
	CreatedAt    int64        `json:"created_at"`
	AttemptToken string       `json:"attempt_token,omitempty"` // never included in SSE events
	Event        MessageEvent `json:"event"`
}

type sharedInboxTombstone struct {
	InboxID   string `json:"inbox_id"`
	Outcome   string `json:"outcome"`
	CreatedAt int64  `json:"created_at"`
}

type sharedInboxStore struct{ manager *SessionManager }

func (s *sharedInboxStore) numberDir(numberID int) string {
	return filepath.Join(s.manager.dataDir, "linq_number_inbox", fmt.Sprintf("%d", numberID))
}
func (s *sharedInboxStore) entryPath(numberID int, sourceKey string) string {
	return filepath.Join(s.numberDir(numberID), "entries", sourceKey+".json")
}
func (s *sharedInboxStore) tombstonePath(numberID int, sourceKey string) string {
	return filepath.Join(s.numberDir(numberID), "tombstones", sourceKey+".json")
}

func sharedInboxSourceKey(payload MessagePayload) string {
	// Contact arrays share a stanza ID. ContactIndex is set only for array members,
	// so regular text continues to deduplicate exclusively by its provider source.
	discriminator := ""
	if payload.ContactIndex != nil {
		discriminator = fmt.Sprintf("contact:%d", *payload.ContactIndex)
	}
	sum := sha256.Sum256([]byte(payload.ID + "\x00" + payload.ChatJID + "\x00" + payload.SenderJID + "\x00" + discriminator))
	// A number-scoped 128-bit hash is also the opaque inbox ID. Direct keyed
	// reads avoid decrypting every other user's checkpoint for a single ack.
	return hex.EncodeToString(sum[:16])
}

func (s *sharedInboxStore) encode(value interface{}) ([]byte, error) {
	plain, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(plain) > sharedInboxMaxEventBytes {
		return nil, errors.New("shared inbox event exceeds size limit")
	}
	if s.manager.encryptKey == nil {
		return plain, nil
	}
	sealed, err := s.manager.encrypt(plain)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(struct {
		Encrypted string `json:"encrypted"`
	}{sealed})
	if err != nil {
		return nil, err
	}
	if len(data) > sharedInboxMaxDiskBytes {
		return nil, errors.New("shared inbox encrypted event exceeds disk size limit")
	}
	return data, nil
}

func (s *sharedInboxStore) decode(data []byte, value interface{}) error {
	var envelope struct {
		Encrypted string `json:"encrypted"`
	}
	if json.Unmarshal(data, &envelope) == nil && envelope.Encrypted != "" {
		plain, err := s.manager.decrypt(envelope.Encrypted)
		if err != nil {
			return err
		}
		return json.Unmarshal(plain, value)
	}
	return json.Unmarshal(data, value)
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// ensurePrivateDir fsyncs every newly-created directory's parent, so the rename
// below cannot point into a directory that disappears after a hard crash.
func ensurePrivateDir(path string) error {
	path = filepath.Clean(path)
	missing := []string{}
	for p := path; ; p = filepath.Dir(p) {
		_, err := os.Stat(p)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return err
		}
		missing = append(missing, p)
		next := filepath.Dir(p)
		if next == p {
			return fmt.Errorf("no existing parent for %s", path)
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		dir := missing[i]
		if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
			return err
		}
		if err := os.Chmod(dir, 0700); err != nil {
			return err
		}
		if err := syncDir(filepath.Dir(dir)); err != nil {
			return err
		}
	}
	// Existing inbox directories may predate this code; retain no group/world
	// access there either. Sync the metadata change before using the directory.
	if err := os.Chmod(path, 0700); err != nil {
		return err
	}
	return syncDir(path)
}

func atomicPrivateWrite(path string, data []byte) error {
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".inbox-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return err
	}
	if err = os.Chmod(path, 0600); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func readBounded(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > sharedInboxMaxDiskBytes {
		return nil, errors.New("shared inbox disk record exceeds size limit")
	}
	return os.ReadFile(path)
}
func (s *sharedInboxStore) readEntry(numberID int, sourceKey string) (*sharedInboxEntry, error) {
	data, err := readBounded(s.entryPath(numberID, sourceKey))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entry sharedInboxEntry
	if err := s.decode(data, &entry); err != nil {
		return nil, err
	}
	if entry.SourceKey != sourceKey || entry.InboxID == "" {
		return nil, errors.New("invalid shared inbox entry")
	}
	return &entry, nil
}
func (s *sharedInboxStore) readTombstone(numberID int, sourceKey string) (*sharedInboxTombstone, error) {
	data, err := readBounded(s.tombstonePath(numberID, sourceKey))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var tombstone sharedInboxTombstone
	if err := s.decode(data, &tombstone); err != nil {
		return nil, err
	}
	if tombstone.InboxID == "" || tombstone.Outcome == "" {
		return nil, errors.New("invalid shared inbox tombstone")
	}
	return &tombstone, nil
}

func (s *sharedInboxStore) usedBytesLocked(numberID int) (int64, error) {
	var total int64
	for _, kind := range []string{"entries", "tombstones"} {
		files, err := filepath.Glob(filepath.Join(s.numberDir(numberID), kind, "*"))
		if err != nil {
			return 0, err
		}
		for _, file := range files {
			info, err := os.Stat(file)
			if err != nil {
				return 0, err
			}
			if info.Size() > sharedInboxMaxDiskBytes {
				return 0, errors.New("shared inbox disk record exceeds size limit")
			}
			total += info.Size()
			if total > sharedInboxMaxBytes {
				return total, nil
			}
		}
	}
	return total, nil
}

// cleanup removes only old terminal tombstones; it never evicts unacknowledged
// entries. The 90-day duplicate horizon is intentional and documented in README.
func (s *sharedInboxStore) cleanupTombstonesLocked(numberID int, now time.Time) error {
	files, err := filepath.Glob(filepath.Join(s.numberDir(numberID), "tombstones", "*.json"))
	if err != nil {
		return err
	}
	for _, file := range files {
		source := strings.TrimSuffix(filepath.Base(file), ".json")
		t, err := s.readTombstone(numberID, source)
		if err != nil {
			return err
		}
		if t != nil && t.CreatedAt > 0 && now.Sub(time.Unix(0, t.CreatedAt)) > sharedInboxTombstoneTTL {
			if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := syncDir(filepath.Dir(file)); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *sharedInboxStore) fitsLocked(numberID int, replaced int64, added int64) error {
	used, err := s.usedBytesLocked(numberID)
	if err != nil {
		return err
	}
	if used-replaced+added > sharedInboxMaxBytes {
		return errInboxFull
	}
	return nil
}

func (s *sharedInboxStore) persistInbound(numberID int, event MessageEvent) (*sharedInboxEntry, bool, error) {
	payload, ok := event.Payload.(MessagePayload)
	if !ok || payload.ID == "" {
		return nil, false, errors.New("invalid shared inbound event")
	}
	sourceKey := sharedInboxSourceKey(payload)
	s.manager.inboxMu.Lock()
	defer s.manager.inboxMu.Unlock()
	if err := s.cleanupTombstonesLocked(numberID, time.Now()); err != nil {
		return nil, false, err
	}
	if t, err := s.readTombstone(numberID, sourceKey); err != nil {
		return nil, false, err
	} else if t != nil {
		return nil, true, nil
	}
	if existing, err := s.readEntry(numberID, sourceKey); err != nil {
		return nil, false, err
	} else if existing != nil {
		return existing, true, nil
	}
	inboxID := sourceKey
	event.InboxID, event.ProcessingState, event.ResponseCheckpoint = inboxID, "pending", nil
	entry := &sharedInboxEntry{InboxID: inboxID, SourceKey: sourceKey, CreatedAt: time.Now().UnixNano(), Event: event}
	data, err := s.encode(entry)
	if err != nil {
		return nil, false, err
	}
	if err := s.fitsLocked(numberID, 0, int64(len(data))); err != nil {
		return nil, false, err
	}
	if err := atomicPrivateWrite(s.entryPath(numberID, sourceKey), data); err != nil {
		return nil, false, err
	}
	return entry, false, nil
}

func (s *sharedInboxStore) entriesLocked(numberID int, metadataOnly bool, seen map[string]bool) ([]*sharedInboxEntry, error) {
	files, err := filepath.Glob(filepath.Join(s.numberDir(numberID), "entries", "*.json"))
	if err != nil {
		return nil, err
	}
	entries := make([]*sharedInboxEntry, 0, len(files))
	for _, file := range files {
		id := strings.TrimSuffix(filepath.Base(file), ".json")
		if seen[id] {
			continue
		}
		entry, err := s.byInboxIDLocked(numberID, id)
		if err == errInboxNotFound {
			continue
		}
		if err != nil {
			return nil, err
		}
		if metadataOnly {
			entry.Event.Payload, entry.Event.ResponseCheckpoint, entry.AttemptToken = nil, nil, ""
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
func (s *sharedInboxStore) list(numberID int) ([]sharedInboxEntry, error) {
	return s.listEntries(numberID, false, nil)
}

func (s *sharedInboxStore) references(numberID int, seen map[string]bool) ([]sharedInboxEntry, error) {
	return s.listEntries(numberID, true, seen)
}

func (s *sharedInboxStore) listEntries(numberID int, metadataOnly bool, seen map[string]bool) ([]sharedInboxEntry, error) {
	s.manager.inboxMu.Lock()
	defer s.manager.inboxMu.Unlock()
	entries, err := s.entriesLocked(numberID, metadataOnly, seen)
	if err != nil {
		return nil, err
	}
	out := make([]sharedInboxEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out, nil
}
func (s *sharedInboxStore) byInboxIDLocked(numberID int, inboxID string) (*sharedInboxEntry, error) {
	if !validInboxID(inboxID) {
		return nil, errInboxNotFound
	}
	if tombstone, err := s.readTombstone(numberID, inboxID); err != nil {
		return nil, err
	} else if tombstone != nil {
		if err := s.removeEntryLocked(numberID, inboxID); err != nil {
			return nil, err
		}
		return nil, errInboxNotFound
	}
	entry, err := s.readEntry(numberID, inboxID)
	if err != nil {
		return nil, err
	}
	if entry == nil || entry.InboxID != inboxID {
		return nil, errInboxNotFound
	}
	return entry, nil
}

func (s *sharedInboxStore) removeEntryLocked(numberID int, inboxID string) error {
	path := s.entryPath(numberID, inboxID)
	if err := os.Remove(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func (s *sharedInboxStore) byInboxID(numberID int, inboxID string) (*sharedInboxEntry, error) {
	s.manager.inboxMu.Lock()
	defer s.manager.inboxMu.Unlock()
	return s.byInboxIDLocked(numberID, inboxID)
}

func validAttemptToken(token string) bool {
	return token != "" && len(token) <= sharedInboxMaxTokenBytes
}
func (s *sharedInboxStore) checkpoint(numberID int, inboxID, state, token string, checkpoint json.RawMessage) (*sharedInboxEntry, error) {
	s.manager.inboxMu.Lock()
	defer s.manager.inboxMu.Unlock()
	entry, err := s.byInboxIDLocked(numberID, inboxID)
	if err != nil {
		return nil, err
	}
	if state == "started" {
		if !validAttemptToken(token) || len(checkpoint) != 0 || entry.Event.ProcessingState == "ready" {
			return nil, errInboxConflict
		}
		if entry.Event.ProcessingState == "started" {
			if entry.AttemptToken == token {
				return entry, nil
			}
			return nil, errInboxConflict
		}
		entry.AttemptToken, entry.Event.ProcessingState = token, "started"
	} else if state == "ready" {
		if !validAttemptToken(token) || len(checkpoint) == 0 || entry.Event.ProcessingState == "pending" || entry.AttemptToken != token {
			return nil, errInboxConflict
		}
		if entry.Event.ProcessingState == "ready" {
			if string(entry.Event.ResponseCheckpoint) == string(checkpoint) {
				return entry, nil
			}
			return nil, errInboxConflict
		}
		entry.Event.ProcessingState, entry.Event.ResponseCheckpoint = "ready", checkpoint
	} else {
		return nil, errInboxConflict
	}
	data, err := s.encode(entry)
	if err != nil {
		return nil, err
	}
	old, err := os.Stat(s.entryPath(numberID, entry.SourceKey))
	if err != nil {
		return nil, err
	}
	if err := s.fitsLocked(numberID, old.Size(), int64(len(data))); err != nil {
		return nil, err
	}
	if err := atomicPrivateWrite(s.entryPath(numberID, entry.SourceKey), data); err != nil {
		return nil, err
	}
	return entry, nil
}

func (s *sharedInboxStore) tombstoneByInboxIDLocked(numberID int, inboxID string) (*sharedInboxTombstone, error) {
	if !validInboxID(inboxID) {
		return nil, errInboxNotFound
	}
	return s.readTombstone(numberID, inboxID)
}
func (s *sharedInboxStore) ack(numberID int, inboxID, outcome, token string) error {
	s.manager.inboxMu.Lock()
	defer s.manager.inboxMu.Unlock()
	t, err := s.tombstoneByInboxIDLocked(numberID, inboxID)
	if err != nil {
		return err
	}
	if t != nil {
		if t.Outcome != outcome {
			return errInboxConflict
		}
		return s.removeEntryLocked(numberID, inboxID)
	}
	entry, err := s.byInboxIDLocked(numberID, inboxID)
	if err != nil {
		return err
	}
	state := entry.Event.ProcessingState
	allowed := (outcome == "completed" && state == "ready" && (token == "" || token == entry.AttemptToken)) ||
		(outcome == "ignored" && ((state == "pending" && token == "") || (state == "started" && token == entry.AttemptToken))) ||
		(outcome == "superseded" && (state == "started" || state == "ready") && token == entry.AttemptToken)
	if !allowed {
		return errInboxConflict
	}
	t = &sharedInboxTombstone{InboxID: inboxID, Outcome: outcome, CreatedAt: time.Now().UnixNano()}
	data, err := s.encode(t)
	if err != nil {
		return err
	}
	if err := s.cleanupTombstonesLocked(numberID, time.Now()); err != nil {
		return err
	}
	// Account for the payload that will be removed only after the tombstone is
	// durable. This permits terminal progress at the cap without risking replay:
	// the bounded tombstone is the sole temporary overage.
	old, err := os.Stat(s.entryPath(numberID, entry.SourceKey))
	if err != nil {
		return err
	}
	if err := s.fitsLocked(numberID, old.Size(), int64(len(data))); err != nil {
		return err
	}
	if err := atomicPrivateWrite(s.tombstonePath(numberID, inboxID), data); err != nil {
		return err
	}
	return s.removeEntryLocked(numberID, inboxID)
}

type sharedInboxStatus struct {
	Pending          int   `json:"pending"`
	Started          int   `json:"started"`
	Ready            int   `json:"ready"`
	OldestAgeSeconds int64 `json:"oldest_age_seconds"`
	UsedBytes        int64 `json:"used_bytes"`
}

func (s *sharedInboxStore) status(numberID int) (sharedInboxStatus, error) {
	s.manager.inboxMu.Lock()
	defer s.manager.inboxMu.Unlock()
	entries, err := s.entriesLocked(numberID, true, nil)
	if err != nil {
		return sharedInboxStatus{}, err
	}
	used, err := s.usedBytesLocked(numberID)
	if err != nil {
		return sharedInboxStatus{}, err
	}
	status := sharedInboxStatus{UsedBytes: used}
	oldest := int64(0)
	now := time.Now().UnixNano()
	for _, entry := range entries {
		switch entry.Event.ProcessingState {
		case "pending":
			status.Pending++
		case "started":
			status.Started++
		case "ready":
			status.Ready++
		}
		if oldest == 0 || entry.CreatedAt < oldest {
			oldest = entry.CreatedAt
		}
	}
	if oldest != 0 {
		status.OldestAgeSeconds = (now - oldest) / int64(time.Second)
	}
	return status, nil
}

func (s *UserSession) publishMessageEvent(event MessageEvent) bool {
	if s.Kind == LinqNumberSessionKind {
		if payload, ok := event.Payload.(MessagePayload); ok {
			if payload.IsFromMe {
				return true
			}
			if s.Inbox == nil {
				return false
			}
			entry, duplicate, err := s.Inbox.persistInbound(s.LinqNumberID, event)
			if err != nil {
				sharedInboxPersistenceFailuresTotal.WithLabelValues("persist").Inc()
				log.Printf("shared inbox persistence failed type=%T", event.Payload)
				return false
			}
			if duplicate {
				return true
			}
			event = entry.Event
		}
	}
	select {
	case s.EventChan <- event:
	default:
		sharedInboxPersistenceFailuresTotal.WithLabelValues("notification_deferred").Inc()
		log.Printf("shared inbox notification deferred type=%T", event.Payload)
	}
	return true
}
