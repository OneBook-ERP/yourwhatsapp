package session

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types/events"

	"yourwhatsapp/relay/internal/outbox"
)

type fakeTimer struct {
	delay   time.Duration
	fn      func()
	stopped bool
}

// fakeTimers replaces the manager's afterFunc; nothing fires until the test
// says so.
type fakeTimers struct {
	mu     sync.Mutex
	timers []*fakeTimer
}

func installFakeTimers(h *harness) *fakeTimers {
	ft := &fakeTimers{}
	h.m.afterFunc = func(d time.Duration, fn func()) func() bool {
		t := &fakeTimer{delay: d, fn: fn}
		ft.mu.Lock()
		ft.timers = append(ft.timers, t)
		ft.mu.Unlock()
		return func() bool {
			ft.mu.Lock()
			defer ft.mu.Unlock()
			was := !t.stopped
			t.stopped = true
			return was
		}
	}
	h.m.rnd = func() float64 { return 0.5 } // no jitter
	return ft
}

// pending returns the timers not yet stopped or fired whose delay matches.
func (ft *fakeTimers) pending(match func(time.Duration) bool) []*fakeTimer {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	var out []*fakeTimer
	for _, t := range ft.timers {
		if !t.stopped && match(t.delay) {
			out = append(out, t)
		}
	}
	return out
}

func isGrace(d time.Duration) bool { return d == disconnectGrace }
func isRetry(d time.Duration) bool { return d != disconnectGrace }

// fireOne fires the single pending timer matching match.
func (ft *fakeTimers) fireOne(t *testing.T, what string, match func(time.Duration) bool) time.Duration {
	t.Helper()
	p := ft.pending(match)
	if len(p) != 1 {
		t.Fatalf("pending %s timers = %d, want 1", what, len(p))
	}
	ft.mu.Lock()
	p[0].stopped = true // fired
	ft.mu.Unlock()
	p[0].fn()
	return p[0].delay
}

func failConnects(upTo int) func(int) error {
	return func(n int) error {
		if upTo < 0 || n <= upTo {
			return errFake
		}
		return nil
	}
}

// The 2026-11-02 incident: at boot Connect() failed and nothing retried.
func TestRestoreRetriesFailedConnect(t *testing.T) {
	h := newHarness(t)
	ft := installFakeTimers(h)
	h.setup = func(fc *fakeClient) { fc.connectErr = failConnects(2) }
	h.pairedAccount("A", testJID)
	if err := h.m.RestoreAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := h.m.Status("A"); st != StatusDisconnected {
		t.Fatalf("status after failed restore = %s", st)
	}
	if d := ft.fireOne(t, "retry", isRetry); d != 5*time.Second {
		t.Errorf("first retry after %v, want 5s", d)
	}
	if d := ft.fireOne(t, "retry", isRetry); d != 10*time.Second {
		t.Errorf("second retry after %v, want 10s", d)
	}
	fc := h.client(0)
	if fc.connects != 3 || h.clientCount() != 1 {
		t.Fatalf("connects = %d, clients = %d; want 3 on the same client", fc.connects, h.clientCount())
	}
	if st, _, _ := h.m.Status("A"); st != StatusConnecting {
		t.Fatalf("status after successful retry = %s", st)
	}
	fc.emit(eventConnected())
	if st, _, _ := h.m.Status("A"); st != StatusConnected {
		t.Errorf("status = %s, want connected without a manual Start", st)
	}
	if p := ft.pending(isRetry); len(p) != 0 {
		t.Errorf("retry timers left pending: %d", len(p))
	}
	if got := statusPushes(h); len(got) != 1 || got[0] != "Active" {
		t.Errorf("pushes = %v, want only Active (down < 60 s)", got)
	}
}

func TestReconnectBackoffSequenceAndCap(t *testing.T) {
	h := newHarness(t)
	ft := installFakeTimers(h)
	h.setup = func(fc *fakeClient) { fc.connectErr = failConnects(-1) }
	h.pairedAccount("A", testJID)
	if err := h.m.RestoreAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{5, 10, 20, 40, 80, 160, 300, 300, 300}
	for i, w := range want {
		if d := ft.fireOne(t, "retry", isRetry); d != w*time.Second {
			t.Errorf("retry %d after %v, want %v", i+1, d, w*time.Second)
		}
	}
	if c := h.client(0).connects; c != len(want)+1 {
		t.Errorf("connects = %d, want %d", c, len(want)+1)
	}
}

func TestReconnectDelayJitter(t *testing.T) {
	for _, tc := range []struct {
		attempt int
		r       float64
		want    time.Duration
	}{
		{0, 0, 4 * time.Second},
		{0, 1, 6 * time.Second},
		{6, 0, 240 * time.Second},
		{100, 1, 360 * time.Second},
	} {
		if got := reconnectDelay(tc.attempt, func() float64 { return tc.r }); got != tc.want {
			t.Errorf("reconnectDelay(%d, %v) = %v, want %v", tc.attempt, tc.r, got, tc.want)
		}
	}
}

