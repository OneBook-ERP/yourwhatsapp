// Package outbox delivers relay-to-Frappe callbacks from a persistent queue in
// strict id order: a failing head row blocks every row behind it.
package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"yourwhatsapp/relay/internal/store"
)

type Action int

const (
	ActionDelete Action = iota
	ActionRetry
	ActionDead
)

const (
	maxBackoff     = 5 * time.Minute
	authRetryDelay = 60 * time.Second
	notFoundGrace  = 24 * time.Hour
	idlePoll       = time.Minute
)

// Backoff returns min(1s * 2^attempts, 5m).
func Backoff(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	if attempts >= 9 { // 2^9 s = 512s > 5m
		return maxBackoff
	}
	return min(time.Second<<attempts, maxBackoff)
}

// Decide maps a delivery outcome to what happens to the row. next is only
// meaningful for ActionRetry.
func Decide(status int, err error, row store.OutboxRow, now time.Time) (Action, time.Time) {
	backoff := now.Add(Backoff(row.Attempts))
	switch {
	case err != nil:
		return ActionRetry, backoff
	case status >= 200 && status < 300:
		return ActionDelete, time.Time{}
	case status == 401 || status == 403:
		return ActionRetry, now.Add(authRetryDelay)
	case status == 404:
		if now.Sub(row.CreatedAt) > notFoundGrace {
			return ActionDead, time.Time{}
		}
		return ActionRetry, backoff
	case status == 429 || status >= 500:
		return ActionRetry, backoff
	default:
		return ActionDead, time.Time{}
	}
}

type Worker struct {
	st     *store.Store
	poster Poster
	log    *slog.Logger
	wake   chan struct{}

	now   func() time.Time
	after func(d time.Duration) <-chan time.Time
}

func NewWorker(st *store.Store, p Poster, log *slog.Logger) *Worker {
	return &Worker{st: st, poster: p, log: log, wake: make(chan struct{}, 1), now: time.Now, after: time.After}
}

// Notify wakes the worker without blocking.
func (w *Worker) Notify() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run processes the outbox until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		head, err := w.st.HeadCallback(ctx)
		if err != nil {
			if ctx.Err() == nil {
				w.log.Error("outbox head failed", "err", err)
			}
			w.wait(ctx, idlePoll)
			continue
		}
		if head == nil {
			w.wait(ctx, idlePoll)
			continue
		}
		if d := head.NextAttemptAt.Sub(w.now()); d > 0 {
			w.wait(ctx, d)
			continue
		}
		w.deliver(ctx, head)
	}
}

// wait sleeps for d or until Notify or cancellation.
func (w *Worker) wait(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-w.wake:
	case <-w.after(d):
	}
}

func (w *Worker) deliver(ctx context.Context, row *store.OutboxRow) {
	status, err := w.poster.Post(ctx, row.Method, row.Payload)
	if ctx.Err() != nil {
		return
	}
	now := w.now()
	action, next := Decide(status, err, *row, now)
	msg := describe(status, err)
	log := w.log.With("outbox_id", row.ID, "method", row.Method, "attempts", row.Attempts, "status", status)

	var opErr error
	switch action {
	case ActionDelete:
		opErr = w.st.DeleteCallback(ctx, row.ID)
	case ActionRetry:
		if status == 401 || status == 403 {
			log.Error("outbox callback rejected, check relay API key", "retry_at", next)
		} else {
			log.Warn("outbox callback failed, will retry", "err", msg, "retry_at", next)
		}
		opErr = w.st.RetryCallback(ctx, row.ID, next, msg)
	case ActionDead:
		log.Error("outbox callback dead-lettered", "err", msg)
		opErr = w.st.KillCallback(ctx, row.ID, msg)
	}
	if opErr != nil && ctx.Err() == nil {
		log.Error("outbox bookkeeping failed", "err", opErr)
		w.wait(ctx, idlePoll)
	}
}

func describe(status int, err error) string {
	if err != nil {
		return "network error: " + err.Error()
	}
	return fmt.Sprintf("http %d", status)
}
