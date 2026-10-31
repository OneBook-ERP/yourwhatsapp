package claim

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"yourwhatsapp/relay/internal/config"
	"yourwhatsapp/relay/internal/store"
)

var (
	fixedNow = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	goodKey  = strings.Repeat("k", MinKeyLen)
)

func clock() time.Time { return fixedNow }

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newState(t *testing.T, st *store.Store, cfg config.Config) *State {
	t.Helper()
	cs, err := New(st, cfg, clock)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func TestModeEnvWhenKeySet(t *testing.T) {
	st := openStore(t)
	// An env key wins even over a stored claim.
	if err := st.InsertRelayConfig(context.Background(), store.RelayConfig{APIKey: goodKey, CallbackURL: "http://stored", ClaimedAt: fixedNow}); err != nil {
		t.Fatal(err)
	}
	cs := newState(t, st, config.Config{APIKey: "envkey", FrappeBaseURL: "http://127.0.0.1:8001", FrappeSiteName: "site.local"})
	if cs.Mode() != ModeEnv {
		t.Fatalf("mode = %q, want env", cs.Mode())
	}
	if url, site, key := cs.Target(); url != "http://127.0.0.1:8001" || site != "site.local" || key != "envkey" {
		t.Fatalf("Target = %q %q %q", url, site, key)
	}
	if !cs.Authorized("envkey") || cs.Authorized(goodKey) || cs.Authorized("") {
		t.Fatal("env mode must accept only the env key")
	}
}

func TestModeClaimedFromRow(t *testing.T) {
	st := openStore(t)
	if err := st.InsertRelayConfig(context.Background(), store.RelayConfig{APIKey: goodKey, CallbackURL: "http://frontend:8080", SiteName: "crm.example.com", ClaimedAt: fixedNow}); err != nil {
		t.Fatal(err)
	}
	cs := newState(t, st, config.Config{FrappeBaseURL: "http://ignored"})
	if cs.Mode() != ModeClaimed {
		t.Fatalf("mode = %q, want claimed", cs.Mode())
	}
	if url, site, key := cs.Target(); url != "http://frontend:8080" || site != "crm.example.com" || key != goodKey {
		t.Fatalf("Target = %q %q %q", url, site, key)
	}
	if !cs.Authorized(goodKey) || cs.Authorized("wrong") {
		t.Fatal("claimed mode must accept only the stored key")
	}
}

func TestModeUnclaimed(t *testing.T) {
	cs := newState(t, openStore(t), config.Config{FrappeBaseURL: "http://127.0.0.1:8001"})
	if cs.Mode() != ModeUnclaimed {
		t.Fatalf("mode = %q, want unclaimed", cs.Mode())
	}
	if cs.Authorized("") || cs.Authorized(goodKey) {
		t.Fatal("an unclaimed relay authorizes nobody")
	}
	if url, site, key := cs.Target(); url != "" || site != "" || key != "" {
		t.Fatalf("unclaimed Target = %q %q %q, want empty", url, site, key)
	}
}

func TestClaimHappyPath(t *testing.T) {
	st := openStore(t)
	cs := newState(t, st, config.Config{})
	var hookRuns atomic.Int32
	cs.OnClaimed(func() { hookRuns.Add(1) })

	if err := cs.Claim(context.Background(), goodKey, "https://crm.example.com/", "crm.example.com", ""); err != nil {
		t.Fatal(err)
	}
	if cs.Mode() != ModeClaimed || !cs.Authorized(goodKey) {
		t.Fatalf("mode = %q after claim", cs.Mode())
	}
	if url, site, key := cs.Target(); url != "https://crm.example.com/" || site != "crm.example.com" || key != goodKey {
		t.Fatalf("Target = %q %q %q", url, site, key)
	}
	rc, err := st.GetRelayConfig(context.Background())
	if err != nil || rc == nil || rc.APIKey != goodKey || rc.CallbackURL != "https://crm.example.com/" || !rc.ClaimedAt.Equal(fixedNow) {
		t.Fatalf("stored = %+v, %v", rc, err)
	}
	if err := cs.Claim(context.Background(), goodKey, "http://x", "", ""); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("second claim err = %v", err)
	}
	if n := hookRuns.Load(); n != 1 {
		t.Fatalf("hook ran %d times, want 1", n)
	}
	// A fresh State over the same store comes up claimed.
	if again := newState(t, st, config.Config{}); again.Mode() != ModeClaimed {
		t.Fatalf("reloaded mode = %q", again.Mode())
	}
}

