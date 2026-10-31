package session

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"yourwhatsapp/relay/internal/convert"
	"yourwhatsapp/relay/internal/outbox"
	"yourwhatsapp/relay/internal/store"
)

// Relay-internal session statuses (spec §7).
const (
	StatusNotStarted   = "not_started"
	StatusQRPending    = "qr_pending"
	StatusPairing      = "pairing"
	StatusConnecting   = "connecting"
	StatusConnected    = "connected"
	StatusDisconnected = "disconnected"
	StatusLoggedOut    = "logged_out"
	StatusPaused       = "paused"
	StatusTempBanned   = "temp_banned"
)

// Frappe-facing account statuses carried by update_qr.
const (
	frappeActive       = "Active"
	frappeDisconnected = "Disconnected"
)

const (
	numberCacheTTL   = 24 * time.Hour
	downloadTimeout  = 2 * time.Minute
	sendTimeout      = 2 * time.Minute
	callbackTimeout  = 10 * time.Second
	unknownBanExpiry = time.Hour

	unconfirmedReason = "send not confirmed by WhatsApp; may have been delivered"
)

type statusPayload struct {
	LocalMessageID string `json:"local_message_id,omitempty"`
	MessageID      string `json:"message_id,omitempty"`
	Status         string `json:"status"`
	ErrorCode      string `json:"error_code,omitempty"`
	ErrorReason    string `json:"error_reason,omitempty"`
}

type qrPayload struct {
	SessionID string `json:"session_id"`
	QRBase64  string `json:"qr_base64,omitempty"`
	Status    string `json:"status,omitempty"`
}

type accountEventPayload struct {
	SessionID string `json:"session_id"`
	Event     string `json:"event"`
	Until     string `json:"until"`
}

type numberEntry struct {
	jid     types.JID
	expires time.Time
}

// Session is one account's client, send queue and connection state. A
// retired Session (ctx cancelled) ignores client events and is replaced by
// the next Start; one without a client only records a terminal status.
type Session struct {
	id     string
	m      *Manager
	log    *slog.Logger
	dev    *wastore.Device
	client WAClient
	queue  *sendQueue
	ctx    context.Context
	cancel context.CancelFunc

	// sendMu is held from SendMessage until the Sent callback is enqueued,
	// and by onReceipt, so a receipt can never reach the outbox before the
	// Sent for its message.
	sendMu sync.Mutex

	mu       sync.Mutex
	status   string
	jid      types.JID
	banUntil time.Time
	stopBan  func() bool
	numbers  map[string]numberEntry
}

func (s *Session) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *Session) setStatus(status string) (prev string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, s.status = s.status, status
	return prev
}

func (s *Session) identity() (status string, jid types.JID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.jid
}

// live reports whether the session has a running client that Start should
// leave alone rather than rebuild.
func (s *Session) live() bool {
	switch s.Status() {
	case StatusQRPending, StatusPairing, StatusConnecting, StatusConnected, StatusTempBanned:
		return s.ctx.Err() == nil
	}
	return false
}

func (s *Session) retired() bool { return s.ctx.Err() != nil }

func (s *Session) stopBanTimer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopBan != nil {
		s.stopBan()
		s.stopBan = nil
	}
}

// connect dials a paired device; Connected arrives later as an event.
func (s *Session) connect() (string, error) {
	s.setStatus(StatusConnecting)
	if err := s.client.Connect(); err != nil {
		s.setStatus(StatusDisconnected)
		return StatusDisconnected, err
	}
	return s.Status(), nil
}

// startQR opens the QR channel, connects, and streams codes to Frappe.
func (s *Session) startQR() (string, error) {
	ch, err := s.client.GetQRChannel(s.ctx)
	if err != nil {
		return StatusDisconnected, err
	}
	s.setStatus(StatusQRPending)
	go s.consumeQR(ch, true)
	if err := s.client.Connect(); err != nil {
		s.setStatus(StatusDisconnected)
		return StatusDisconnected, err
	}
	return s.Status(), nil
}

