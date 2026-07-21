package main

import (
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

var (
	backendHTTPClient = &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
		},
		Timeout: 30 * time.Second,
	}
	sharedMediaHTTPClient = &http.Client{
		Transport: &baileysTransport{base: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		}},
		Timeout: 60 * time.Second,
	}
)

func configuredUnloadGrace() time.Duration {
	value := strings.TrimSpace(os.Getenv("SESSION_UNLOAD_GRACE"))
	if value == "" {
		return defaultUnloadGrace
	}
	grace, err := time.ParseDuration(value)
	if err != nil || grace <= 0 {
		log.Printf("Invalid SESSION_UNLOAD_GRACE=%q; using %s", value, defaultUnloadGrace)
		return defaultUnloadGrace
	}
	return grace
}

func (m *SessionManager) scheduleUnloadLocked(session *UserSession) {
	session.lifecycleMu.Lock()
	defer session.lifecycleMu.Unlock()
	if session.closed || session.sseListeners != 0 {
		return
	}
	if session.unloadTimer != nil {
		session.unloadTimer.Stop()
	}
	session.zeroListenersAt = time.Now()
	session.unloadTimer = time.AfterFunc(m.unloadGrace, func() {
		m.unloadIfIdle(session)
	})
}

func (m *SessionManager) unloadIfIdle(session *UserSession) {
	m.mu.Lock()
	current, exists := m.sessions[session.UserID]
	if !exists || current != session {
		m.mu.Unlock()
		return
	}
	session.lifecycleMu.Lock()
	eligible := !session.closed && session.sseListeners == 0 && time.Since(session.zeroListenersAt) >= m.unloadGrace
	if eligible {
		session.closed = true
		delete(m.sessions, session.UserID)
	}
	session.lifecycleMu.Unlock()
	m.mu.Unlock()
	if !eligible {
		return
	}

	session.close(false)
	activeSessions.Dec()
	sessionLifecycleTotal.WithLabelValues("unloaded").Inc()
	log.Printf("Unloaded idle WhatsApp session user=%d after %s without logout", session.UserID, m.unloadGrace)
}

func (m *SessionManager) acquireSSEListener(userID int) (*UserSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[userID]
	if !ok {
		return nil, false
	}
	session.lifecycleMu.Lock()
	defer session.lifecycleMu.Unlock()
	if session.closed {
		return nil, false
	}
	if session.unloadTimer != nil {
		session.unloadTimer.Stop()
		session.unloadTimer = nil
	}
	session.sseListeners++
	activeSSEListeners.Inc()
	return session, true
}

func (m *SessionManager) releaseSSEListener(session *UserSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[session.UserID] != session {
		return
	}
	session.lifecycleMu.Lock()
	if session.sseListeners > 0 {
		session.sseListeners--
		activeSSEListeners.Dec()
	}
	shouldSchedule := !session.closed && session.sseListeners == 0
	session.lifecycleMu.Unlock()
	if shouldSchedule {
		m.scheduleUnloadLocked(session)
	}
}

func (s *UserSession) beginEvent() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closed {
		return false
	}
	s.eventWG.Add(1)
	return true
}

func (s *UserSession) close(logout bool) {
	s.closeOnce.Do(func() {
		if s.evictCancel != nil {
			s.evictCancel()
		}
		s.evictWG.Wait()

		s.Client.RemoveEventHandlers()
		if logout && s.Client.IsLoggedIn() {
			if err := s.Client.Logout(); err != nil {
				log.Printf("Logout failed during session deletion user=%d: %v; disconnecting", s.UserID, err)
				s.Client.Disconnect()
			}
		} else {
			s.Client.Disconnect()
		}
		s.eventWG.Wait()
		if s.Container != nil {
			if err := s.Container.Close(); err != nil {
				log.Printf("Failed to close session database user=%d: %v", s.UserID, err)
			}
		}

		s.MediaMu.Lock()
		entries, bytes := len(s.MediaCache), s.mediaCacheSize
		clear(s.MediaCache)
		s.mediaCacheSize = 0
		s.MediaMu.Unlock()
		if entries > 0 {
			mediaCacheEntries.Sub(float64(entries))
			mediaCacheBytes.Sub(float64(bytes))
			mediaCacheEvictionsTotal.WithLabelValues("session_close").Add(float64(entries))
		}
		s.PendingRetriesMu.Lock()
		clear(s.PendingRetries)
		s.PendingRetriesMu.Unlock()
	})
}
