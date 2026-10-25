package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testMigratedPhone = "+15550002222"

// writeNumberHistoryFixture drops one inbox and one delivery-receipt file under
// the number id so a migration can be checked for preserving user history.
func writeNumberHistoryFixture(t *testing.T, m *SessionManager, numberID int) (inboxPath, receiptPath string) {
	t.Helper()
	inboxDir := filepath.Join(m.dataDir, "linq_number_inbox", "2")
	receiptDir := filepath.Join(m.dataDir, "linq_number_delivery_receipts", "2")
	if err := os.MkdirAll(inboxDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(receiptDir, 0700); err != nil {
		t.Fatal(err)
	}
	inboxPath = filepath.Join(inboxDir, "conversation.json")
	receiptPath = filepath.Join(receiptDir, "receipt.json")
	if err := os.WriteFile(inboxPath, []byte(`{"kept":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receiptPath, []byte(`{"kept":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	return inboxPath, receiptPath
}

func TestMigrateLinqNumberRepointsPhoneAndPreservesHistory(t *testing.T) {
	m := setupTestManager(t)

	firstToken, err := m.registerLinqNumber(linqNumberRecord{ID: 2, Phone: testPairingPhone})
	if err != nil {
		t.Fatal(err)
	}
	// Create the session (and its database) so the migration has an old,
	// now-invalid device credential to clear.
	numberState, err := m.getOrCreateLinqNumberSession(2)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := numberState.session.DBPath
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("number database not created: %v", err)
	}
	inboxPath, receiptPath := writeNumberHistoryFixture(t, m, 2)

	oldPhone, newPhone, migrateToken, err := m.migrateLinqNumber(2, "+1 (555) 000-2222")
	if err != nil {
		t.Fatal(err)
	}
	if oldPhone != testPairingPhone || newPhone != testMigratedPhone {
		t.Fatalf("migrate reported old=%q new=%q", oldPhone, newPhone)
	}
	if migrateToken == firstToken {
		t.Fatal("migration must issue a fresh pairing capability")
	}

	// The id is preserved; only the registered phone changes.
	if m.numberSessions[2] == nil || m.numberSessions[2].Phone != testMigratedPhone {
		t.Fatalf("in-memory registry not repointed: %#v", m.numberSessions[2])
	}
	if m.numberSessions[2].session != nil {
		t.Fatal("old device session must be cleared for re-pairing")
	}

	// The change is persisted to the on-disk registry under the same id.
	data, err := os.ReadFile(filepath.Join(m.dataDir, linqNumberRegistryFile))
	if err != nil {
		t.Fatal(err)
	}
	var records []linqNumberRecord
	if err := json.Unmarshal(data, &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != 2 || records[0].Phone != testMigratedPhone {
		t.Fatalf("persisted registry not repointed: %#v", records)
	}

	// The banned device credential is gone, but user-facing history survives.
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("old device database should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(inboxPath); err != nil {
		t.Fatalf("inbox history must be preserved across migration: %v", err)
	}
	if _, err := os.Stat(receiptPath); err != nil {
		t.Fatalf("delivery-receipt history must be preserved across migration: %v", err)
	}
}

func TestMigrateLinqNumberRejectsInvalidRequests(t *testing.T) {
	m := setupTestManager(t)
	if _, err := m.registerLinqNumber(linqNumberRecord{ID: 2, Phone: testPairingPhone}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.registerLinqNumber(linqNumberRecord{ID: 3, Phone: testMigratedPhone}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		id      int
		phone   string
		wantSub string
	}{
		{"unregistered id", 99, "+15550009999", "not registered"},
		{"same phone", 2, testPairingPhone, "matches the current"},
		{"phone owned by another id", 2, testMigratedPhone, "already registered to another"},
		{"invalid phone", 2, "15550002222", "E.164 phone required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := m.migrateLinqNumber(tc.id, tc.phone)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("migrate(%d,%q) err = %v, want contains %q", tc.id, tc.phone, err, tc.wantSub)
			}
		})
	}
	// A rejected migration must not change the registered phone.
	if m.numberSessions[2].Phone != testPairingPhone {
		t.Fatalf("rejected migration mutated registry: %q", m.numberSessions[2].Phone)
	}
}

func TestMigrateLinqNumberHandler(t *testing.T) {
	manager = setupTestManager(t)
	manager.joBotInternalToken = testInternalToken
	if _, err := manager.registerLinqNumber(linqNumberRecord{ID: 2, Phone: testPairingPhone}); err != nil {
		t.Fatal(err)
	}
	const migratePath = "/internal/linq-numbers/2/migrate"
	body := `{"phone":"` + testMigratedPhone + `"}`

	// Unauthorized is rejected and leaves the phone unchanged.
	unauthorized := httptest.NewRecorder()
	linqNumberInternalHandler(unauthorized, internalNumberRequest(http.MethodPost, migratePath, body, ""))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401", unauthorized.Code)
	}
	if manager.numberSessions[2].Phone != testPairingPhone {
		t.Fatal("unauthorized migration changed state")
	}

	// Wrong method is a not-found on the internal surface.
	wrongMethod := httptest.NewRecorder()
	linqNumberInternalHandler(wrongMethod, internalNumberRequest(http.MethodGet, migratePath, "", testInternalToken))
	if wrongMethod.Code != http.StatusNotFound {
		t.Fatalf("GET status = %d, want 404", wrongMethod.Code)
	}

	// Missing phone is a bad request.
	missing := httptest.NewRecorder()
	linqNumberInternalHandler(missing, internalNumberRequest(http.MethodPost, migratePath, `{"phone":""}`, testInternalToken))
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("missing-phone status = %d, want 400", missing.Code)
	}

	// Authorized migration returns a fresh pairing link and the id is preserved.
	response := httptest.NewRecorder()
	linqNumberInternalHandler(response, internalNumberRequest(http.MethodPost, migratePath, body, testInternalToken))
	if response.Code != http.StatusOK {
		t.Fatalf("migration status = %d, body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Status   string `json:"status"`
		OldPhone string `json:"old_phone"`
		NewPhone string `json:"new_phone"`
		URL      string `json:"pairing_url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "migrated" || result.OldPhone != testPairingPhone || result.NewPhone != testMigratedPhone {
		t.Fatalf("unexpected migration response: %#v", result)
	}
	if !strings.HasPrefix(result.URL, "/admin/linq-numbers/2/pair?token=") {
		t.Fatalf("missing pairing link: %#v", result)
	}
	if manager.numberSessions[2].Phone != testMigratedPhone {
		t.Fatalf("handler did not repoint registered phone: %q", manager.numberSessions[2].Phone)
	}
}
