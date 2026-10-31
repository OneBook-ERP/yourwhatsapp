// Package api is the relay's HTTP surface (spec §7).
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

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
}

type handler struct {
	m      Manager
	st     *store.Store
	apiKey []byte
	log    *slog.Logger
}

// NewHandler returns the relay's HTTP handler. Every route but /health needs x-api-key.
func NewHandler(m Manager, st *store.Store, apiKey string, log *slog.Logger) http.Handler {
	h := &handler{m: m, st: st, apiKey: []byte(apiKey), log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
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

func (h *handler) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("x-api-key"))
		if subtle.ConstantTimeCompare(got, h.apiKey) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid api key"})
			return
		}
		next(w, r)
	})
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
