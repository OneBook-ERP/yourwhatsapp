package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yourwhatsapp/relay/internal/claim"
	"yourwhatsapp/relay/internal/config"
	"yourwhatsapp/relay/internal/outbox"
)

var claimKey = strings.Repeat("c", claim.MinKeyLen)

func claimJSON(key, url, token string) string {
	return `{"api_key":"` + key + `","callback_url":"` + url + `","site_name":"crm.example.com","bootstrap_token":"` + token + `"}`
}

func TestUnclaimedProtectedRoutes503(t *testing.T) {
	h, _, _ := newServerWith(t, &fakeManager{}, config.Config{})
	for _, r := range []struct{ method, path, body string }{
		{"GET", "/session/x/status", ""},
		{"POST", "/message/send", `{"local_message_id":"m1"}`},
		{"GET", "/outbox/dead", ""},
	} {
		rec := do(h, r.method, r.path, r.body, true)
		if rec.Code != 503 {
			t.Errorf("%s %s = %d, want 503", r.method, r.path, rec.Code)
			continue
		}
		if m := decode(t, rec); m["error"] != "unclaimed" {
			t.Errorf("%s %s body = %v", r.method, r.path, m)
		}
	}
	if rec := do(h, "GET", "/health", "", false); rec.Code != 200 {
		t.Errorf("/health = %d, want 200", rec.Code)
	}
}

func TestClaimRouteThenProtectedRouteWithKey(t *testing.T) {
	h, st, cs := newServerWith(t, &fakeManager{}, config.Config{})
	rec := do(h, "POST", "/claim", claimJSON(claimKey, "http://127.0.0.1:8001", ""), false)
	if rec.Code != 200 || decode(t, rec)["claimed"] != true {
		t.Fatalf("claim = %d %s", rec.Code, rec.Body)
	}
	if cs.Mode() != claim.ModeClaimed {
		t.Fatalf("mode = %q", cs.Mode())
	}
	if rc, err := st.GetRelayConfig(context.Background()); err != nil || rc == nil || rc.SiteName != "crm.example.com" {
		t.Fatalf("stored = %+v, %v", rc, err)
	}
	if rec := doWithKey(h, "GET", "/session/x/status", claimKey); rec.Code != 200 {
		t.Fatalf("status with claimed key = %d", rec.Code)
	}
	if rec := doWithKey(h, "GET", "/session/x/status", "wrong"); rec.Code != 401 {
		t.Fatalf("status with wrong key = %d", rec.Code)
	}
}

func TestClaimRouteErrors(t *testing.T) {
	cases := []struct {
		name   string
		cfg    config.Config
		body   string
		status int
		code   string
	}{
		{"env mode", config.Config{APIKey: testKey}, claimJSON(claimKey, "http://a", ""), 409, "already_claimed"},
		{"bootstrap missing", config.Config{BootstrapToken: "boot"}, claimJSON(claimKey, "http://a", ""), 401, "bad_bootstrap_token"},
		{"bootstrap wrong", config.Config{BootstrapToken: "boot"}, claimJSON(claimKey, "http://a", "nope"), 401, "bad_bootstrap_token"},
		{"short key", config.Config{}, claimJSON("short", "http://a", ""), 400, "bad_request"},
		{"bad url", config.Config{}, claimJSON(claimKey, "ftp://a", ""), 400, "bad_request"},
		{"userinfo url", config.Config{}, claimJSON(claimKey, "http://u:p@a", ""), 400, "bad_request"},
		{"bad json", config.Config{}, "{", 400, "bad_request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, _, _ := newServerWith(t, &fakeManager{}, c.cfg)
			rec := do(h, "POST", "/claim", c.body, false)
			if rec.Code != c.status || decode(t, rec)["error"] != c.code {
				t.Fatalf("got %d %s, want %d %s", rec.Code, rec.Body, c.status, c.code)
			}
			if strings.Contains(rec.Body.String(), claimKey) {
				t.Fatal("response leaks the key")
			}
		})
	}
}