// A ConnectFailure after a working connection also retries, on the same
// client. (ClientOutdated does not: TestClientOutdatedReportsImmediatelyWithoutRetry.)
func TestConnectFailureEventRetries(t *testing.T) {
	for name, evt := range map[string]any{
		"connect failure": &events.ConnectFailure{Reason: events.ConnectFailureReason(499)},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			ft := installFakeTimers(h)
			fc := h.connected("A", testJID)
			fc.emit(evt)
			fc.emit(evt) // a second event must not start a second loop
			if p := ft.pending(isRetry); len(p) != 1 {
				t.Fatalf("retry timers = %d, want 1", len(p))
			}
			ft.fireOne(t, "retry", isRetry)
			if fc.connects != 2 || h.clientCount() != 1 {
				t.Errorf("connects = %d, clients = %d", fc.connects, h.clientCount())
			}
		})
	}
}

func TestStreamReplacedDoesNotRetry(t *testing.T) {
	h := newHarness(t)
	ft := installFakeTimers(h)
	fc := h.connected("A", testJID)
	fc.emit(&events.StreamReplaced{})
	if p := ft.pending(func(time.Duration) bool { return true }); len(p) != 0 {
		t.Errorf("timers pending after StreamReplaced: %d", len(p))
	}
}

func TestReconnectStopsOnPauseLogoutAndLoggedOut(t *testing.T) {
	stops := map[string]func(h *harness, fc *fakeClient){
		"pause": func(h *harness, _ *fakeClient) {
			if err := h.m.Pause(context.Background(), "A"); err != nil {
				h.t.Fatal(err)
			}
		},
		"api logout": func(h *harness, _ *fakeClient) {
			if err := h.m.Logout(context.Background(), "A"); err != nil {
				h.t.Fatal(err)
			}
		},
		"logged out by WhatsApp": func(_ *harness, fc *fakeClient) {
			fc.emit(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
		},
		"shutdown": func(h *harness, _ *fakeClient) {
			if err := h.m.Shutdown(context.Background()); err != nil {
				h.t.Fatal(err)
			}
		},
	}
	for name, stop := range stops {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			ft := installFakeTimers(h)
			fc := h.connected("A", testJID)
			fc.connectErr = failConnects(-1)
			fc.emit(&events.ConnectFailure{Reason: events.ConnectFailureReason(499)})
			retries := ft.pending(isRetry)
			if len(retries) != 1 {
				t.Fatalf("retry timers = %d, want 1", len(retries))
			}
			stop(h, fc)
			if p := ft.pending(func(time.Duration) bool { return true }); len(p) != 0 {
				t.Errorf("timers still pending: %d", len(p))
			}
			retries[0].fn() // a timer that fired just before it was stopped
			if fc.connects != 1 {
				t.Errorf("connects = %d, want no reconnect", fc.connects)
			}
		})
	}
}

func TestConcurrentStartKeepsOneReconnectLoop(t *testing.T) {
	h := newHarness(t)
	ft := installFakeTimers(h)
	h.setup = func(fc *fakeClient) { fc.connectErr = failConnects(-1) }
	h.pairedAccount("A", testJID)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.m.Start(context.Background(), "A", nil)
		}()
	}
	wg.Wait()
	if p := ft.pending(isRetry); len(p) != 1 {
		t.Fatalf("retry timers = %d, want 1", len(p))
	}
	before := h.clientCount()
	ft.fireOne(t, "retry", isRetry)
	if h.clientCount() != before {
		t.Errorf("retry built a new client")
	}
}

// Frappe hears Disconnected only once a session has been down for 60 s
// straight, and only once per outage.
func TestDisconnectedPushedAfterGrace(t *testing.T) {
	h := newHarness(t)
	ft := installFakeTimers(h)
	h.setup = func(fc *fakeClient) { fc.connectErr = failConnects(3) }
	h.pairedAccount("A", testJID)
	if err := h.m.RestoreAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	ft.fireOne(t, "retry", isRetry) // fails again
	if got := statusPushes(h); len(got) != 0 {
		t.Fatalf("pushes before grace = %v", got)
	}
	ft.fireOne(t, "grace", isGrace)
	if got := statusPushes(h); len(got) != 1 || got[0] != "Disconnected" {
		t.Fatalf("pushes after grace = %v, want one Disconnected", got)
	}
	ft.fireOne(t, "retry", isRetry) // fails again: no second push
	if got := statusPushes(h); len(got) != 1 {
		t.Errorf("pushes = %v, want still one", got)
	}
	ft.fireOne(t, "retry", isRetry) // succeeds
	h.client(0).emit(eventConnected())
	if got := statusPushes(h); len(got) != 2 || got[1] != "Active" {
		t.Errorf("pushes = %v, want Disconnected then Active", got)
	}
	if p := ft.pending(isGrace); len(p) != 0 {
		t.Errorf("grace timer still pending after connect")
	}
}

