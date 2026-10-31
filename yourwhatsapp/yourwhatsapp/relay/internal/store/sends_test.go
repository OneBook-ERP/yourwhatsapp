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
	if _, err := s.MarkFailed(ctx, "m1", "A", "send_failed"); err != nil {
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
	if _, err := s.MarkSent(context.Background(), "m1", "A", "WAID1"); err != nil {
		t.Fatal(err)
	}
	rec := mustReserve(t, s, "m1", "B")
	if rec == nil || rec.State != SendSent || rec.MessageID != "WAID1" {
		t.Fatalf("existing = %+v", rec)
	}
}

func TestListQueued(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	mustReserve(t, s, "q1", "A")
	mustReserve(t, s, "q2", "B")
	mustReserve(t, s, "s1", "A")
	if _, err := s.MarkSent(ctx, "s1", "A", "WAID"); err != nil {
		t.Fatal(err)
	}
	queued, err := s.ListQueued(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 2 || queued[0].LocalID != "q1" || queued[1].LocalID != "q2" || queued[1].SessionID != "B" {
		t.Fatalf("queued = %+v", queued)
	}
	if got := getSend(t, s, "q1"); got.State != SendQueued {
		t.Errorf("ListQueued changed a row: %+v", got)
	}
}

func TestMarkResultGuardedBySessionAndState(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	mustReserve(t, s, "m1", "A")
	if ok, err := s.MarkFailed(ctx, "m1", "A", "send_failed"); err != nil || !ok {
		t.Fatalf("MarkFailed A = %v, %v", ok, err)
	}
	mustReserve(t, s, "m1", "B")

	// A late result from session A must not clobber B's reservation.
	if ok, err := s.MarkSent(ctx, "m1", "A", "LATE"); err != nil || ok {
		t.Fatalf("MarkSent A = %v, %v; want false", ok, err)
	}
	if ok, err := s.MarkFailed(ctx, "m1", "A", "send_failed"); err != nil || ok {
		t.Fatalf("MarkFailed A = %v, %v; want false", ok, err)
	}
	if got := getSend(t, s, "m1"); got.SessionID != "B" || got.State != SendQueued {
		t.Fatalf("row = %+v", got)
	}

	if ok, err := s.MarkSent(ctx, "m1", "B", "WAID"); err != nil || !ok {
		t.Fatalf("MarkSent B = %v, %v", ok, err)
	}
	// A sent row is final.
	if ok, _ := s.MarkFailed(ctx, "m1", "B", "send_failed"); ok {
		t.Fatal("MarkFailed overwrote a sent row")
	}
}

func TestGetSend(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	if rec, err := s.GetSend(ctx, "nope"); err != nil || rec != nil {
		t.Fatalf("GetSend missing = %+v, %v", rec, err)
	}
	mustReserve(t, s, "m1", "A")
	rec, err := s.GetSend(ctx, "m1")
	if err != nil || rec == nil || rec.SessionID != "A" || rec.State != SendQueued {
		t.Fatalf("GetSend = %+v, %v", rec, err)
	}
}

func TestMarkUnconfirmedKeepsMessageID(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	mustReserve(t, s, "m1", "A")
	if ok, err := s.MarkUnconfirmed(ctx, "m1", "B", "WAID"); err != nil || ok {
		t.Fatalf("MarkUnconfirmed from another session = %v, %v; want false", ok, err)
	}
	if ok, err := s.MarkUnconfirmed(ctx, "m1", "A", "WAID"); err != nil || !ok {
		t.Fatalf("MarkUnconfirmed = %v, %v", ok, err)
	}
	got := getSend(t, s, "m1")
	if got.State != SendFailed || got.ErrorCode != ErrCodeSendUnconfirmed || got.MessageID != "WAID" {
		t.Fatalf("row = %+v", got)
	}
}