// pairWithCode connects with a QR channel open (whatsmeow requires it), waits
// for the login websocket to be ready, then requests a phone pairing code.
func (s *Session) pairWithCode(ctx context.Context, digits string) (string, error) {
	ch, err := s.client.GetQRChannel(s.ctx)
	if err != nil {
		return "", err
	}
	s.setStatus(StatusPairing)
	if err := s.client.Connect(); err != nil {
		s.setStatus(StatusDisconnected)
		return "", err
	}
	select {
	case item, ok := <-ch:
		if !ok || item.Event != whatsmeow.QRChannelEventCode {
			s.setStatus(StatusDisconnected)
			return "", &pairingError{event: item.Event}
		}
	case <-ctx.Done():
		s.setStatus(StatusDisconnected)
		return "", ctx.Err()
	}
	go s.consumeQR(ch, false)
	code, err := s.client.PairPhone(ctx, "+"+digits, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		s.setStatus(StatusDisconnected)
		s.client.Disconnect()
		return "", err
	}
	return code, nil
}

type pairingError struct{ event string }

func (e *pairingError) Error() string {
	return "pairing websocket not ready (qr channel event " + e.event + ")"
}

// consumeQR handles QR channel items until it closes or the session retires.
// Success is handled by the PairSuccess event.
func (s *Session) consumeQR(ch <-chan whatsmeow.QRChannelItem, pushCodes bool) {
	for {
		select {
		case <-s.ctx.Done():
			return
		case item, ok := <-ch:
			if !ok {
				return
			}
			s.handleQRItem(item, pushCodes)
		}
	}
}

func (s *Session) handleQRItem(item whatsmeow.QRChannelItem, pushCodes bool) {
	switch item.Event {
	case whatsmeow.QRChannelEventCode:
		if pushCodes {
			s.pushQR(item.Code)
		}
	case whatsmeow.QRChannelSuccess.Event, whatsmeow.QRChannelEventPasskeyRequest, whatsmeow.QRChannelEventPasskeyResponse:
	default:
		s.log.Warn("pairing ended without success", "event", item.Event)
		s.setStatus(StatusDisconnected)
		s.pushStatus(frappeDisconnected)
	}
}

func (s *Session) pushQR(code string) {
	png, err := qrcode.Encode(code, qrcode.Medium, 256)
	if err != nil {
		s.log.Error("render qr code", "error", err)
		return
	}
	s.callback(outbox.MethodQR, qrPayload{SessionID: s.id, QRBase64: base64.StdEncoding.EncodeToString(png)})
}

func (s *Session) pushStatus(status string) {
	s.callback(outbox.MethodQR, qrPayload{SessionID: s.id, Status: status})
}

// callback hands a payload to the outbox. Payloads are never logged.
func (s *Session) callback(method string, payload any) {
	ctx, cancel := context.WithTimeout(context.Background(), callbackTimeout)
	defer cancel()
	if err := s.m.enqueue(ctx, method, payload); err != nil {
		s.log.Error("enqueue callback", "method", method, "error", err)
	}
}

func (s *Session) handleEvent(evt any) {
	if s.retired() {
		return
	}
	switch e := evt.(type) {
	case *events.Connected:
		s.onConnected()
	case *events.PairSuccess:
		s.onPairSuccess(e)
	case *events.LoggedOut:
		s.onLoggedOut(e)
	case *events.StreamReplaced:
		s.onStreamReplaced()
	case *events.TemporaryBan:
		s.onTemporaryBan(e)
	case *events.ConnectFailure:
		s.onConnectionLost("connect failure", "reason", int(e.Reason))
	case *events.ClientOutdated:
		s.onConnectionLost("client outdated: whatsmeow needs upgrading")
	case *events.Disconnected:
		s.log.Info("socket disconnected; auto-reconnect will retry")
	case *events.Message:
		s.onMessage(e)
	case *events.Receipt:
		s.onReceipt(e)
	}
}

