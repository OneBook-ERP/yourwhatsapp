package api

import (
	"context"
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
