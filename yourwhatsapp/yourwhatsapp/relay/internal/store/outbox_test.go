package store

import (
	"context"
	"testing"
	"time"
)

func TestOutboxEnqueueHeadOrder(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	if h, err := s.HeadCallback(ctx); err != nil || h != nil {
		t.Fatalf("empty head = %v, %v", h, err)
	}
	for _, m := range []string{"a", "b"} {
		if err := s.EnqueueCallback(ctx, m, map[string]int{"n": 1}); err != nil {
			t.Fatal(err)
		}
	}
	h, err := s.HeadCallback(ctx)
	if err != nil || h == nil || h.Method != "a" || string(h.Payload) != `{"n":1}` || h.Attempts != 0 {
		t.Fatalf("head = %+v, %v", h, err)
	}
	if err := s.DeleteCallback(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	if h, _ = s.HeadCallback(ctx); h == nil || h.Method != "b" {
		t.Fatalf("head after delete = %+v", h)
	}
}

func TestOutboxRetry(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	s.EnqueueCallback(ctx, "a", 1)
	h, _ := s.HeadCallback(ctx)
	next := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	if err := s.RetryCallback(ctx, h.ID, next, "boom"); err != nil {
		t.Fatal(err)
	}
	h, _ = s.HeadCallback(ctx)
	if h.Attempts != 1 || h.LastError != "boom" || !h.NextAttemptAt.Equal(next) {
		t.Fatalf("row = %+v", h)
	}
}

func TestOutboxKillMovesToDead(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	s.EnqueueCallback(ctx, "a", 1)
	s.EnqueueCallback(ctx, "b", 2)
	h, _ := s.HeadCallback(ctx)
	if err := s.KillCallback(ctx, h.ID, "bad request"); err != nil {
		t.Fatal(err)
	}
	if h2, _ := s.HeadCallback(ctx); h2 == nil || h2.Method != "b" {
		t.Fatalf("head = %+v", h2)
	}
	dead, err := s.ListDead(ctx, 10)
	if err != nil || len(dead) != 1 || dead[0].ID != h.ID || dead[0].Method != "a" || dead[0].LastError != "bad request" {
		t.Fatalf("dead = %+v, %v", dead, err)
	}
}