// becomeConnected pushes Active only on a transition, so reconnects after a
// socket drop (status unchanged) don't flap Frappe.
func (s *Session) becomeConnected() {
	s.mu.Lock()
	prev := s.status
	s.status = StatusConnected
	s.banUntil = time.Time{}
	s.mu.Unlock()
	if prev != StatusConnected {
		s.log.Info("connected")
		s.pushStatus(frappeActive)
	}
}

func (s *Session) onConnected() { s.becomeConnected() }

func (s *Session) onPairSuccess(e *events.PairSuccess) {
	ctx, cancel := context.WithTimeout(context.Background(), callbackTimeout)
	defer cancel()
	if err := s.m.st.SetAccountJID(ctx, s.id, e.ID.String()); err != nil {
		s.log.Error("store paired jid", "error", err)
	}
	s.mu.Lock()
	s.jid = e.ID
	s.mu.Unlock()
	s.log.Info("paired", "platform", e.Platform)
	s.becomeConnected()
}

// onLoggedOut: whatsmeow already deletes the device store before emitting it.
func (s *Session) onLoggedOut(e *events.LoggedOut) {
	ctx, cancel := context.WithTimeout(context.Background(), callbackTimeout)
	defer cancel()
	if err := s.m.st.SetAccountJID(ctx, s.id, ""); err != nil {
		s.log.Error("clear jid after logout", "error", err)
	}
	s.stopBanTimer()
	s.mu.Lock()
	s.status = StatusLoggedOut
	s.jid = types.EmptyJID
	s.mu.Unlock()
	s.log.Warn("logged out by WhatsApp", "reason", e.Reason.String(), "on_connect", e.OnConnect)
	s.pushStatus(frappeDisconnected)
}

func (s *Session) onStreamReplaced() {
	s.log.Error("stream replaced: another client connected with this account's keys; not reconnecting")
	s.client.Disconnect()
	s.setStatus(StatusDisconnected)
	s.pushStatus(frappeDisconnected)
}

// onConnectionLost handles failures after which whatsmeow does not reconnect
// on its own (LoggedOut and TemporaryBan have their own events). The session
// becomes disconnected, so the next Start rebuilds it.
func (s *Session) onConnectionLost(msg string, attrs ...any) {
	s.log.Error(msg+"; not reconnecting", attrs...)
	if prev := s.setStatus(StatusDisconnected); prev != StatusDisconnected {
		s.pushStatus(frappeDisconnected)
	}
}

func (s *Session) onTemporaryBan(e *events.TemporaryBan) {
	wait := e.Expire
	if wait <= 0 {
		wait = unknownBanExpiry
	}
	until := s.m.now().Add(wait).UTC()
	s.stopBanTimer()
	stop := s.m.afterFunc(wait, s.reconnectAfterBan)
	s.mu.Lock()
	s.status = StatusTempBanned
	s.banUntil = until
	s.stopBan = stop
	s.mu.Unlock()
	s.log.Warn("temporarily banned by WhatsApp", "code", int(e.Code), "until", until.Format(time.RFC3339))
	s.callback(outbox.MethodAccountEvent, accountEventPayload{
		SessionID: s.id, Event: StatusTempBanned, Until: until.Format(time.RFC3339),
	})
}

// reconnectAfterBan runs when the ban expires; Connected then pushes Active.
func (s *Session) reconnectAfterBan() {
	unlock := s.m.lockID(s.id)
	defer unlock()
	if s.retired() || s.Status() != StatusTempBanned || s.client.IsConnected() {
		return
	}
	s.log.Info("ban expired; reconnecting")
	if err := s.client.Connect(); err != nil {
		s.log.Error("reconnect after ban", "error", err)
		s.setStatus(StatusDisconnected)
		s.pushStatus(frappeDisconnected)
	}
}

