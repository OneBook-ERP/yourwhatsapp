package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yourwhatsapp/relay/internal/claim"
	"yourwhatsapp/relay/internal/config"
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
