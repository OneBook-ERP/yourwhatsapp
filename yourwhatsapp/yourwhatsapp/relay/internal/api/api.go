// Package api is the relay's HTTP surface (spec §7).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"yourwhatsapp/relay/internal/claim"
	"yourwhatsapp/relay/internal/phone"
	"yourwhatsapp/relay/internal/session"
	"yourwhatsapp/relay/internal/store"
)

const (
	maxBodyBytes = 25 << 20
	deadLimit    = 200
)

// Manager is the subset of *session.Manager the API drives.
type Manager interface {
	Start(ctx context.Context, id string, proxy *string) (string, error)
	PairCode(ctx context.Context, id, rawPhone string, proxy *string) (string, error)
	Status(id string) (status, jid, phoneNumber string)
	Pause(ctx context.Context, id string) error
	Resume(ctx context.Context, id string, proxy *string) (string, error)
	Logout(ctx context.Context, id string) error
	Send(ctx context.Context, req session.SendRequest) *session.SendError
	Connected() int
}

type handler struct {
	m       Manager
	st      *store.Store
	cs      *claim.State
	log     *slog.Logger
	version string
}

// NewHandler returns the relay's HTTP handler. Every route but /health,
// /claim and /info needs x-api-key, and answers 503 unclaimed until the relay is claimed.
func NewHandler(m Manager, st *store.Store, cs *claim.State, log *slog.Logger, version string) http.Handler {
	h := &handler{m: m, st: st, cs: cs, log: log, version: version}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /claim", h.claim)
	mux.HandleFunc("GET /info", h.info)
	mux.Handle("POST /claim/rotate", h.auth(h.rotate))
	mux.Handle("PUT /claim/callback", h.authCurrent(h.setCallback))
	mux.Handle("POST /session/{id}/start", h.auth(h.start))
	mux.Handle("POST /session/{id}/pair-code", h.auth(h.pairCode))
	mux.Handle("GET /session/{id}/status", h.auth(h.status))
	mux.Handle("POST /session/{id}/pause", h.auth(h.pause))
	mux.Handle("POST /session/{id}/resume", h.auth(h.resume))
	mux.Handle("POST /session/{id}/logout", h.auth(h.logout))
	mux.Handle("POST /message/send", h.auth(h.send))
	mux.Handle("GET /outbox/dead", h.auth(h.deadList))
	return mux
}

func (h *handler) auth(next http.HandlerFunc) http.Handler { return h.gate(next, h.cs.Authorized) }

// authCurrent refuses the rotation grace key: for routes that change the relay's identity.
func (h *handler) authCurrent(next http.HandlerFunc) http.Handler {
	return h.gate(next, h.cs.AuthorizedCurrent)
}

func (h *handler) gate(next http.HandlerFunc, allowed func(key string) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.cs.Mode() == claim.ModeUnclaimed {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "unclaimed"})
			return
		}
		if !allowed(r.Header.Get("x-api-key")) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid api key"})
			return
		}
		next(w, r)
	})
}

type claimBody struct {
	APIKey         string `json:"api_key"`
	CallbackURL    string `json:"callback_url"`
	SiteName       string `json:"site_name"`
	BootstrapToken string `json:"bootstrap_token"`
}

func (h *handler) claim(w http.ResponseWriter, r *http.Request) {
	var b claimBody
	if !decodeBody(w, r, &b, false) {
		return
	}
	err := h.cs.Claim(r.Context(), b.APIKey, b.CallbackURL, b.SiteName, b.BootstrapToken)
	switch {
	case err == nil:
		h.log.Info("relay claimed", "callback_url", redactedURL(b.CallbackURL), "site_name", b.SiteName)
		writeJSON(w, http.StatusOK, map[string]any{"claimed": true})
	case errors.Is(err, claim.ErrAlreadyClaimed):
		writeError(w, http.StatusConflict, "already_claimed", err.Error())
	case errors.Is(err, claim.ErrBadBootstrapToken):
		h.log.Warn("claim rejected: bad bootstrap token")
		writeError(w, http.StatusUnauthorized, "bad_bootstrap_token", err.Error())
	case errors.Is(err, claim.ErrBadRequest):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		h.fail(w, r, err)
	}
}

func (h *handler) rotate(w http.ResponseWriter, r *http.Request) {
	var b struct {
		APIKey string `json:"api_key"`
	}
	if !decodeBody(w, r, &b, false) {
		return
	}
	// Only the current key may rotate. The grace key may repeat a rotation
	// that already took effect (Frappe lost the 200): same key, nothing to do.
	if !h.cs.AuthorizedCurrent(r.Header.Get("x-api-key")) {
		if h.cs.AuthorizedCurrent(b.APIKey) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid api key"})
		return
	}
	if !h.claimChange(w, r, h.cs.Rotate(r.Context(), b.APIKey)) {
		return
	}
	h.log.Info("api key rotated", "grace", claim.RotationGrace.String())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *handler) setCallback(w http.ResponseWriter, r *http.Request) {
	var b struct {
		CallbackURL string `json:"callback_url"`
		SiteName    string `json:"site_name"`
	}
	if !decodeBody(w, r, &b, false) {
		return
	}
	if !h.claimChange(w, r, h.cs.SetCallback(r.Context(), b.CallbackURL, b.SiteName)) {
		return
	}
	h.log.Info("callback updated", "callback_url", redactedURL(b.CallbackURL), "site_name", b.SiteName)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// claimChange answers for a failed rotate/callback change and reports whether it succeeded.
func (h *handler) claimChange(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, claim.ErrEnvMode):
		writeError(w, http.StatusConflict, "env_mode", err.Error())
	case errors.Is(err, claim.ErrUnclaimed):
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "unclaimed"})
	case errors.Is(err, claim.ErrBadRequest):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		h.fail(w, r, err)
	}
	return false
}

