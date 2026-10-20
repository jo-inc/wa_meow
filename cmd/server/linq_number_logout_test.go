package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

// waitForClearedNumberSession polls until the shared-number session is torn
// down (teardown runs asynchronously to avoid an eventWG deadlock).
func waitForClearedNumberSession(t *testing.T, m *SessionManager, numberID int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.RLock()
		cleared := m.numberSessions[numberID] != nil && m.numberSessions[numberID].session == nil
		m.mu.RUnlock()
		if cleared {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("number session was not invalidated after logout")
}

func TestNumberLoggedOutEventInvalidatesSession(t *testing.T) {
	m, client := setupLinqSendManager(t, testPairingPhone)
	session := m.numberSessions[2].session
	dbPath := session.DBPath
	if err := os.WriteFile(dbPath, []byte("stale-device"), 0600); err != nil {
		t.Fatal(err)
	}

	if !session.handleNumberEvent(&events.LoggedOut{}) {
		t.Fatal("logout event was not acknowledged")
	}
	waitForClearedNumberSession(t, m, 2)

	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatal("logged-out device database was not deleted")
	}
	if countMockCalls(client.GetCalls(), "Disconnect") == 0 {
		t.Fatal("logged-out session was not disconnected")
	}
	// The registry entry (id + phone) is retained so a fresh QR re-pairs it.
	if m.numberSessions[2] == nil || m.numberSessions[2].Phone != testPairingPhone {
		t.Fatal("logout dropped the number registry entry")
	}
	if m.numberSessions[2].pairingStarted {
		t.Fatal("logout left a stale pairing flag set")
	}
}

func TestForceRelinkNumberClearsStaleDevice(t *testing.T) {
	m, _ := setupLinqSendManager(t, testPairingPhone)
	// A device that still claims to be logged in locally but is dead remotely.
	session := m.numberSessions[2].session
	dbPath := session.DBPath
	if err := os.WriteFile(dbPath, []byte("stale-device"), 0600); err != nil {
		t.Fatal(err)
	}
	if !session.Client.IsLoggedIn() {
		t.Fatal("precondition: device should look logged in")
	}

	m.forceRelinkNumber(2)

	if m.numberSessions[2].session != nil {
		t.Fatal("force relink did not clear the stale session")
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatal("force relink did not delete the stale device database")
	}
	if m.numberSessions[2].Phone != testPairingPhone {
		t.Fatal("force relink dropped the number registry entry")
	}
}

func TestRelinkEndpointForcesFreshPairing(t *testing.T) {
	m, _ := setupLinqSendManager(t, testPairingPhone)
	token, err := m.registerLinqNumber(linqNumberRecord{ID: 2, Phone: testPairingPhone})
	if err != nil {
		t.Fatal(err)
	}
	// registerLinqNumber keeps the existing session; give it a device on disk.
	if session := m.numberSessions[2].session; session != nil && session.DBPath != "" {
		if err := os.WriteFile(session.DBPath, []byte("stale-device"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	response := httptest.NewRecorder()
	linqNumberAdminHandler(response, httptest.NewRequest(http.MethodGet, "/admin/linq-numbers/2/relink?token="+token, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("relink status = %d, body=%s", response.Code, response.Body.String())
	}
	var result map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["status"] != "relink_ready" {
		t.Fatalf("unexpected relink response: %#v", result)
	}
	if m.numberSessions[2].session != nil {
		t.Fatal("relink endpoint did not clear the session")
	}

	// An invalid token must not be able to destroy a device credential.
	bad := httptest.NewRecorder()
	linqNumberAdminHandler(bad, httptest.NewRequest(http.MethodGet, "/admin/linq-numbers/2/relink?token=invalid", nil))
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized relink status = %d, want 401", bad.Code)
	}
}

func TestPairingTokenInternalEndpointMintsUsableToken(t *testing.T) {
	m, _ := setupInternalNumberManager(t)
	response := httptest.NewRecorder()
	linqNumberInternalHandler(response, internalNumberRequest(http.MethodGet, "/internal/linq-numbers/2/pairing-token", "", testInternalToken))
	if response.Code != http.StatusOK {
		t.Fatalf("pairing-token status = %d, body=%s", response.Code, response.Body.String())
	}
	var result map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	token := result["token"]
	if token == "" {
		t.Fatalf("pairing-token response had no token: %s", response.Body.String())
	}
	if m.authorizePairingToken(2, token) == nil {
		t.Fatal("internally minted pairing token is not usable")
	}

	bad := httptest.NewRecorder()
	linqNumberInternalHandler(bad, internalNumberRequest(http.MethodGet, "/internal/linq-numbers/2/pairing-token", "", "wrong-token"))
	if bad.Code == http.StatusOK {
		t.Fatal("pairing-token served without a valid internal token")
	}
}

func TestMintedPairingTokenDrivesPairingPage(t *testing.T) {
	m, _ := setupInternalNumberManager(t)
	token, ok := m.mintPairingToken(2)
	if !ok {
		t.Fatal("mintPairingToken failed for registered number")
	}
	response := httptest.NewRecorder()
	linqNumberAdminHandler(response, httptest.NewRequest(http.MethodGet, "/admin/linq-numbers/2/pair?token="+token, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("pair page status = %d, body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "<") {
		t.Fatalf("pair page did not render HTML: %s", response.Body.String())
	}

	bad := httptest.NewRecorder()
	linqNumberAdminHandler(bad, httptest.NewRequest(http.MethodGet, "/admin/linq-numbers/2/pair?token=not-a-real-token", nil))
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("invalid pairing token status = %d, want 401", bad.Code)
	}
}
