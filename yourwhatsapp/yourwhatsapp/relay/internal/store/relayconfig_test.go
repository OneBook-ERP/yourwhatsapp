package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRelayConfigEmptyIsNil(t *testing.T) {
	s, _ := openTemp(t)
	rc, err := s.GetRelayConfig(context.Background())
	if err != nil || rc != nil {
		t.Fatalf("GetRelayConfig = %+v, %v; want nil, nil", rc, err)
	}
}

func TestRelayConfigInsertOnceAndSurvivesReopen(t *testing.T) {
	s, dir := openTemp(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	want := RelayConfig{APIKey: "k1", CallbackURL: "http://frappe:8080", SiteName: "crm.example.com", ClaimedAt: at}
	if err := s.InsertRelayConfig(ctx, want); err != nil {
		t.Fatal(err)
	}
	second := RelayConfig{APIKey: "k2", CallbackURL: "http://other", SiteName: "x", ClaimedAt: at}
	if err := s.InsertRelayConfig(ctx, second); !errors.Is(err, ErrRelayConfigExists) {
		t.Fatalf("second insert err = %v, want ErrRelayConfigExists", err)
	}
	s.Close()

	s2, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.GetRelayConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("GetRelayConfig = %+v, %v", got, err)
	}
	if got.APIKey != want.APIKey || got.CallbackURL != want.CallbackURL || got.SiteName != want.SiteName || !got.ClaimedAt.Equal(at) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
