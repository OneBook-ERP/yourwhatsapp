package store

import (
	"context"
	"testing"
)

func mustReserve(t *testing.T, s *Store, local, session string) *SendRecord {
	t.Helper()
	rec, err := s.ReserveSend(context.Background(), local, session)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func getSend(t *testing.T, s *Store, local string) SendRecord {
	t.Helper()
	var r SendRecord
	err := s.DB.QueryRow(`SELECT local_message_id, session_id, state, COALESCE(message_id,''), COALESCE(error_code,'') FROM sends WHERE local_message_id = ?`, local).
		Scan(&r.LocalID, &r.SessionID, &r.State, &r.MessageID, &r.ErrorCode)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReserveFresh(t *testing.T) {
	s, _ := openTemp(t)
	if rec := mustReserve(t, s, "m1", "A"); rec != nil {
		t.Fatalf("expected nil, got %+v", rec)
	}
	if got := getSend(t, s, "m1"); got.State != SendQueued || got.SessionID != "A" {
		t.Errorf("row = %+v", got)
	}
}

func TestReserveDuplicateAcrossSessions(t *testing.T) {
	s, _ := openTemp(t)
	mustReserve(t, s, "m1", "A")
	rec := mustReserve(t, s, "m1", "B")
	if rec == nil || rec.SessionID != "A" || rec.State != SendQueued {
		t.Fatalf("existing = %+v", rec)
	}
	if got := getSend(t, s, "m1"); got.SessionID != "A" {
		t.Errorf("row overwritten: %+v", got)
	}
}

func TestReserveAfterFailedAllowed(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	mustReserve(t, s, "m1", "A")
	if err := s.MarkFailed(ctx, "m1", "send_failed"); err != nil {
		t.Fatal(err)
	}
	if rec := mustReserve(t, s, "m1", "B"); rec != nil {
		t.Fatalf("expected nil, got %+v", rec)
	}
	got := getSend(t, s, "m1")
	if got.SessionID != "B" || got.State != SendQueued || got.ErrorCode != "" {
		t.Errorf("row = %+v", got)
	}
}

func TestReserveSentIsDuplicate(t *testing.T) {
	s, _ := openTemp(t)
	mustReserve(t, s, "m1", "A")
	if err := s.MarkSent(context.Background(), "m1", "WAID1"); err != nil {
		t.Fatal(err)
	}
	rec := mustReserve(t, s, "m1", "B")
	if rec == nil || rec.State != SendSent || rec.MessageID != "WAID1" {
		t.Fatalf("existing = %+v", rec)
	}
}

func TestFailQueued(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	mustReserve(t, s, "q1", "A")
	mustReserve(t, s, "q2", "B")
	mustReserve(t, s, "s1", "A")
	if err := s.MarkSent(ctx, "s1", "WAID"); err != nil {
		t.Fatal(err)
	}
	failed, err := s.FailQueued(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 2 {
		t.Fatalf("got %d failed, want 2", len(failed))
	}
	for _, id := range []string{"q1", "q2"} {
		if got := getSend(t, s, id); got.State != SendFailed || got.ErrorCode != "send_failed" {
			t.Errorf("%s = %+v", id, got)
		}
	}
	if got := getSend(t, s, "s1"); got.State != SendSent {
		t.Errorf("sent row changed: %+v", got)
	}
}