func TestClaimAlreadyClaimed(t *testing.T) {
	st := openStore(t)
	if err := st.InsertRelayConfig(context.Background(), store.RelayConfig{APIKey: goodKey, CallbackURL: "http://a", ClaimedAt: fixedNow}); err != nil {
		t.Fatal(err)
	}
	cs := newState(t, st, config.Config{})
	other := strings.Repeat("o", MinKeyLen)
	if err := cs.Claim(context.Background(), other, "http://b", "", ""); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("err = %v, want ErrAlreadyClaimed", err)
	}
	if _, _, key := cs.Target(); key != goodKey {
		t.Fatal("a rejected claim must not change the key")
	}
}

func TestClaimEnvModeRejected(t *testing.T) {
	st := openStore(t)
	cs := newState(t, st, config.Config{APIKey: "envkey"})
	if err := cs.Claim(context.Background(), goodKey, "http://b", "", ""); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("err = %v, want ErrAlreadyClaimed", err)
	}
	if rc, _ := st.GetRelayConfig(context.Background()); rc != nil {
		t.Fatal("env mode must not store a claim")
	}
}

func TestClaimBootstrapRequiredWrongRight(t *testing.T) {
	cs := newState(t, openStore(t), config.Config{BootstrapToken: "boot-secret"})
	for _, tok := range []string{"", "wrong"} {
		if err := cs.Claim(context.Background(), goodKey, "http://a", "", tok); !errors.Is(err, ErrBadBootstrapToken) {
			t.Fatalf("token %q: err = %v, want ErrBadBootstrapToken", tok, err)
		}
	}
	if cs.Mode() != ModeUnclaimed {
		t.Fatal("a bad token must leave the relay unclaimed")
	}
	if err := cs.Claim(context.Background(), goodKey, "http://a", "", "boot-secret"); err != nil {
		t.Fatalf("right token: %v", err)
	}
}

func TestClaimShortKeyOrBadURL400(t *testing.T) {
	cs := newState(t, openStore(t), config.Config{})
	cases := []struct{ key, url string }{
		{strings.Repeat("k", MinKeyLen-1), "http://a"},
		{goodKey, ""},
		{goodKey, "ftp://a"},
		{goodKey, "frappe:8080"},
		{goodKey, "http://"},
		{goodKey, "://bad"},
	}
	for _, c := range cases {
		err := cs.Claim(context.Background(), c.key, c.url, "", "")
		if !errors.Is(err, ErrBadRequest) {
			t.Errorf("key len %d url %q: err = %v, want ErrBadRequest", len(c.key), c.url, err)
		}
		if err != nil && strings.Contains(err.Error(), c.key) {
			t.Errorf("error %q leaks the key", err)
		}
	}
	if cs.Mode() != ModeUnclaimed {
		t.Fatal("a bad request must leave the relay unclaimed")
	}
}

func TestClaimConcurrentOnlyOneWins(t *testing.T) {
	st := openStore(t)
	cs := newState(t, st, config.Config{})
	var hookRuns atomic.Int32
	cs.OnClaimed(func() { hookRuns.Add(1) })

	const n = 20
	errs := make([]error, n)
	keys := make([]string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		keys[i] = strings.Repeat(string(rune('a'+i)), MinKeyLen)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = cs.Claim(context.Background(), keys[i], "http://a", "", "")
		}()
	}
	close(start)
	wg.Wait()

	winner := -1
	for i, err := range errs {
		switch {
		case err == nil:
			if winner >= 0 {
				t.Fatalf("claims %d and %d both won", winner, i)
			}
			winner = i
		case !errors.Is(err, ErrAlreadyClaimed):
			t.Fatalf("claim %d: unexpected err %v", i, err)
		}
	}
	if winner < 0 {
		t.Fatal("no claim won")
	}
	rc, err := st.GetRelayConfig(context.Background())
	if err != nil || rc == nil || rc.APIKey != keys[winner] {
		t.Fatalf("stored key does not match the winner: %+v, %v", rc, err)
	}
	if _, _, key := cs.Target(); key != keys[winner] {
		t.Fatal("in-memory key does not match the winner")
	}
	if n := hookRuns.Load(); n != 1 {
		t.Fatalf("hook ran %d times, want 1", n)
	}
}

func TestFreshUnclaimedStarts(t *testing.T) {
	if cs := newState(t, openStore(t), config.Config{}); cs.Mode() != ModeUnclaimed {
		t.Fatalf("mode = %q", cs.Mode())
	}
}

// A data dir with linked numbers but no claim must not be handed to whoever
// claims first.
func TestPopulatedUnclaimedRefusesWithoutToken(t *testing.T) {
	st := openStore(t)
	if err := st.UpsertAccount(context.Background(), "acc1"); err != nil {
		t.Fatal(err)
	}
	_, err := New(st, config.Config{}, clock)
	if !errors.Is(err, ErrPopulatedUnclaimed) {
		t.Fatalf("err = %v, want ErrPopulatedUnclaimed", err)
	}
	if !strings.Contains(err.Error(), "RELAY_API_KEY") || !strings.Contains(err.Error(), "RELAY_BOOTSTRAP_TOKEN") {
		t.Fatalf("error %q does not say how to fix it", err)
	}
}

