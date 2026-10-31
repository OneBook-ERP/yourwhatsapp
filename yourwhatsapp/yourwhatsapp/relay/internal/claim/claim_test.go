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
