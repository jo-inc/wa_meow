package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// writeDeviceStore creates a minimal store file matching the one column
// storeHasLoggedInDevice reads. jids lists the device rows to insert; an empty
// list models a logged-out or never-paired account (table present, no rows).
func writeDeviceStore(t *testing.T, path string, jids ...string) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE whatsmeow_device (jid TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for _, jid := range jids {
		if _, err := db.Exec(`INSERT INTO whatsmeow_device (jid) VALUES (?)`, jid); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
}

func TestParseStoreID(t *testing.T) {
	cases := []struct {
		name, prefix string
		wantID       int
		wantOK       bool
	}{
		{"user_17.db", "user_", 17, true},
		{"linq_number_3.db", "linq_number_", 3, true},
		{"user_.db", "user_", 0, false},
		{"user_abc.db", "user_", 0, false},
		{"user_17.db-wal", "user_", 0, false},
		{"linq_numbers.json", "linq_number_", 0, false},
		{"user_5.db", "linq_number_", 0, false},
	}
	for _, c := range cases {
		id, ok := parseStoreID(c.name, c.prefix)
		if ok != c.wantOK || id != c.wantID {
			t.Errorf("parseStoreID(%q,%q) = (%d,%v), want (%d,%v)", c.name, c.prefix, id, ok, c.wantID, c.wantOK)
		}
	}
}

func TestStoreHasLoggedInDevice(t *testing.T) {
	dir := t.TempDir()

	loggedIn := filepath.Join(dir, "in.db")
	writeDeviceStore(t, loggedIn, "15551230000.0:0@s.whatsapp.net")
	if ok, err := storeHasLoggedInDevice(loggedIn); err != nil || !ok {
		t.Errorf("logged-in store: got (%v,%v), want (true,nil)", ok, err)
	}

	loggedOut := filepath.Join(dir, "out.db")
	writeDeviceStore(t, loggedOut)
	if ok, err := storeHasLoggedInDevice(loggedOut); err != nil || ok {
		t.Errorf("logged-out store: got (%v,%v), want (false,nil)", ok, err)
	}

	emptyJID := filepath.Join(dir, "empty.db")
	writeDeviceStore(t, emptyJID, "")
	if ok, err := storeHasLoggedInDevice(emptyJID); err != nil || ok {
		t.Errorf("empty-jid store: got (%v,%v), want (false,nil)", ok, err)
	}

	if _, err := storeHasLoggedInDevice(filepath.Join(dir, "missing.db")); err == nil {
		t.Error("missing store should error")
	}
}

func TestSessionCountHandler(t *testing.T) {
	manager = setupTestManager(t)
	manager.adminToken = "0123456789abcdef0123456789abcdef" // >= minAdminTokenLength

	dir := manager.dataDir
	// Three personal accounts: two logged in, one logged out.
	writeDeviceStore(t, filepath.Join(dir, "user_1.db"), "a@s.whatsapp.net")
	writeDeviceStore(t, filepath.Join(dir, "user_2.db"), "b@s.whatsapp.net")
	writeDeviceStore(t, filepath.Join(dir, "user_3.db"))
	// Two shared numbers registered in the registry: on disk one is logged in,
	// one is logged out. Both start cold (no in-memory session).
	writeDeviceStore(t, filepath.Join(dir, "linq_number_2.db"), "n@s.whatsapp.net")
	writeDeviceStore(t, filepath.Join(dir, "linq_number_3.db"))
	manager.numberSessions[2] = &numberSessionState{linqNumberRecord: linqNumberRecord{ID: 2, Phone: "+100"}}
	manager.numberSessions[3] = &numberSessionState{linqNumberRecord: linqNumberRecord{ID: 3, Phone: "+200"}}
	// Noise that must be ignored.
	writeDeviceStore(t, filepath.Join(dir, "user_x.db"), "ignored@s.whatsapp.net")

	t.Run("rejects without admin token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/admin/sessions/count", nil)
		w := httptest.NewRecorder()
		sessionCountHandler(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
	})

	t.Run("rejects non-GET", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/admin/sessions/count", nil)
		req.Header.Set("Authorization", "Bearer "+manager.adminToken)
		w := httptest.NewRecorder()
		sessionCountHandler(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", w.Code)
		}
	})

	t.Run("counts disk state", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/admin/sessions/count", nil)
		req.Header.Set("Authorization", "Bearer "+manager.adminToken)
		w := httptest.NewRecorder()
		sessionCountHandler(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
		}
		var resp struct {
			Users         sessionTally `json:"users"`
			SharedNumbers sessionTally `json:"shared_numbers"`
		}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Users.Provisioned != 3 || resp.Users.LoggedIn != 2 {
			t.Errorf("users = %+v, want provisioned 3 logged_in 2", resp.Users)
		}
		if resp.SharedNumbers.Registered != 2 || resp.SharedNumbers.Provisioned != 2 ||
			resp.SharedNumbers.LoggedIn != 1 || resp.SharedNumbers.Connected != 0 {
			t.Errorf("shared_numbers = %+v, want registered 2 provisioned 2 logged_in 1 connected 0", resp.SharedNumbers)
		}
	})

	t.Run("warm connected number counts via memory", func(t *testing.T) {
		// Number 3 is logged out on disk; a warm, connected, logged-in session
		// must override the file and be counted as logged in and connected.
		manager.numberSessions[3].session = &UserSession{
			Kind:         LinqNumberSessionKind,
			LinqNumberID: 3,
			Client:       NewLoggedInMockClient(),
		}
		defer func() { manager.numberSessions[3].session = nil }()

		req := httptest.NewRequest(http.MethodGet, "/admin/sessions/count", nil)
		req.Header.Set("Authorization", "Bearer "+manager.adminToken)
		w := httptest.NewRecorder()
		sessionCountHandler(w, req)

		var resp struct {
			SharedNumbers sessionTally `json:"shared_numbers"`
		}
		json.NewDecoder(w.Body).Decode(&resp)
		if resp.SharedNumbers.Registered != 2 || resp.SharedNumbers.LoggedIn != 2 ||
			resp.SharedNumbers.Connected != 1 || resp.SharedNumbers.Warm != 1 {
			t.Errorf("shared_numbers = %+v, want registered 2 logged_in 2 connected 1 warm 1", resp.SharedNumbers)
		}
	})

	t.Run("warm session overrides disk", func(t *testing.T) {
		// A warm, logged-in session for an account whose disk store is logged
		// out must be counted via memory, not the file.
		writeDeviceStore(t, filepath.Join(dir, "user_9.db"))
		injectMockSession(manager, 9, NewLoggedInMockClient())

		req := httptest.NewRequest(http.MethodGet, "/admin/sessions/count", nil)
		req.Header.Set("Authorization", "Bearer "+manager.adminToken)
		w := httptest.NewRecorder()
		sessionCountHandler(w, req)

		var resp struct {
			Users sessionTally `json:"users"`
		}
		json.NewDecoder(w.Body).Decode(&resp)
		if resp.Users.Provisioned != 4 || resp.Users.LoggedIn != 3 || resp.Users.Warm != 1 {
			t.Errorf("users = %+v, want provisioned 4 logged_in 3 warm 1", resp.Users)
		}
	})
}
