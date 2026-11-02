package session

import (
	"math/rand/v2"
	"time"
)

// whatsmeow only auto-reconnects a connection that once existed, so the relay
// retries a paired session whose own connect attempt failed (boot restore,
// Start/Resume, ConnectFailure, ClientOutdated, ban expiry) until it connects,
// is paused, logged out, retired or the relay shuts down.
var reconnectBackoff = []time.Duration{
	5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second,
}

const (
	reconnectCap    = 300 * time.Second
	reconnectJitter = 0.2
	// disconnectGrace is how long a session must stay down before Frappe is
	// told, so short blips don't flap the account.
	disconnectGrace = 60 * time.Second
)

// reconnectDelay is the wait before retry attempt n (from 0), ±20 % jitter;
// rnd returns a value in [0, 1).
func reconnectDelay(attempt int, rnd func() float64) time.Duration {
	base := reconnectCap
	if attempt < len(reconnectBackoff) {
		base = reconnectBackoff[attempt]
	}
	return time.Duration(float64(base) * (1 - reconnectJitter + 2*reconnectJitter*rnd()))
}

func (m *Manager) random() float64 {
	if m.rnd != nil {
		return m.rnd()
	}
	return rand.Float64()
}

// dial connects the paired client. On failure the session is disconnected and
// a retry is scheduled.
func (s *Session) dial() error {
	s.setStatus(StatusConnecting)
	s.armDownTimer()
	if s.client.IsConnected() { // a socket left over from a failed login
		s.client.Disconnect()
	}
	err := s.client.Connect()
	if s.retired() { // retired (e.g. Shutdown) while connecting
		s.client.Disconnect()
		return err
	}
	if err != nil {
		s.setStatus(StatusDisconnected)
		s.scheduleReconnect()
	}
	return err
}

// scheduleReconnect starts the next retry unless one is already pending, so
// a session never has two loops.
func (s *Session) scheduleReconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil || s.retryStop != nil {
		return
	}
	delay := reconnectDelay(s.retryAttempt, s.m.random)
	s.retryAttempt++
	s.retryGen++
	gen := s.retryGen
	s.retryStop = s.m.afterFunc(delay, func() { s.reconnect(gen) })
	s.log.Info("connect failed; retrying", "attempt", s.retryAttempt, "in", delay.Round(time.Second).String())
}

func (s *Session) reconnect(gen int) {
	unlock := s.m.lockID(s.id)
	defer unlock()
	s.mu.Lock()
	current := gen == s.retryGen && s.retryStop != nil
	if current {
		s.retryStop = nil
	}
	status := s.status
	s.mu.Unlock()
	if !current || s.retired() || s.m.isClosed() || status != StatusDisconnected {
		return
	}
	if err := s.dial(); err != nil {
		s.log.Warn("reconnect failed", "error", err)
	}
}

// armDownTimer starts the grace period after which Frappe is told the
// session is Disconnected, unless one is running or this outage was reported.
func (s *Session) armDownTimer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil || s.downStop != nil || s.downReported {
		return
	}
	s.downGen++
	gen := s.downGen
	s.downStop = s.m.afterFunc(disconnectGrace, func() { s.downTimeout(gen) })
}

func (s *Session) downTimeout(gen int) {
	unlock := s.m.lockID(s.id)
	defer unlock()
	socketUp := s.client.IsConnected()
	s.mu.Lock()
	if gen != s.downGen || s.downStop == nil || s.ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	s.downStop = nil
	switch {
	case s.status == StatusConnected && socketUp:
		s.mu.Unlock()
		return
	case s.status == StatusConnected, s.status == StatusConnecting, s.status == StatusDisconnected:
	default: // paused, logged out, banned, pairing: reported elsewhere
		s.mu.Unlock()
		return
	}
	s.status = StatusDisconnected
	s.downReported = true
	s.mu.Unlock()
	s.log.Warn("not connected for the grace period; reporting Disconnected", "grace", disconnectGrace.String())
	s.pushStatus(frappeDisconnected)
}

// stopReconnectLocked cancels a pending retry and grace timer and forgets
// the outage. The caller holds s.mu.
func (s *Session) stopReconnectLocked() {
	if s.retryStop != nil {
		s.retryStop()
		s.retryStop = nil
	}
	if s.downStop != nil {
		s.downStop()
		s.downStop = nil
	}
	s.retryAttempt = 0
	s.downReported = false
}

func (s *Session) stopReconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopReconnectLocked()
}

// stopTimers stops every timer the session owns.
func (s *Session) stopTimers() {
	s.stopBanTimer()
	s.stopReconnect()
}