// A socket drop that whatsmeow's own auto-reconnect repairs within 60 s is
// not reported; one that lasts longer is.
func TestSocketDropReportedOnlyAfterGrace(t *testing.T) {
	h := newHarness(t)
	ft := installFakeTimers(h)
	fc := h.connected("A", testJID)

	fc.Disconnect()
	fc.emit(&events.Disconnected{})
	if p := ft.pending(isRetry); len(p) != 0 {
		t.Errorf("relay retry started although whatsmeow auto-reconnects")
	}
	grace := ft.pending(isGrace)
	if len(grace) != 1 {
		t.Fatalf("grace timers = %d, want 1", len(grace))
	}
	fc.Connect()
	fc.emit(eventConnected())
	grace[0].fn() // stale: fired just as it was stopped
	if got := statusPushes(h); len(got) != 1 {
		t.Fatalf("pushes = %v, want only the first Active", got)
	}

	fc.Disconnect()
	fc.emit(&events.Disconnected{})
	ft.fireOne(t, "grace", isGrace)
	if p := ft.pending(isRetry); len(p) != 0 {
		t.Errorf("relay retry started although whatsmeow owns the socket")
	}
	if st, _, _ := h.m.Status("A"); st != StatusDisconnected {
		t.Errorf("status = %s", st)
	}
	if got := statusPushes(h); len(got) != 2 || got[1] != "Disconnected" {
		t.Errorf("pushes = %v", got)
	}
	fc.Connect()
	fc.emit(eventConnected())
	if got := lastStatusPush(h); got != "Active" {
		t.Errorf("last push = %q, want Active on reconnect", got)
	}
	if n := len(h.callbacks(outbox.MethodQR)); n != 3 {
		t.Errorf("update_qr callbacks = %d, want 3", n)
	}
}

// whatsmeow's own auto-reconnect may already hold the socket.
func TestDialAlreadyConnectedIsSuccess(t *testing.T) {
	h := newHarness(t)
	ft := installFakeTimers(h)
	h.setup = func(fc *fakeClient) {
		fc.connectErr = func(int) error { return fmt.Errorf("connect: %w", whatsmeow.ErrAlreadyConnected) }
	}
	h.pairedAccount("A", testJID)
	if err := h.m.RestoreAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := h.m.Status("A"); st != StatusConnecting {
		t.Errorf("status = %s, want connecting", st)
	}
	if p := ft.pending(isRetry); len(p) != 0 {
		t.Errorf("retry scheduled for an already-connected client")
	}
}

// whatsmeow deleted the device (logged out on connect): terminal, like LoggedOut.
func TestDialDeviceDeletedIsTerminal(t *testing.T) {
	h := newHarness(t)
	ft := installFakeTimers(h)
	h.setup = func(fc *fakeClient) { fc.connectErr = func(int) error { return wastore.ErrDeviceDeleted } }
	h.pairedAccount("A", testJID)
	if err := h.m.RestoreAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := h.m.Status("A"); st != StatusLoggedOut {
		t.Errorf("status = %s, want logged_out", st)
	}
	if acct, _, _ := h.st.GetAccount(context.Background(), "A"); acct.JID != "" {
		t.Errorf("jid = %q, want cleared", acct.JID)
	}
	if got := statusPushes(h); len(got) != 1 || got[0] != "Disconnected" {
		t.Errorf("pushes = %v, want one immediate Disconnected", got)
	}
	if p := ft.pending(func(time.Duration) bool { return true }); len(p) != 0 {
		t.Errorf("timers pending: %d", len(p))
	}
}

// Connect() returned but login never completed and no whatsmeow loop owns
// the socket: after the grace period the relay reports and retries itself.
func TestStuckConnectingRetriesAfterGrace(t *testing.T) {
	h := newHarness(t)
	ft := installFakeTimers(h)
	h.pairedAccount("A", testJID)
	if err := h.m.RestoreAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	ft.fireOne(t, "grace", isGrace)
	if got := statusPushes(h); len(got) != 1 || got[0] != "Disconnected" {
		t.Errorf("pushes = %v", got)
	}
	if st, _, _ := h.m.Status("A"); st != StatusDisconnected {
		t.Errorf("status = %s", st)
	}
	ft.fireOne(t, "retry", isRetry)
	fc := h.client(0)
	if fc.connects != 2 || fc.disconnects == 0 {
		t.Errorf("connects = %d, disconnects = %d; want the stuck socket dropped and redialled", fc.connects, fc.disconnects)
	}
}
