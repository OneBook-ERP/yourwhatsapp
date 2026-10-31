package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"yourwhatsapp/relay/internal/claim"
	"yourwhatsapp/relay/internal/config"
	"yourwhatsapp/relay/internal/store"
)

type fakeRestorer struct {
	restored chan struct{}
	failArms atomic.Int32
}

func newFakeRestorer() *fakeRestorer { return &fakeRestorer{restored: make(chan struct{}, 4)} }

func (f *fakeRestorer) RestoreAll(context.Context) error {
	f.restored <- struct{}{}
	return nil
}
func (f *fakeRestorer) FailLostSendsAfter(time.Duration) { f.failArms.Add(1) }

func startupState(t *testing.T, row bool) *claim.State {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if row {
		rc := store.RelayConfig{APIKey: strings.Repeat("k", claim.MinKeyLen), CallbackURL: "http://frontend:8080", ClaimedAt: time.Now()}
		if err := st.InsertRelayConfig(context.Background(), rc); err != nil {
			t.Fatal(err)
		}
	}
	cs, err := claim.New(st, config.Config{}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

var discard = slog.New(slog.NewJSONHandler(io.Discard, nil))

// A relay_config row survives a restart: the relay comes up claimed and
// restores its sessions before serving.
func TestStartupModeClaimedRestores(t *testing.T) {
	cs := startupState(t, true)
	r := newFakeRestorer()
	restoreWhenClaimed(context.Background(), cs, r, discard)
	select {
	case <-r.restored:
	default:
		t.Fatal("claimed relay did not restore sessions synchronously")
	}
	if r.failArms.Load() != 1 {
		t.Fatal("lost-send failover not armed")
	}
}

func TestStartupUnclaimedRestoresOnlyAfterClaim(t *testing.T) {
	cs := startupState(t, false)
	r := newFakeRestorer()
	restoreWhenClaimed(context.Background(), cs, r, discard)
	select {
	case <-r.restored:
		t.Fatal("unclaimed relay restored sessions")
	default:
	}
	if err := cs.Claim(context.Background(), strings.Repeat("k", claim.MinKeyLen), "http://a", "", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.restored:
	case <-time.After(5 * time.Second):
		t.Fatal("sessions not restored after claim")
	}
}

func TestClaimedSignal(t *testing.T) {
	ready := claimedSignal(startupState(t, true))
	select {
	case <-ready:
	default:
		t.Fatal("claimed relay: signal not closed at once")
	}

	cs := startupState(t, false)
	ready = claimedSignal(cs)
	select {
	case <-ready:
		t.Fatal("unclaimed relay: signal closed early")
	default:
	}
	if err := cs.Claim(context.Background(), strings.Repeat("k", claim.MinKeyLen), "http://a", "", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("signal not closed after claim")
	}
}

// A claim that lands while the relay is shutting down must not restore sessions.
func TestRestoreSessionsSkipsAfterShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := newFakeRestorer()
	restoreSessions(ctx, r, discard)
	select {
	case <-r.restored:
		t.Fatal("restored sessions after shutdown began")
	default:
	}
	if r.failArms.Load() != 0 {
		t.Fatal("armed lost-send failover after shutdown began")
	}
}