func TestClaimRouteSecondClaim409(t *testing.T) {
	h, _, _ := newServerWith(t, &fakeManager{}, config.Config{})
	if rec := do(h, "POST", "/claim", claimJSON(claimKey, "http://a", ""), false); rec.Code != 200 {
		t.Fatalf("first claim = %d", rec.Code)
	}
	other := strings.Repeat("o", claim.MinKeyLen)
	rec := do(h, "POST", "/claim", claimJSON(other, "http://b", ""), false)
	if rec.Code != 409 || decode(t, rec)["error"] != "already_claimed" {
		t.Fatalf("second claim = %d %s", rec.Code, rec.Body)
	}
}

func infoBody(t *testing.T, h http.Handler, key string) (int, map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/info", nil)
	if key != "" {
		req.Header.Set("x-api-key", key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, decode(t, rec), rec.Body.String()
}

func TestInfoUnauthenticatedMinimal(t *testing.T) {
	h, _, _ := newServerWith(t, &fakeManager{}, config.Config{APIKey: testKey})
	for _, key := range []string{"", "wrong"} {
		code, m, _ := infoBody(t, h, key)
		if code != 200 || m["version"] != "test" || m["claimed"] != true || m["mode"] != "env" {
			t.Fatalf("key %q: %d %v", key, code, m)
		}
		for _, k := range []string{"sessions", "outbox", "callback_url", "site_name"} {
			if _, ok := m[k]; ok {
				t.Errorf("unauthenticated /info leaks %q", k)
			}
		}
	}
	h, _, _ = newServerWith(t, &fakeManager{}, config.Config{})
	if code, m, _ := infoBody(t, h, ""); code != 200 || m["claimed"] != false || m["mode"] != "unclaimed" {
		t.Fatalf("unclaimed: %d %v", code, m)
	}
}

func TestInfoAuthenticatedCounts(t *testing.T) {
	h, st, _ := newServerWith(t, &fakeManager{}, config.Config{APIKey: testKey, FrappeBaseURL: "http://127.0.0.1:8001", FrappeSiteName: "s.local"})
	if err := st.EnqueueCallback(context.Background(), "update_qr", map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "unlinked"} {
		if err := st.UpsertAccount(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"a", "b"} {
		if err := st.SetAccountJID(context.Background(), id, "1@s.whatsapp.net"); err != nil {
			t.Fatal(err)
		}
	}
	code, m, _ := infoBody(t, h, testKey)
	if code != 200 {
		t.Fatal(code)
	}
	if s := m["sessions"].(map[string]any); s["connected"] != float64(1) || s["total"] != float64(2) {
		t.Errorf("sessions = %v", s)
	}
	if o := m["outbox"].(map[string]any); o["pending"] != float64(1) || o["dead"] != float64(0) {
		t.Errorf("outbox = %v", o)
	}
	if m["callback_url"] != "http://127.0.0.1:8001" || m["site_name"] != "s.local" {
		t.Errorf("target = %v %v", m["callback_url"], m["site_name"])
	}
}

func TestInfoNeverContainsKey(t *testing.T) {
	cases := map[string]config.Config{
		"env":       {APIKey: testKey},
		"unclaimed": {},
	}
	for name, cfg := range cases {
		h, _, _ := newServerWith(t, &fakeManager{}, cfg)
		for _, key := range []string{"", testKey} {
			if _, _, body := infoBody(t, h, key); strings.Contains(body, testKey) {
				t.Errorf("%s: /info body contains the key: %s", name, body)
			}
		}
	}
	h, _, _ := newServerWith(t, &fakeManager{}, config.Config{})
	do(h, "POST", "/claim", claimJSON(claimKey, "http://127.0.0.1:8001", ""), false)
	for _, key := range []string{"", claimKey} {
		if _, _, body := infoBody(t, h, key); strings.Contains(body, claimKey) {
			t.Errorf("claimed: /info body contains the key: %s", body)
		}
	}
}

func TestRotateAndCallbackRoutes(t *testing.T) {
	h, _, _ := newServerWith(t, &fakeManager{}, config.Config{})
	do(h, "POST", "/claim", claimJSON(claimKey, "http://127.0.0.1:8001", ""), false)
	newK := strings.Repeat("r", claim.MinKeyLen)
	send := func(method, path, body, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("x-api-key", key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := send("POST", "/claim/rotate", `{"api_key":"x"}`, "wrong"); rec.Code != 401 {
		t.Fatalf("wrong key = %d", rec.Code)
	}
	if rec := send("POST", "/claim/rotate", `{"api_key":"short"}`, claimKey); rec.Code != 400 {
		t.Fatalf("short = %d", rec.Code)
	}
	if rec := send("POST", "/claim/rotate", `{"api_key":"`+newK+`"}`, claimKey); rec.Code != 200 {
		t.Fatalf("rotate = %d %s", rec.Code, rec.Body)
	}
	if rec := send("PUT", "/claim/callback", `{"callback_url":"http://u:p@h","site_name":"s"}`, newK); rec.Code != 400 {
		t.Fatalf("bad callback = %d", rec.Code)
	}
	if rec := send("PUT", "/claim/callback", `{"callback_url":"http://frontend:8080","site_name":"s"}`, newK); rec.Code != 200 {
		t.Fatalf("callback = %d %s", rec.Code, rec.Body)
	}
}

func TestRotateCallbackEnvMode409(t *testing.T) {
	h, _ := newServer(t, &fakeManager{})
	for _, r := range []struct{ method, path, body string }{
		{"POST", "/claim/rotate", `{"api_key":"` + strings.Repeat("r", claim.MinKeyLen) + `"}`},
		{"PUT", "/claim/callback", `{"callback_url":"http://a","site_name":"s"}`},
	} {
		rec := do(h, r.method, r.path, r.body, true)
		if rec.Code != 409 || decode(t, rec)["error"] != "env_mode" {
			t.Errorf("%s = %d %s", r.path, rec.Code, rec.Body)
		}
	}
}

// rotatedRelay is a claimed relay that has just rotated claimKey -> rotatedKey,
// so claimKey is the grace key.
func rotatedRelay(t *testing.T) (send func(method, path, body, key string) *httptest.ResponseRecorder, rotatedKey string, cs *claim.State) {
	t.Helper()
	h, _, cs := newServerWith(t, &fakeManager{}, config.Config{})
	do(h, "POST", "/claim", claimJSON(claimKey, "http://127.0.0.1:8001", ""), false)
	rotatedKey = strings.Repeat("r", claim.MinKeyLen)
	send = func(method, path, body, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("x-api-key", key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := send("POST", "/claim/rotate", `{"api_key":"`+rotatedKey+`"}`, claimKey); rec.Code != 200 {
		t.Fatalf("rotate = %d", rec.Code)
	}
	return send, rotatedKey, cs
}

func TestGraceKeyCannotChangeCallback(t *testing.T) {
	send, rotatedKey, cs := rotatedRelay(t)
	body := `{"callback_url":"http://attacker.example","site_name":"s"}`
	if rec := send("PUT", "/claim/callback", body, claimKey); rec.Code != 401 {
		t.Fatalf("grace key callback = %d, want 401", rec.Code)
	}
	if u, _, _ := cs.Target(); u != "http://127.0.0.1:8001" {
		t.Fatalf("callback changed to %q", u)
	}
	if rec := send("PUT", "/claim/callback", body, rotatedKey); rec.Code != 200 {
		t.Fatalf("current key callback = %d", rec.Code)
	}
}

func TestGraceKeyCannotRotateToDifferentKey(t *testing.T) {
	send, rotatedKey, cs := rotatedRelay(t)
	other := strings.Repeat("o", claim.MinKeyLen)
	if rec := send("POST", "/claim/rotate", `{"api_key":"`+other+`"}`, claimKey); rec.Code != 401 {
		t.Fatalf("grace key rotate = %d, want 401", rec.Code)
	}
	if _, _, k := cs.Target(); k != rotatedKey {
		t.Fatal("key changed")
	}
	if rec := send("POST", "/claim/rotate", `{"api_key":"`+other+`"}`, rotatedKey); rec.Code != 200 {
		t.Fatalf("current key rotate = %d", rec.Code)
	}
}

// Frappe lost the 200 and retries the same rotation with the old key: a no-op 200.
func TestGraceKeyRotateToCurrentKeyIsIdempotent(t *testing.T) {
	send, rotatedKey, cs := rotatedRelay(t)
	rec := send("POST", "/claim/rotate", `{"api_key":"`+rotatedKey+`"}`, claimKey)
	if rec.Code != 200 {
		t.Fatalf("idempotent retry = %d %s", rec.Code, rec.Body)
	}
	if _, _, k := cs.Target(); k != rotatedKey {
		t.Fatal("key changed")
	}
	if !cs.Authorized(claimKey) || cs.AuthorizedCurrent(claimKey) {
		t.Fatal("retry must leave the old key as grace only, window unchanged")
	}
}

// checkRecorder records callback checks and fails them with err.
type checkRecorder struct {
	calls          int
	url, site, key string
	err            error
}

func (c *checkRecorder) check(_ context.Context, url, site, key string) error {
	c.calls++
	c.url, c.site, c.key = url, site, key
	return c.err
}

func TestClaimProbesCallbackWithCandidateKey(t *testing.T) {
	rec := &checkRecorder{}
	h, _, cs := newServerFull(t, &fakeManager{}, config.Config{}, rec.check, func() {})
	if r := do(h, "POST", "/claim", claimJSON(claimKey, "http://frontend:8080", ""), false); r.Code != 200 {
		t.Fatalf("claim = %d %s", r.Code, r.Body)
	}
	if rec.calls != 1 || rec.url != "http://frontend:8080" || rec.site != "crm.example.com" || rec.key != claimKey {
		t.Fatalf("check = %+v", rec)
	}
	if cs.Mode() != claim.ModeClaimed {
		t.Fatal("not claimed")
	}
}

func TestClaimUnreachableCallback400(t *testing.T) {
	rec := &checkRecorder{err: &outbox.UnreachableError{Detail: "http 404"}}
	h, st, cs := newServerFull(t, &fakeManager{}, config.Config{}, rec.check, func() {})
	r := do(h, "POST", "/claim", claimJSON(claimKey, "http://frontend:8080", ""), false)
	m := decode(t, r)
	if r.Code != 400 || m["error"] != "callback_unreachable" || m["detail"] != "http 404" {
		t.Fatalf("claim = %d %s", r.Code, r.Body)
	}
	if strings.Contains(r.Body.String(), claimKey) {
		t.Fatal("response leaks the key")
	}
	if rc, _ := st.GetRelayConfig(context.Background()); rc != nil || cs.Mode() != claim.ModeUnclaimed {
		t.Fatal("an unreachable callback must leave the relay unclaimed")
	}
	rec.err = errors.New("dial tcp: secret detail " + claimKey)
	r = do(h, "POST", "/claim", claimJSON(claimKey, "http://frontend:8080", ""), false)
	if r.Code != 400 || decode(t, r)["detail"] != "request error" || strings.Contains(r.Body.String(), claimKey) {
		t.Fatalf("plain error = %d %s", r.Code, r.Body)
	}
}

// A claim refused for any other reason never makes the relay send a request.
func TestRefusedClaimDoesNotProbe(t *testing.T) {
	for name, c := range map[string]struct {
		cfg  config.Config
		body string
	}{
		"env":       {config.Config{APIKey: testKey}, claimJSON(claimKey, "http://a", "")},
		"token":     {config.Config{BootstrapToken: "boot"}, claimJSON(claimKey, "http://a", "wrong")},
		"short key": {config.Config{}, claimJSON("short", "http://a", "")},
		"bad site":  {config.Config{}, `{"api_key":"` + claimKey + `","callback_url":"http://a","site_name":"a b"}`},
	} {
		rec := &checkRecorder{}
		h, _, _ := newServerFull(t, &fakeManager{}, c.cfg, rec.check, func() {})
		if r := do(h, "POST", "/claim", c.body, false); r.Code == 200 || rec.calls != 0 {
			t.Errorf("%s: %d, %d checks", name, r.Code, rec.calls)
		}
	}
}

func TestSetCallbackProbesWithCurrentKey(t *testing.T) {
	rec := &checkRecorder{}
	h, _, cs := newServerFull(t, &fakeManager{}, config.Config{}, rec.check, func() {})
	do(h, "POST", "/claim", claimJSON(claimKey, "http://127.0.0.1:8001", ""), false)
	rec.err = &outbox.UnreachableError{Detail: "timeout"}
	r := doKeyBody(h, "PUT", "/claim/callback", `{"callback_url":"http://frontend:8080","site_name":"crm.example.com"}`, claimKey)
	if r.Code != 400 || decode(t, r)["error"] != "callback_unreachable" || decode(t, r)["detail"] != "timeout" {
		t.Fatalf("callback = %d %s", r.Code, r.Body)
	}
	if rec.url != "http://frontend:8080" || rec.site != "crm.example.com" || rec.key != claimKey {
		t.Fatalf("check = %+v", rec)
	}
	if u, _, _ := cs.Target(); u != "http://127.0.0.1:8001" {
		t.Fatalf("callback changed to %q", u)
	}
	rec.err = nil
	if r := doKeyBody(h, "PUT", "/claim/callback", `{"callback_url":"http://frontend:8080","site_name":"crm.example.com"}`, claimKey); r.Code != 200 {
		t.Fatalf("reachable callback = %d", r.Code)
	}
	rec.calls = 0
	if r := doKeyBody(h, "PUT", "/claim/callback", `{"callback_url":"ftp://x","site_name":"s"}`, claimKey); r.Code != 400 || rec.calls != 0 {
		t.Fatalf("bad callback = %d, %d checks", r.Code, rec.calls)
	}
}

func TestClaimRoutesCapBodyAt64KB(t *testing.T) {
	big := `{"api_key":"` + strings.Repeat("k", 65<<10) + `"}`
	h, _, _ := newServerWith(t, &fakeManager{}, config.Config{})
	if r := do(h, "POST", "/claim", big, false); r.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("/claim = %d", r.Code)
	}
	do(h, "POST", "/claim", claimJSON(claimKey, "http://a", ""), false)
	for _, route := range []struct{ method, path string }{{"POST", "/claim/rotate"}, {"PUT", "/claim/callback"}} {
		if r := doKeyBody(h, route.method, route.path, big, claimKey); r.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s = %d", route.path, r.Code)
		}
	}
}

func TestReplayDeadRoute(t *testing.T) {
	notified := 0
	h, st, _ := newServerFull(t, &fakeManager{}, config.Config{APIKey: testKey}, reachable, func() { notified++ })
	ctx := context.Background()
	for _, m := range []string{"a", "b"} {
		st.EnqueueCallback(ctx, m, map[string]int{})
		head, _ := st.HeadCallback(ctx)
		st.KillCallback(ctx, head.ID, "http 400")
	}
	if r := do(h, "POST", "/outbox/dead/replay", "", false); r.Code != 401 {
		t.Fatalf("unauthenticated = %d", r.Code)
	}
	r := do(h, "POST", "/outbox/dead/replay", "", true)
	if r.Code != 200 || decode(t, r)["replayed"] != float64(2) || notified != 1 {
		t.Fatalf("replay = %d %s, notified %d", r.Code, r.Body, notified)
	}
	if pending, dead, _ := st.OutboxCounts(ctx); pending != 2 || dead != 0 {
		t.Fatalf("counts = %d / %d", pending, dead)
	}
}

func doKeyBody(h http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("x-api-key", key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
