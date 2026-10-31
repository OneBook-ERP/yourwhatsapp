package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yourwhatsapp/relay/internal/phone"
	"yourwhatsapp/relay/internal/session"
	"yourwhatsapp/relay/internal/store"
)

const testKey = "secret"

type fakeManager struct {
	startID    string
	startProxy *string
	pairProxy  *string
	pairErr    error
	sendErr    *session.SendError
	sendReq    session.SendRequest
}

func (f *fakeManager) Start(_ context.Context, id string, proxy *string) (string, error) {
	f.startID, f.startProxy = id, proxy
	return "qr_pending", nil
}
func (f *fakeManager) PairCode(_ context.Context, _, _ string, proxy *string) (string, error) {
	f.pairProxy = proxy
	return "ABCD-1234", f.pairErr
}
func (f *fakeManager) Status(string) (string, string, string) { return "connected", "1@s", "1" }
func (f *fakeManager) Pause(context.Context, string) error    { return nil }
func (f *fakeManager) Resume(context.Context, string, *string) error {
	return nil
}
func (f *fakeManager) Logout(context.Context, string) error { return session.ErrUnknownSession }
func (f *fakeManager) Send(_ context.Context, req session.SendRequest) *session.SendError {
	f.sendReq = req
	return f.sendErr
}

func newServer(t *testing.T, fm *fakeManager) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return NewHandler(fm, st, testKey, slog.New(slog.NewJSONHandler(io.Discard, nil))), st
}

func do(h http.Handler, method, path, body string, auth bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth {
		req.Header.Set("x-api-key", testKey)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("bad json %q: %v", rec.Body.String(), err)
	}
	return m
}