func (s *Session) onMessage(e *events.Message) {
	p, media, ok := convert.Inbound(s.id, e, s.resolvePN)
	if !ok {
		return
	}
	if media != nil {
		ctx, cancel := context.WithTimeout(s.ctx, downloadTimeout)
		data, err := s.client.Download(ctx, media)
		cancel()
		if err != nil {
			s.log.Warn("inbound media download failed; delivering without media", "message_id", p.MessageID, "error", err)
			p.MediaMimetype, p.MediaFilename = "", ""
		} else {
			p.MediaBase64 = base64.StdEncoding.EncodeToString(data)
		}
	}
	s.callback(outbox.MethodInbound, p)
}

func (s *Session) resolvePN(lid types.JID) (types.JID, bool) {
	if s.dev == nil || s.dev.LIDs == nil {
		return types.EmptyJID, false
	}
	pn, err := s.dev.LIDs.GetPNForLID(s.ctx, lid)
	if err != nil || pn.IsEmpty() {
		return types.EmptyJID, false
	}
	return pn, true
}

func (s *Session) onReceipt(e *events.Receipt) {
	if !convert.IsDirectChat(e.Chat) {
		return
	}
	status, ok := convert.ReceiptStatus(e.Type)
	if !ok {
		return
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	for _, id := range e.MessageIDs {
		s.callback(outbox.MethodStatus, statusPayload{MessageID: id, Status: status})
	}
}

// sendBlocker returns the error code that stops a send right now, if any.
func (s *Session) sendBlocker() (code, detail string) {
	s.mu.Lock()
	status, until := s.status, s.banUntil
	s.mu.Unlock()
	switch {
	case status == StatusTempBanned:
		return CodeTempBanned, "temporarily banned until " + until.Format(time.RFC3339)
	case status != StatusConnected || s.client == nil || !s.client.IsConnected():
		return CodeSessionUnavailable, "session is " + status
	}
	return "", ""
}

// checkNumber resolves digits to a registered WhatsApp JID, caching hits.
func (s *Session) checkNumber(ctx context.Context, digits string) (types.JID, *SendError) {
	now := s.m.now()
	s.mu.Lock()
	entry, cached := s.numbers[digits]
	s.mu.Unlock()
	if cached && now.Before(entry.expires) {
		return entry.jid, nil
	}
	resp, err := s.client.IsOnWhatsApp(ctx, []string{"+" + digits})
	if err != nil {
		return types.EmptyJID, &SendError{Code: CodeSessionUnavailable, HTTP: 409, Detail: "number check failed: " + err.Error()}
	}
	for _, r := range resp {
		if r.IsIn && !r.JID.IsEmpty() {
			s.mu.Lock()
			s.numbers[digits] = numberEntry{jid: r.JID, expires: now.Add(numberCacheTTL)}
			s.mu.Unlock()
			return r.JID, nil
		}
	}
	return types.EmptyJID, &SendError{Code: CodeInvalidNumber, HTTP: 422, Detail: "number is not on WhatsApp"}
}

// deliver is the queue worker's send (spec §8.3 asynchronous steps 4-7).
func (s *Session) deliver(job *sendJob) {
	if code, detail := s.sendBlocker(); code != "" {
		s.finishFailed(job, code, detail)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()

	s.presence(ctx, job.to, types.ChatPresenceComposing)
	s.queue.sleep(typingDelay(job.text))
	if s.queue.isStopped() { // paused, logged out or shutting down mid-typing
		s.finishFailed(job, CodeSessionUnavailable, "session stopped")
		return
	}
	s.presence(ctx, job.to, types.ChatPresencePaused)

	msg, err := s.buildMessage(ctx, job)
	if err != nil {
		s.finishFailed(job, store.ErrCodeSendFailed, err.Error())
		return
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	// The id is fixed up front so an unconfirmed send can still be matched
	// to its receipts.
	id := s.client.GenerateMessageID()
	resp, err := s.client.SendMessage(ctx, job.to, msg, whatsmeow.SendRequestExtra{ID: id})
	switch {
	case err == nil:
		s.finishSent(job, resp.ID)
	case sendUnconfirmed(err):
		s.finishUnconfirmed(job, id, err)
	default:
		s.finishFailed(job, store.ErrCodeSendFailed, err.Error())
	}
}

// sendUnconfirmed reports whether a SendMessage error can come after the
// message node was written (whatsmeow send.go at the pinned version: waiting
// for the server ack timed out or was cancelled, or the socket dropped and
// the retried frame got no answer). Failing over would risk a duplicate.
func sendUnconfirmed(err error) bool {
	var disconnected *whatsmeow.DisconnectedError
	return errors.Is(err, whatsmeow.ErrMessageTimedOut) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) ||
		errors.As(err, &disconnected)
}

func typingDelay(text string) time.Duration {
	return time.Second + time.Duration(min(utf8.RuneCountInString(text)/50, 2))*time.Second
}

func (s *Session) presence(ctx context.Context, to types.JID, state types.ChatPresence) {
	if err := s.client.SendChatPresence(ctx, to, state, types.ChatPresenceMediaText); err != nil {
		s.log.Debug("send chat presence", "state", string(state), "error", err)
	}
}

func (s *Session) buildMessage(ctx context.Context, job *sendJob) (*waE2E.Message, error) {
	if job.media == nil {
		return convert.TextMessage(job.text), nil
	}
	up, err := s.client.Upload(ctx, job.media, convert.ContentToMediaType(job.contentType))
	if err != nil {
		return nil, err
	}
	return convert.BuildMediaMessage(job.contentType, job.text, job.mimetype, job.filename, up), nil
}

func (s *Session) finishSent(job *sendJob, messageID string) {
	ctx, cancel := context.WithTimeout(context.Background(), callbackTimeout)
	defer cancel()
	ok, err := s.m.st.MarkSent(ctx, job.localID, s.id, messageID)
	if err != nil {
		s.log.Error("mark send sent", "local_message_id", job.localID, "error", err)
	} else if !ok {
		s.log.Warn("send result ignored: reservation no longer queued on this session", "local_message_id", job.localID)
		return
	}
	s.callback(outbox.MethodStatus, statusPayload{LocalMessageID: job.localID, MessageID: messageID, Status: "Sent"})
}

// finishUnconfirmed reports a send of unknown outcome as Failed with
// send_unconfirmed and its message id; Frappe does not fail it over, and a
// later receipt for the id lifts the status.
func (s *Session) finishUnconfirmed(job *sendJob, messageID string, sendErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), callbackTimeout)
	defer cancel()
	ok, err := s.m.st.MarkUnconfirmed(ctx, job.localID, s.id, messageID)
	if err != nil {
		s.log.Error("mark send unconfirmed", "local_message_id", job.localID, "error", err)
	} else if !ok {
		s.log.Warn("send result ignored: reservation no longer queued on this session", "local_message_id", job.localID)
		return
	}
	s.log.Warn("send unconfirmed", "local_message_id", job.localID, "message_id", messageID, "error", sendErr)
	s.callback(outbox.MethodStatus, statusPayload{
		LocalMessageID: job.localID, MessageID: messageID, Status: "Failed",
		ErrorCode: store.ErrCodeSendUnconfirmed, ErrorReason: unconfirmedReason,
	})
}

func (s *Session) finishFailed(job *sendJob, code, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), callbackTimeout)
	defer cancel()
	ok, err := s.m.st.MarkFailed(ctx, job.localID, s.id, code)
	if err != nil {
		s.log.Error("mark send failed", "local_message_id", job.localID, "error", err)
	} else if !ok {
		s.log.Warn("send result ignored: reservation no longer queued on this session", "local_message_id", job.localID)
		return
	}
	s.log.Warn("send failed", "local_message_id", job.localID, "error_code", code)
	s.callback(outbox.MethodStatus, statusPayload{LocalMessageID: job.localID, Status: "Failed", ErrorCode: code, ErrorReason: reason})
}
