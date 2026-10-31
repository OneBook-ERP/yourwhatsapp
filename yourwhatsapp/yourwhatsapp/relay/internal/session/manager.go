package session

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"

	"yourwhatsapp/relay/internal/config"
	"yourwhatsapp/relay/internal/outbox"
	"yourwhatsapp/relay/internal/phone"
	"yourwhatsapp/relay/internal/store"
)

// /message/send error codes (spec §7).
const (
	CodeBadRequest         = "bad_request"
	CodeInvalidNumber      = "invalid_number"
	CodeSessionUnavailable = "session_unavailable"
	CodeTempBanned         = "temp_banned"
	CodeDuplicate          = "duplicate"
	CodeInternal           = "internal"
)

const (
	defaultQueueCapacity = 50
	inFlightWait         = 10 * time.Second
)

var (
	ErrUnknownSession = errors.New("unknown session")
	ErrAlreadyPaired  = errors.New("session is already paired")
	ErrPaused         = errors.New("session is paused")
	ErrShuttingDown   = errors.New("relay is shutting down")
)

// SendRequest is the body of POST /message/send.
type SendRequest struct {
	SessionID, To, ContentType, Message, LocalID, AttachBase64, AttachMimetype, AttachFilename string
	ToIsLID                                                                                    bool
}

// SendError is a synchronous /message/send outcome other than 202. Code
// duplicate (HTTP 200) carries the Existing record.
type SendError struct {
	Code     string
	HTTP     int
	Detail   string
	Existing *store.SendRecord
}

func (e *SendError) Error() string {
	if e.Detail == "" {
		return e.Code
	}
	return e.Code + ": " + e.Detail
}

// Manager owns every account's Session.
type Manager struct {
	st      *store.Store
	enqueue func(ctx context.Context, method string, payload any) error
	factory ClientFactory
	cfg     config.Config
	log     *slog.Logger

	// Seams for tests; NewManager sets production values.
	sleep         func(time.Duration) // nil: real, shutdown-interruptible sleep
	rnd           func() float64      // nil: math/rand
	now           func() time.Time
	afterFunc     func(time.Duration, func()) (stop func() bool)
	queueCapacity int

	mu       sync.Mutex
	sessions map[string]*Session
	idLocks  map[string]*sync.Mutex
	closed   bool
}

var deviceIdentityOnce sync.Once

// setDeviceIdentity makes linked devices present as Chrome on Linux (§8.7).
func setDeviceIdentity() {
	deviceIdentityOnce.Do(func() {
		wastore.DeviceProps.PlatformType = waCompanionReg.DeviceProps_CHROME.Enum()
		wastore.SetOSInfo("Linux", [3]uint32{0, 1, 0})
	})
}

// NewManager builds a Manager. enqueue is the only path to Frappe.
func NewManager(st *store.Store, enqueue func(ctx context.Context, method string, payload any) error,
	factory ClientFactory, cfg config.Config, log *slog.Logger) *Manager {
	setDeviceIdentity()
	return &Manager{
		st: st, enqueue: enqueue, factory: factory, cfg: cfg, log: log,
		now: time.Now,
		afterFunc: func(d time.Duration, f func()) func() bool {
			return time.AfterFunc(d, f).Stop
		},
		queueCapacity: defaultQueueCapacity,
		sessions:      map[string]*Session{},
		idLocks:       map[string]*sync.Mutex{},
	}
}