func TestPopulatedUnclaimedStartsWithToken(t *testing.T) {
	st := openStore(t)
	if err := st.EnqueueCallback(context.Background(), "update_qr", map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	cs := newState(t, st, config.Config{BootstrapToken: "boot"})
	if cs.Mode() != ModeUnclaimed {
		t.Fatalf("mode = %q", cs.Mode())
	}
	if err := cs.Claim(context.Background(), goodKey, "http://a", "", ""); !errors.Is(err, ErrBadBootstrapToken) {
		t.Fatalf("claim without token err = %v", err)
	}
	if err := cs.Claim(context.Background(), goodKey, "http://a", "", "boot"); err != nil {
		t.Fatal(err)
	}
}

func TestClaimRejectsCredentialedOrDecoratedURL(t *testing.T) {
	cs := newState(t, openStore(t), config.Config{})
	for _, u := range []string{
		"http://user:pass@frappe:8080",
		"http://user@frappe:8080",
		"http://frappe:8080/?x=1",
		"http://frappe:8080/#frag",
	} {
		err := cs.Claim(context.Background(), goodKey, u, "", "")
		if !errors.Is(err, ErrBadRequest) {
			t.Errorf("%q: err = %v, want ErrBadRequest", u, err)
		}
		if err != nil && strings.Contains(err.Error(), "pass") {
			t.Errorf("%q: error leaks userinfo: %v", u, err)
		}
	}
}

const newKey = "n-ewkey-ewkey-ewkey-ewkey-ewkey-ew"

func claimedState(t *testing.T, now *time.Time) (*State, *store.Store) {
	t.Helper()
	st := openStore(t)
	cs, err := New(st, config.Config{}, func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.Claim(context.Background(), goodKey, "http://a", "site", ""); err != nil {
		t.Fatal(err)
	}
	return cs, st
}

func TestRotateGraceWindow(t *testing.T) {
	now := fixedNow
	cs, st := claimedState(t, &now)
	if err := cs.Rotate(context.Background(), newKey); err != nil {
		t.Fatal(err)
	}
	if _, _, k := cs.Target(); k != newKey {
		t.Fatal("Target must return the new key immediately")
	}
	now = fixedNow.Add(9 * time.Minute)
	if !cs.Authorized(goodKey) || !cs.Authorized(newKey) {
		t.Fatal("old and new key must both work at +9m")
	}
	now = fixedNow.Add(11 * time.Minute)
	if cs.Authorized(goodKey) || !cs.Authorized(newKey) {
		t.Fatal("old key must be rejected at +11m, new key still ok")
	}
	// The new key survives a restart; the grace does not.
	now = fixedNow
	again, err := New(st, config.Config{}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if !again.Authorized(newKey) || again.Authorized(goodKey) {
		t.Fatal("restart must keep only the persisted new key")
	}
}

func TestRotateRejectsShortKey(t *testing.T) {
	now := fixedNow
	cs, _ := claimedState(t, &now)
	if err := cs.Rotate(context.Background(), "short"); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("err = %v", err)
	}
	if !cs.Authorized(goodKey) {
		t.Fatal("failed rotate must keep the key")
	}
}

func TestRotateEnvMode409(t *testing.T) {
	cs := newState(t, openStore(t), config.Config{APIKey: "envkey"})
	if err := cs.Rotate(context.Background(), newKey); !errors.Is(err, ErrEnvMode) {
		t.Fatalf("rotate err = %v", err)
	}
	if err := cs.SetCallback(context.Background(), "http://b", "s"); !errors.Is(err, ErrEnvMode) {
		t.Fatalf("callback err = %v", err)
	}
}

func TestSetCallbackPersistsAndTargets(t *testing.T) {
	now := fixedNow
	cs, st := claimedState(t, &now)
	if err := cs.SetCallback(context.Background(), "http://frontend:8080", "crm.example.com"); err != nil {
		t.Fatal(err)
	}
	if u, s, _ := cs.Target(); u != "http://frontend:8080" || s != "crm.example.com" {
		t.Fatalf("Target = %q %q", u, s)
	}
	rc, err := st.GetRelayConfig(context.Background())
	if err != nil || rc.CallbackURL != "http://frontend:8080" || rc.SiteName != "crm.example.com" {
		t.Fatalf("stored = %+v, %v", rc, err)
	}
	for _, bad := range []string{"ftp://a", "http://u:p@a", "http://a?x=1", "http://a#f", ""} {
		if err := cs.SetCallback(context.Background(), bad, "s"); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}
