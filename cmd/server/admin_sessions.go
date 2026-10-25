package main

import (
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// sessionTally is a read-only count of how many linked accounts the bridge
// holds versus how many are still logged in or actively connected.
type sessionTally struct {
	Registered  int `json:"registered,omitempty"` // entries in the number registry (shared numbers only)
	Provisioned int `json:"provisioned"`          // store files present on the volume
	LoggedIn    int `json:"logged_in"`            // device still registered (not logged out)
	Connected   int `json:"connected"`            // live WhatsApp socket up right now (warm sessions only)
	Warm        int `json:"warm"`                 // currently loaded in memory
	Unreadable  int `json:"unreadable"`           // files we could not inspect
}

// warmState is the live in-memory view of a loaded session.
type warmState struct {
	loggedIn  bool
	connected bool
}

// storeHasLoggedInDevice reports whether a whatsmeow store file still holds a
// registered device. When an account logs out (or never finishes pairing),
// whatsmeow deletes its row from whatsmeow_device, so an empty table means
// "not logged in". The store is opened read-only and WhatsApp is never
// contacted, so this is safe to run against idle (unloaded) sessions.
func storeHasLoggedInDevice(dbPath string) (bool, error) {
	// mode=ro prevents any write or schema migration; a short busy timeout
	// avoids hanging if the file is transiently locked.
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro&_busy_timeout=2000")
	if err != nil {
		return false, err
	}
	defer db.Close()

	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM whatsmeow_device WHERE jid IS NOT NULL AND jid != ''`).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// warmLoginSnapshot returns the in-memory state for every session currently
// loaded, keyed by user ID and by linq-number ID, plus the number of shared
// numbers in the registry. Reading live sessions from memory avoids opening a
// second connection to a database file an active session already holds.
func (m *SessionManager) warmLoginSnapshot() (users map[int]warmState, numbers map[int]warmState, registeredNumbers int) {
	users = make(map[int]warmState)
	numbers = make(map[int]warmState)
	m.mu.RLock()
	defer m.mu.RUnlock()
	registeredNumbers = len(m.numberSessions)
	for userID, session := range m.sessions {
		if session != nil && session.Client != nil {
			users[userID] = warmState{loggedIn: session.Client.IsLoggedIn(), connected: session.Client.IsConnected()}
		}
	}
	for numberID, state := range m.numberSessions {
		if state != nil && state.session != nil && state.session.Client != nil {
			numbers[numberID] = warmState{loggedIn: state.session.Client.IsLoggedIn(), connected: state.session.Client.IsConnected()}
		}
	}
	return users, numbers, registeredNumbers
}

// parseStoreID extracts the numeric ID from a store filename like
// "user_17.db" or "linq_number_3.db" given its prefix.
func parseStoreID(name, prefix string) (int, bool) {
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".db") {
		return 0, false
	}
	middle := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".db")
	id, err := strconv.Atoi(middle)
	if err != nil {
		return 0, false
	}
	return id, true
}

// sessionCountHandler reports, read-only, how many personal accounts and shared
// numbers are provisioned on the volume and how many are still logged in. It
// never connects to WhatsApp and never loads or mutates a session.
func sessionCountHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorResponse(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !requireAdmin(w, r) {
		return
	}

	entries, err := os.ReadDir(manager.dataDir)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "cannot read data directory")
		return
	}

	warmUsers, warmNumbers, registeredNumbers := manager.warmLoginSnapshot()
	var users, numbers sessionTally
	users.Warm = len(warmUsers)
	numbers.Warm = len(warmNumbers)
	numbers.Registered = registeredNumbers
	for _, s := range warmUsers {
		if s.connected {
			users.Connected++
		}
	}
	for _, s := range warmNumbers {
		if s.connected {
			numbers.Connected++
		}
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()

		if id, ok := parseStoreID(name, "user_"); ok {
			users.Provisioned++
			if warm, ok := warmUsers[id]; ok {
				if warm.loggedIn {
					users.LoggedIn++
				}
				continue
			}
			loggedIn, err := storeHasLoggedInDevice(filepath.Join(manager.dataDir, name))
			if err != nil {
				users.Unreadable++
			} else if loggedIn {
				users.LoggedIn++
			}
			continue
		}

		if id, ok := parseStoreID(name, "linq_number_"); ok {
			numbers.Provisioned++
			if warm, ok := warmNumbers[id]; ok {
				if warm.loggedIn {
					numbers.LoggedIn++
				}
				continue
			}
			loggedIn, err := storeHasLoggedInDevice(filepath.Join(manager.dataDir, name))
			if err != nil {
				numbers.Unreadable++
			} else if loggedIn {
				numbers.LoggedIn++
			}
			continue
		}
	}

	jsonResponse(w, map[string]interface{}{
		"users":          users,
		"shared_numbers": numbers,
		"generated_at":   time.Now().UTC().Format(time.RFC3339),
	})
}