// lockID serializes lifecycle operations on one session id.
func (m *Manager) lockID(id string) (unlock func()) {
	m.mu.Lock()
	l, ok := m.idLocks[id]
	if !ok {
		l = &sync.Mutex{}
		m.idLocks[id] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (m *Manager) session(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// RestoreAll fails sends lost with the previous process, then connects every
// paired, unpaused account.
func (m *Manager) RestoreAll(ctx context.Context) error {
	if err := m.failLostSends(ctx); err != nil {
		return err
	}
	accounts, err := m.st.ListRestorable(ctx)
	if err != nil {
		return fmt.Errorf("list restorable accounts: %w", err)
	}
	for _, a := range accounts {
		unlock := m.lockID(a.SessionID)
		_, err := m.start(ctx, a.SessionID, nil, false)
		unlock()
		if err != nil {
			m.log.Error("restore session", "session_id", a.SessionID, "error", err)
		}
	}
	return nil
}

// failLostSends reports every send still queued from the previous process as
// Failed. Each row is marked failed only after its callback is in the outbox,
// so an error leaves the remaining rows queued for the next boot.
func (m *Manager) failLostSends(ctx context.Context) error {
	queued, err := m.st.ListQueued(ctx)
	if err != nil {
		return fmt.Errorf("list queued sends: %w", err)
	}
	for _, r := range queued {
		if err := m.enqueue(ctx, outbox.MethodStatus, statusPayload{
			LocalMessageID: r.LocalID, Status: "Failed", ErrorCode: store.ErrCodeSendFailed,
			ErrorReason: "relay restarted before the message was sent",
		}); err != nil {
			return fmt.Errorf("enqueue failed send %s: %w", r.LocalID, err)
		}
		if _, err := m.st.MarkFailed(ctx, r.LocalID, r.SessionID, store.ErrCodeSendFailed); err != nil {
			return fmt.Errorf("mark send %s failed: %w", r.LocalID, err)
		}
	}
	return nil
}

// Start connects a paired account or begins QR pairing for an unpaired one.
// It is idempotent for a running session and rebuilds a dead one.
func (m *Manager) Start(ctx context.Context, sessionID string, proxy *string) (string, error) {
	unlock := m.lockID(sessionID)
	defer unlock()
	return m.start(ctx, sessionID, proxy, true)
}

// start does Start's work under the id lock. interactive=false (boot restore)
// never begins QR pairing.
func (m *Manager) start(ctx context.Context, id string, proxy *string, interactive bool) (string, error) {
	if s := m.session(id); s != nil {
		if s.live() {
			return s.Status(), nil
		}
		m.retire(s)
	}
	acct, err := m.prepareAccount(ctx, id, proxy)
	if err != nil {
		return "", err
	}
	if acct.Paused {
		return StatusPaused, nil
	}
	dev, err := m.loadDevice(ctx, acct)
	if err != nil {
		return "", err
	}
	if dev.ID == nil && !interactive {
		m.log.Warn("paired account has no device in the store; marking logged out", "session_id", id)
		m.setInactive(id, StatusLoggedOut, types.EmptyJID)
		return StatusLoggedOut, m.enqueue(ctx, outbox.MethodQR, qrPayload{SessionID: id, Status: frappeDisconnected})
	}
	s, err := m.newSession(id, dev, acct.Proxy)
	if err != nil {
		return "", err
	}
	if dev.ID == nil {
		return s.startQR()
	}
	return s.connect()
}

// PairCode begins phone-number pairing for an unpaired account and returns
// the code the user types on their phone.
func (m *Manager) PairCode(ctx context.Context, sessionID, rawPhone string, proxy *string) (string, error) {
	digits, err := phone.Normalize(rawPhone)
	if err != nil {
		return "", err
	}
	unlock := m.lockID(sessionID)
	defer unlock()
	if s := m.session(sessionID); s != nil {
		switch s.Status() {
		case StatusConnected, StatusConnecting, StatusTempBanned:
			if !s.retired() {
				return "", ErrAlreadyPaired
			}
		}
		m.retire(s)
	}
	acct, err := m.prepareAccount(ctx, sessionID, proxy)
	if err != nil {
		return "", err
	}
	if acct.Paused {
		return "", ErrPaused
	}
	if acct.JID != "" {
		return "", ErrAlreadyPaired
	}
	s, err := m.newSession(sessionID, m.st.Container.NewDevice(), acct.Proxy)
	if err != nil {
		return "", err
	}
	return s.pairWithCode(ctx, digits)
}

// Status reports a session's status and paired JID/phone, falling back to the
// stored account when no session is in memory.
func (m *Manager) Status(sessionID string) (status, jid, phoneNumber string) {
	if s := m.session(sessionID); s != nil {
		st, j := s.identity()
		if j.IsEmpty() {
			return st, "", ""
		}
		return st, j.ToNonAD().String(), j.User
	}
	acct, found, err := m.st.GetAccount(context.Background(), sessionID)
	if err != nil {
		m.log.Error("read account", "session_id", sessionID, "error", err)
		return StatusNotStarted, "", ""
	}
	status = StatusNotStarted
	switch {
	case !found:
	case acct.Paused:
		status = StatusPaused
	case acct.JID != "":
		status = StatusDisconnected
	}
	if j, err := types.ParseJID(acct.JID); err == nil && acct.JID != "" {
		return status, j.ToNonAD().String(), j.User
	}
	return status, "", ""
}

// Pause disconnects the account and keeps it down across restarts.
func (m *Manager) Pause(ctx context.Context, sessionID string) error {
	unlock := m.lockID(sessionID)
	defer unlock()
	acct, found, err := m.st.GetAccount(ctx, sessionID)
	if err != nil {
		return err
	}
	if !found {
		return ErrUnknownSession
	}
	if err := m.st.SetAccountPaused(ctx, sessionID, true); err != nil {
		return err
	}
	if s := m.session(sessionID); s != nil {
		m.retire(s)
	}
	jid, _ := types.ParseJID(acct.JID)
	m.setInactive(sessionID, StatusPaused, jid)
	return nil
}

// Resume clears the pause and connects.
func (m *Manager) Resume(ctx context.Context, sessionID string, proxy *string) error {
	unlock := m.lockID(sessionID)
	defer unlock()
	if err := m.requireAccount(ctx, sessionID); err != nil {
		return err
	}
	if err := m.st.SetAccountPaused(ctx, sessionID, false); err != nil {
		return err
	}
	_, err := m.start(ctx, sessionID, proxy, true)
	return err
}

// Logout unlinks the device, deletes its store and clears the account's JID.
// If WhatsApp can't be reached the local device data is still removed.
func (m *Manager) Logout(ctx context.Context, sessionID string) error {
	unlock := m.lockID(sessionID)
	defer unlock()
	acct, found, err := m.st.GetAccount(ctx, sessionID)
	if err != nil {
		return err
	}
	if !found {
		return ErrUnknownSession
	}
	if s := m.session(sessionID); s != nil {
		if s.client != nil && !s.retired() && s.client.IsLoggedIn() {
			if err := s.client.Logout(ctx); err != nil {
				m.log.Warn("logout request failed; deleting local device data anyway", "session_id", sessionID, "error", err)
			}
		}
		m.retire(s)
	}
	if err := m.deleteDevice(ctx, acct.JID); err != nil {
		return err
	}
	if err := m.st.SetAccountJID(ctx, sessionID, ""); err != nil {
		return err
	}
	m.setInactive(sessionID, StatusLoggedOut, types.EmptyJID)
	return m.enqueue(ctx, outbox.MethodQR, qrPayload{SessionID: sessionID, Status: frappeDisconnected})
}

// Send validates and queues a message (spec §8.3 synchronous part). nil
// means accepted.
func (m *Manager) Send(ctx context.Context, req SendRequest) *SendError {
	media, serr := validateSend(req)
	if serr != nil {
		return serr
	}
	if serr := m.duplicateOf(m.st.GetSend(ctx, req.LocalID)); serr != nil {
		return serr
	}
	s := m.session(req.SessionID)
	if s == nil {
		return &SendError{Code: CodeSessionUnavailable, HTTP: http.StatusConflict, Detail: "session is not running"}
	}
	if code, detail := s.sendBlocker(); code != "" {
		return blockedError(code, detail)
	}
	to, serr := s.recipient(ctx, req)
	if serr != nil {
		return serr
	}
	if s.queue.full() {
		return &SendError{Code: CodeSessionUnavailable, HTTP: http.StatusConflict, Detail: "send queue is full"}
	}
	if serr := m.duplicateOf(m.st.ReserveSend(ctx, req.LocalID, s.id)); serr != nil {
		return serr
	}
	job := &sendJob{
		localID: req.LocalID, to: to, contentType: req.ContentType, text: req.Message,
		media: media, mimetype: req.AttachMimetype, filename: req.AttachFilename,
	}
	if media != nil {
		if job.mimetype == "" {
			job.mimetype = http.DetectContentType(media)
		}
		if job.filename == "" {
			job.filename = "file"
		}
	}
	if !s.queue.push(job) {
		if _, err := m.st.MarkFailed(ctx, req.LocalID, s.id, CodeSessionUnavailable); err != nil {
			m.log.Error("release send reservation", "session_id", s.id, "local_message_id", req.LocalID, "error", err)
		}
		return &SendError{Code: CodeSessionUnavailable, HTTP: http.StatusConflict, Detail: "send queue is full or stopped"}
	}
	return nil
}

func validateSend(req SendRequest) (media []byte, serr *SendError) {
	bad := func(detail string) *SendError {
		return &SendError{Code: CodeBadRequest, HTTP: http.StatusBadRequest, Detail: detail}
	}
	switch {
	case req.SessionID == "":
		return nil, bad("session_id is required")
	case req.To == "":
		return nil, bad("to is required")
	case req.LocalID == "":
		return nil, bad("local_message_id is required")
	case req.Message == "" && req.AttachBase64 == "":
		return nil, bad("message or attachment is required")
	}
	if req.AttachBase64 == "" {
		return nil, nil
	}
	media, err := base64.StdEncoding.DecodeString(req.AttachBase64)
	if err != nil {
		return nil, bad("attach_base64 is not valid base64")
	}
	return media, nil
}

// duplicateOf turns a lookup/reservation result into the duplicate response.
func (m *Manager) duplicateOf(rec *store.SendRecord, err error) *SendError {
	if err != nil {
		m.log.Error("read send record", "error", err)
		return &SendError{Code: CodeInternal, HTTP: http.StatusInternalServerError, Detail: "send record lookup failed"}
	}
	if rec == nil || rec.State == store.SendFailed {
		return nil
	}
	return &SendError{Code: CodeDuplicate, HTTP: http.StatusOK, Detail: "already " + rec.State, Existing: rec}
}

func blockedError(code, detail string) *SendError {
	if code == CodeTempBanned {
		return &SendError{Code: code, HTTP: http.StatusTooManyRequests, Detail: detail}
	}
	return &SendError{Code: code, HTTP: http.StatusConflict, Detail: detail}
}

// recipient resolves the send target: a LID directly, or a checked number.
func (s *Session) recipient(ctx context.Context, req SendRequest) (types.JID, *SendError) {
	invalid := &SendError{Code: CodeInvalidNumber, HTTP: http.StatusUnprocessableEntity, Detail: "unparseable number"}
	if req.ToIsLID {
		digits := onlyDigits(req.To)
		if digits == "" {
			return types.EmptyJID, invalid
		}
		return types.NewJID(digits, types.HiddenUserServer), nil
	}
	digits, err := phone.Normalize(req.To)
	if err != nil {
		return types.EmptyJID, invalid
	}
	return s.checkNumber(ctx, digits)
}

func onlyDigits(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			out = append(out, s[i])
		}
	}
	return string(out)
}

// Shutdown stops every queue (waiting up to 10 s for an in-flight send) and
// disconnects every client, all accounts in parallel. Jobs still waiting stay
// queued in the store and are failed over at the next boot.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.stopBanTimer()
			if s.queue != nil {
				s.queue.shutdown(inFlightWait)
			}
			if s.cancel != nil {
				s.cancel()
			}
			if s.client != nil {
				s.client.Disconnect()
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// prepareAccount creates the account row if needed, stores the proxy when
// Frappe sent one (empty clears it), and returns the account.
func (m *Manager) prepareAccount(ctx context.Context, id string, proxy *string) (store.Account, error) {
	if err := m.st.UpsertAccount(ctx, id); err != nil {
		return store.Account{}, err
	}
	if proxy != nil {
		if err := m.st.SetAccountProxy(ctx, id, *proxy); err != nil {
			return store.Account{}, err
		}
	}
	acct, _, err := m.st.GetAccount(ctx, id)
	return acct, err
}

func (m *Manager) requireAccount(ctx context.Context, id string) error {
	_, found, err := m.st.GetAccount(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		return ErrUnknownSession
	}
	return nil
}

// loadDevice returns the account's stored device, or a new unpaired one when
// it has none. A JID whose device is gone is cleared.
func (m *Manager) loadDevice(ctx context.Context, acct store.Account) (*wastore.Device, error) {
	if acct.JID == "" {
		return m.st.Container.NewDevice(), nil
	}
	jid, err := types.ParseJID(acct.JID)
	if err != nil {
		return nil, fmt.Errorf("parse stored jid: %w", err)
	}
	dev, err := m.st.Container.GetDevice(ctx, jid)
	if err != nil {
		return nil, fmt.Errorf("load device: %w", err)
	}
	if dev != nil {
		return dev, nil
	}
	if err := m.st.SetAccountJID(ctx, acct.SessionID, ""); err != nil {
		return nil, err
	}
	return m.st.Container.NewDevice(), nil
}

func (m *Manager) deleteDevice(ctx context.Context, jidStr string) error {
	if jidStr == "" {
		return nil
	}
	jid, err := types.ParseJID(jidStr)
	if err != nil {
		return fmt.Errorf("parse stored jid: %w", err)
	}
	dev, err := m.st.Container.GetDevice(ctx, jid)
	if err != nil || dev == nil {
		return err
	}
	return dev.Delete(ctx)
}

// newSession builds a Session around a fresh client and registers it.
func (m *Manager) newSession(id string, dev *wastore.Device, proxy string) (*Session, error) {
	if m.isClosed() {
		return nil, ErrShuttingDown
	}
	client := m.factory(dev)
	if proxy != "" {
		if err := client.SetProxyAddress(proxy); err != nil {
			return nil, fmt.Errorf("set proxy: %w", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		id: id, m: m, log: m.log.With("session_id", id), dev: dev, client: client,
		queue:  newSendQueue(m.queueCapacity, m.cfg.SendDelayMin, m.cfg.SendDelayMax, m.sleep, m.rnd),
		ctx:    ctx,
		cancel: cancel,
		status: StatusNotStarted, numbers: map[string]numberEntry{},
	}
	if dev.ID != nil {
		s.jid = *dev.ID
	}
	client.AddEventHandler(s.handleEvent)
	s.queue.start(s.deliver)
	// Re-check under the same lock as registration: a Shutdown that ran
	// while the client was being built would otherwise never stop it.
	m.mu.Lock()
	closed := m.closed
	if !closed {
		m.sessions[id] = s
	}
	m.mu.Unlock()
	if closed {
		m.retire(s)
		return nil, ErrShuttingDown
	}
	return s, nil
}

func (m *Manager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// retire stops a session: its events are ignored from now on, waiting jobs
// are failed so Frappe fails them over, and the client is disconnected.
func (m *Manager) retire(s *Session) {
	if s.cancel != nil {
		s.cancel()
	}
	s.stopBanTimer()
	if s.queue != nil {
		for _, job := range s.queue.shutdown(inFlightWait) {
			s.finishFailed(job, CodeSessionUnavailable, "session stopped")
		}
	}
	if s.client != nil {
		s.client.Disconnect()
	}
}

// setInactive records a terminal status for an id with no running client.
func (m *Manager) setInactive(id, status string, jid types.JID) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &Session{id: id, m: m, log: m.log.With("session_id", id), ctx: ctx, status: status, jid: jid}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
}
