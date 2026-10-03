package main

// handleNumberEvent is the provider ACK boundary for shared-number sessions.
// Returning false tells whatsmeow to retain the decrypted event and withhold the
// provider receipt until durable inbox persistence succeeds.
func (s *UserSession) handleNumberEvent(evt interface{}) bool {
	if !s.beginEvent() {
		return false
	}
	defer s.eventWG.Done()
	return s.handleEvent(evt)
}
