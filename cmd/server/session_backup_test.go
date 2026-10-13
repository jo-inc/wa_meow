package main

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveSessionRequiresAcknowledgedConsistentSQLiteSnapshot(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	var saved string
	var acknowledgementOverride string
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/whatsapp/session" {
			t.Fatalf("unexpected backup request: %s %s", r.Method, r.URL.Path)
		}
		var payload struct {
			Data   string `json:"data"`
			SHA256 string `json:"sha256"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		saved = payload.Data
		w.WriteHeader(status)
		if status == http.StatusOK {
			acknowledgement := payload.SHA256
			if acknowledgementOverride != "" {
				acknowledgement = acknowledgementOverride
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"sha256": acknowledgement})
		}
	}))
	defer server.Close()

	manager = NewSessionManager(t.TempDir(), server.URL, key)
	client := NewLoggedInMockClient()
	injectMockSession(manager, 88, client)
	dbPath := filepath.Join(manager.dataDir, "user_88.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE snapshot_test (value TEXT); INSERT INTO snapshot_test VALUES ('durable');`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dbPath + "-wal"); err != nil {
		t.Fatalf("test must leave committed state in WAL before snapshot: %v", err)
	}
	defer db.Close()

	request := httptest.NewRequest(http.MethodPost, "/sessions/save?user_id=88", nil)
	response := httptest.NewRecorder()
	saveSessionHandler(response, request)
	if response.Code != http.StatusOK || saved == "" {
		t.Fatalf("acknowledged save = %d: %s", response.Code, response.Body.String())
	}
	snapshot, err := manager.decrypt(saved)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.db")
	if err := os.WriteFile(path, snapshot, 0600); err != nil {
		t.Fatal(err)
	}
	verified, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	var value string
	if err := verified.QueryRow(`SELECT value FROM snapshot_test`).Scan(&value); err != nil || value != "durable" {
		t.Fatalf("backup was not a usable SQLite snapshot: value=%q err=%v", value, err)
	}

	acknowledgementOverride = "wrong-snapshot"
	response = httptest.NewRecorder()
	saveSessionHandler(response, httptest.NewRequest(http.MethodPost, "/sessions/save?user_id=88", nil))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("mismatched upstream acknowledgement = %d, want 502", response.Code)
	}

	acknowledgementOverride = ""
	status = http.StatusBadGateway
	response = httptest.NewRecorder()
	saveSessionHandler(response, httptest.NewRequest(http.MethodPost, "/sessions/save?user_id=88", nil))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("non-2xx upstream acknowledgement = %d, want 502", response.Code)
	}
}
