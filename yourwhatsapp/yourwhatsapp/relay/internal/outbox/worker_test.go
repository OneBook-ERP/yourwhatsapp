package outbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"yourwhatsapp/relay/internal/store"
)

func TestBackoff(t *testing.T) {
	for attempts, want := range map[int]time.Duration{0: time.Second, 3: 8 * time.Second, 8: 256 * time.Second, 9: 5 * time.Minute, 20: 5 * time.Minute, 100: 5 * time.Minute} {
		if got := Backoff(attempts); got != want {
			t.Errorf("Backoff(%d) = %v, want %v", attempts, got, want)
		}
	}
}

func TestDecide(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	young := store.OutboxRow{Attempts: 2, CreatedAt: now.Add(-time.Hour)}
	old := store.OutboxRow{Attempts: 2, CreatedAt: now.Add(-25 * time.Hour)}
	backoff := now.Add(4 * time.Second)
	cases := []struct {
		name   string
		status int
		err    error
		row    store.OutboxRow
		action Action
		next   time.Time
	}{
		{"200", 200, nil, young, ActionDelete, time.Time{}},
		{"500", 500, nil, young, ActionRetry, backoff},
		{"network", 0, errors.New("dial"), young, ActionRetry, backoff},
		{"429", 429, nil, young, ActionRetry, backoff},
		{"401", 401, nil, young, ActionRetry, now.Add(60 * time.Second)},
		{"403", 403, nil, old, ActionRetry, now.Add(60 * time.Second)},
		{"404 young", 404, nil, young, ActionRetry, backoff},
		{"404 old", 404, nil, old, ActionDead, time.Time{}},
		{"400", 400, nil, young, ActionDead, time.Time{}},
		{"422", 422, nil, young, ActionDead, time.Time{}},
	}
	for _, c := range cases {
		action, next := Decide(c.status, c.err, c.row, now)
		if action != c.action || !next.Equal(c.next) {
			t.Errorf("%s: got (%v, %v), want (%v, %v)", c.name, action, next, c.action, c.next)
		}
	}
}

type fakePoster struct {
	mu      sync.Mutex
	calls   []string
	respond func(method string, n int) int // n = 1-based count of calls for this method
	counts  map[string]int
}

func (f *fakePoster) Post(_ context.Context, method string, _ []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts == nil {
		f.counts = map[string]int{}
	}
	f.counts[method]++
	f.calls = append(f.calls, method)
	if f.respond == nil {
		return 200, nil
	}
	return f.respond(method, f.counts[method]), nil
}

func (f *fakePoster) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// startWorker runs a worker on a fake clock: sleeps shorter than idlePoll advance
// the clock instantly; longer ones block until Notify or cancellation.
func startWorker(t *testing.T, st *store.Store, p Poster) *Worker {
	t.Helper()
	w := NewWorker(st, p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var mu sync.Mutex
	clock := time.Now()
	w.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	w.after = func(d time.Duration) <-chan time.Time {
		if d >= idlePoll {
			return nil
		}
		mu.Lock()
		clock = clock.Add(d)
		mu.Unlock()
		ch := make(chan time.Time, 1)
		ch <- clock
		return ch
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return w
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func outboxEmpty(st *store.Store) func() bool {
	return func() bool { h, _ := st.HeadCallback(context.Background()); return h == nil }
}

func TestWorkerDeliversInOrder(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	for _, m := range []string{MethodInbound, MethodStatus, MethodQR} {
		st.EnqueueCallback(ctx, m, map[string]string{"k": "v"})
	}
	p := &fakePoster{}
	startWorker(t, st, p)
	eventually(t, outboxEmpty(st))
	got := p.Calls()
	want := []string{MethodInbound, MethodStatus, MethodQR}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestWorkerHeadOfLineBlocks(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	st.EnqueueCallback(ctx, MethodInbound, 1)
	st.EnqueueCallback(ctx, MethodStatus, 2)
	p := &fakePoster{respond: func(method string, n int) int {
		if method == MethodInbound && n == 1 {
			return 500
		}
		return 200
	}}
	startWorker(t, st, p)
	eventually(t, outboxEmpty(st))
	got := p.Calls()
	want := []string{MethodInbound, MethodInbound, MethodStatus}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestWorkerFutureHeadWaitsForNotify(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	st.EnqueueCallback(ctx, MethodInbound, 1)
	st.EnqueueCallback(ctx, MethodStatus, 2)
	h, _ := st.HeadCallback(ctx)
	st.RetryCallback(ctx, h.ID, time.Now().Add(time.Hour), "earlier failure")
	p := &fakePoster{}
	w := startWorker(t, st, p)
	time.Sleep(50 * time.Millisecond)
	if got := p.Calls(); len(got) != 0 {
		t.Fatalf("worker skipped ahead of a future head: %v", got)
	}
	w.Notify() // wakes the worker, but the head is still not due
	time.Sleep(50 * time.Millisecond)
	if got := p.Calls(); len(got) != 0 {
		t.Fatalf("worker posted before head was due: %v", got)
	}
}

func TestWorkerDeadLetters(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	st.EnqueueCallback(ctx, MethodInbound, 1)
	st.EnqueueCallback(ctx, MethodStatus, 2)
	p := &fakePoster{respond: func(method string, _ int) int {
		if method == MethodInbound {
			return 400
		}
		return 200
	}}
	startWorker(t, st, p)
	eventually(t, outboxEmpty(st))
	dead, err := st.ListDead(ctx, 10)
	if err != nil || len(dead) != 1 || dead[0].Method != MethodInbound || dead[0].LastError != "http 400" {
		t.Fatalf("dead = %+v, %v", dead, err)
	}
	if got := p.Calls(); len(got) != 2 || got[1] != MethodStatus {
		t.Fatalf("calls = %v", got)
	}
}

func TestFrappePosterHeaders(t *testing.T) {
	var gotPath, gotKey, gotHost, gotType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey, gotHost, gotType = r.URL.Path, r.Header.Get("X-Api-Key"), r.Host, r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(202)
	}))
	defer srv.Close()
	status, err := NewFrappePoster(srv.URL+"/", "whatsapp.localhost", "secret").Post(context.Background(), MethodQR, []byte(`{"a":1}`))
	if err != nil || status != 202 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if gotPath != "/api/method/yourwhatsapp.yourwhatsapp.api.update_qr" || gotKey != "secret" ||
		gotHost != "whatsapp.localhost" || gotType != "application/json" || gotBody != `{"a":1}` {
		t.Errorf("path=%q key=%q host=%q type=%q body=%q", gotPath, gotKey, gotHost, gotType, gotBody)
	}
}