func TestHealthOpen(t *testing.T) {
	h, _ := newServer(t, &fakeManager{})
	rec := do(h, "GET", "/health", "", false)
	if rec.Code != 200 || decode(t, rec)["ok"] != true {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestAuthRequired(t *testing.T) {
	h, _ := newServer(t, &fakeManager{})
	if rec := do(h, "GET", "/session/a/status", "", false); rec.Code != 401 {
		t.Fatalf("missing key: %d", rec.Code)
	}
	req := httptest.NewRequest("GET", "/session/a/status", nil)
	req.Header.Set("x-api-key", "wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 || decode(t, rec)["error"] != "invalid api key" {
		t.Fatalf("wrong key: %d %s", rec.Code, rec.Body)
	}
}

func TestStartPassesProxy(t *testing.T) {
	fm := &fakeManager{}
	h, _ := newServer(t, fm)
	rec := do(h, "POST", "/session/t1/start", `{"proxy":"socks5://p:1"}`, true)
	if rec.Code != 200 || decode(t, rec)["status"] != "qr_pending" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if fm.startID != "t1" || fm.startProxy == nil || *fm.startProxy != "socks5://p:1" {
		t.Fatalf("start args: %q %v", fm.startID, fm.startProxy)
	}
	do(h, "POST", "/session/t2/start", "", true)
	if fm.startProxy != nil {
		t.Fatalf("empty body should give nil proxy")
	}
}

func TestPairCode(t *testing.T) {
	fm := &fakeManager{}
	h, _ := newServer(t, fm)
	rec := do(h, "POST", "/session/a/pair-code", `{"phone":"+91 98765 43210"}`, true)
	if rec.Code != 200 || decode(t, rec)["code"] != "ABCD-1234" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if fm.pairProxy != nil {
		t.Fatalf("absent proxy should give nil, got %q", *fm.pairProxy)
	}
	do(h, "POST", "/session/a/pair-code", `{"phone":"1","proxy":"socks5://p:1"}`, true)
	if fm.pairProxy == nil || *fm.pairProxy != "socks5://p:1" {
		t.Fatalf("pair proxy = %v", fm.pairProxy)
	}
	fm.pairErr = phone.ErrInvalid
	if rec := do(h, "POST", "/session/a/pair-code", `{"phone":"x"}`, true); rec.Code != 422 || decode(t, rec)["error"] != "invalid_number" {
		t.Fatalf("invalid phone: %d %s", rec.Code, rec.Body)
	}
	fm.pairErr = session.ErrAlreadyPaired
	if rec := do(h, "POST", "/session/a/pair-code", `{"phone":"1"}`, true); rec.Code != 409 {
		t.Fatalf("already paired: %d", rec.Code)
	}
}

func TestUnknownSessionIs404(t *testing.T) {
	h, _ := newServer(t, &fakeManager{})
	if rec := do(h, "POST", "/session/a/logout", "", true); rec.Code != 404 {
		t.Fatalf("got %d", rec.Code)
	}
}

const validSend = `{"session_id":"a","to":"919876543210","content_type":"text","message":"hi","local_message_id":"L1","to_is_lid":true}`

func TestSendAccepted202(t *testing.T) {
	fm := &fakeManager{}
	h, _ := newServer(t, fm)
	rec := do(h, "POST", "/message/send", validSend, true)
	if rec.Code != 202 || decode(t, rec)["state"] != "queued" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if fm.sendReq.LocalID != "L1" || fm.sendReq.SessionID != "a" || !fm.sendReq.ToIsLID {
		t.Fatalf("request not passed through: %+v", fm.sendReq)
	}
}

func TestSendDuplicateReturns200(t *testing.T) {
	fm := &fakeManager{sendErr: &session.SendError{
		Code: session.CodeDuplicate, HTTP: 200,
		Existing: &store.SendRecord{State: "sent", MessageID: "WAID"},
	}}
	h, _ := newServer(t, fm)
	rec := do(h, "POST", "/message/send", validSend, true)
	m := decode(t, rec)
	if rec.Code != 200 || m["error"] != "duplicate" || m["state"] != "sent" || m["message_id"] != "WAID" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestSendErrorMapping(t *testing.T) {
	cases := []struct {
		code string
		http int
	}{
		{session.CodeBadRequest, 400}, {session.CodeInvalidNumber, 422},
		{session.CodeSessionUnavailable, 409}, {session.CodeTempBanned, 429},
	}
	for _, c := range cases {
		fm := &fakeManager{sendErr: &session.SendError{Code: c.code, HTTP: c.http, Detail: "why"}}
		h, _ := newServer(t, fm)
		rec := do(h, "POST", "/message/send", validSend, true)
		m := decode(t, rec)
		if rec.Code != c.http || m["error"] != c.code || m["detail"] != "why" {
			t.Errorf("%s: got %d %s", c.code, rec.Code, rec.Body)
		}
	}
}

func TestSendBadJSON400(t *testing.T) {
	h, _ := newServer(t, &fakeManager{})
	rec := do(h, "POST", "/message/send", `{not json`, true)
	if rec.Code != 400 || decode(t, rec)["error"] != "bad_request" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestSendMissingLocalID400(t *testing.T) {
	h, _ := newServer(t, &fakeManager{})
	rec := do(h, "POST", "/message/send", `{"session_id":"a","to":"1","content_type":"text","message":"x"}`, true)
	if rec.Code != 400 || decode(t, rec)["error"] != "bad_request" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestBodyTooLarge(t *testing.T) {
	h, _ := newServer(t, &fakeManager{})
	big := `{"local_message_id":"L","message":"` + strings.Repeat("a", maxBodyBytes+1) + `"}`
	if rec := do(h, "POST", "/message/send", big, true); rec.Code != 413 {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestDeadList(t *testing.T) {
	h, st := newServer(t, &fakeManager{})
	ctx := context.Background()
	if err := st.EnqueueCallback(ctx, "m", map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	row, err := st.HeadCallback(ctx)
	if err != nil || row == nil {
		t.Fatal(err)
	}
	if err := st.KillCallback(ctx, row.ID, "boom"); err != nil {
		t.Fatal(err)
	}
	rec := do(h, "GET", "/outbox/dead", "", true)
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != 200 {
		t.Fatalf("got %d %s (%v)", rec.Code, rec.Body, err)
	}
	if len(list) != 1 || list[0]["method"] != "m" || list[0]["last_error"] != "boom" {
		t.Fatalf("list: %v", list)
	}
}