// info is open to everyone with the minimal body; a valid key adds the
// operational detail. It never returns a key.
func (h *handler) info(w http.ResponseWriter, r *http.Request) {
	mode := h.cs.Mode()
	out := map[string]any{"version": h.version, "claimed": mode != claim.ModeUnclaimed, "mode": mode}
	if h.cs.Authorized(r.Header.Get("x-api-key")) {
		pending, dead, err := h.st.OutboxCounts(r.Context())
		if err != nil {
			h.fail(w, r, err)
			return
		}
		total, err := h.st.CountLinkedAccounts(r.Context())
		if err != nil {
			h.fail(w, r, err)
			return
		}
		connected := h.m.Connected()
		callbackURL, siteName := h.cs.Info()
		out["sessions"] = map[string]int{"connected": connected, "total": total}
		out["outbox"] = map[string]int{"pending": pending, "dead": dead}
		out["callback_url"] = redactedURL(callbackURL)
		out["site_name"] = siteName
	}
	writeJSON(w, http.StatusOK, out)
}

// redactedURL hides any password before a URL reaches the log.
func redactedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Redacted()
}

type proxyBody struct {
	Proxy *string `json:"proxy"`
}

func (h *handler) start(w http.ResponseWriter, r *http.Request) {
	var body proxyBody
	if !decodeBody(w, r, &body, true) {
		return
	}
	status, err := h.m.Start(r.Context(), r.PathValue("id"), body.Proxy)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status})
}

func (h *handler) pairCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string  `json:"phone"`
		Proxy *string `json:"proxy"`
	}
	if !decodeBody(w, r, &body, false) {
		return
	}
	code, err := h.m.PairCode(r.Context(), r.PathValue("id"), body.Phone, body.Proxy)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": code})
}

func (h *handler) status(w http.ResponseWriter, r *http.Request) {
	status, jid, phoneNumber := h.m.Status(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "jid": jid, "phone": phoneNumber})
}

func (h *handler) pause(w http.ResponseWriter, r *http.Request) {
	if err := h.m.Pause(r.Context(), r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *handler) resume(w http.ResponseWriter, r *http.Request) {
	var body proxyBody
	if !decodeBody(w, r, &body, true) {
		return
	}
	status, err := h.m.Resume(r.Context(), r.PathValue("id"), body.Proxy)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status})
}

func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	if err := h.m.Logout(r.Context(), r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type sendBody struct {
	SessionID      string `json:"session_id"`
	To             string `json:"to"`
	ContentType    string `json:"content_type"`
	Message        string `json:"message"`
	LocalID        string `json:"local_message_id"`
	AttachBase64   string `json:"attach_base64"`
	AttachMimetype string `json:"attach_mimetype"`
	AttachFilename string `json:"attach_filename"`
	ToIsLID        bool   `json:"to_is_lid"`
}

func (h *handler) send(w http.ResponseWriter, r *http.Request) {
	var b sendBody
	if !decodeBody(w, r, &b, false) {
		return
	}
	if b.LocalID == "" {
		writeError(w, http.StatusBadRequest, session.CodeBadRequest, "local_message_id is required")
		return
	}
	serr := h.m.Send(r.Context(), session.SendRequest{
		SessionID: b.SessionID, To: b.To, ContentType: b.ContentType, Message: b.Message,
		LocalID: b.LocalID, AttachBase64: b.AttachBase64, AttachMimetype: b.AttachMimetype,
		AttachFilename: b.AttachFilename, ToIsLID: b.ToIsLID,
	})
	switch {
	case serr == nil:
		writeJSON(w, http.StatusAccepted, map[string]any{"state": store.SendQueued})
	case serr.Code == session.CodeDuplicate && serr.Existing != nil:
		writeJSON(w, http.StatusOK, map[string]any{
			"error": session.CodeDuplicate, "state": serr.Existing.State, "message_id": serr.Existing.MessageID,
		})
	default:
		writeError(w, serr.HTTP, serr.Code, serr.Detail)
	}
}

func (h *handler) deadList(w http.ResponseWriter, r *http.Request) {
	rows, err := h.st.ListDead(r.Context(), deadLimit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"id": row.ID, "method": row.Method, "payload": json.RawMessage(row.Payload),
			"attempts": row.Attempts, "last_error": row.LastError,
			"created_at": row.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// fail maps a manager error onto the response; unexpected errors are logged, not echoed.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, session.ErrUnknownSession):
		writeError(w, http.StatusNotFound, "unknown_session", err.Error())
	case errors.Is(err, session.ErrAlreadyPaired), errors.Is(err, session.ErrPaused):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, phone.ErrInvalid):
		writeError(w, http.StatusUnprocessableEntity, session.CodeInvalidNumber, err.Error())
	case errors.Is(err, session.ErrShuttingDown):
		writeError(w, http.StatusServiceUnavailable, "shutting_down", err.Error())
	default:
		h.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		writeError(w, http.StatusInternalServerError, session.CodeInternal, "internal error")
	}
}

// decodeBody reads a size-limited JSON body into dst, answering 413/400 itself on failure.
// With optional, an empty body is accepted.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any, optional bool) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	err := json.NewDecoder(r.Body).Decode(dst)
	if err == nil || (optional && errors.Is(err, io.EOF)) {
		return true
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, session.CodeBadRequest, "request body too large")
		return false
	}
	writeError(w, http.StatusBadRequest, session.CodeBadRequest, "invalid JSON body")
	return false
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, map[string]any{"error": code, "detail": detail})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
