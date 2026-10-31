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

// seedDevice inserts a minimal linked-device row, as whatsmeow would after pairing.
func seedDevice(ctx context.Context, s *Store) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO whatsmeow_device (jid, registration_id, noise_key, identity_key,
		signed_pre_key, signed_pre_key_id, signed_pre_key_sig, adv_key, adv_details,
		adv_account_sig, adv_account_sig_key, adv_device_sig)
		VALUES ('1@s.whatsapp.net', 1, zeroblob(32), zeroblob(32), zeroblob(32), 1, zeroblob(64),
		x'00', x'00', zeroblob(64), zeroblob(32), zeroblob(64))`)
	return err
}

func TestHasAnyData(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		seed func(s *Store) error
	}{
		{"account", func(s *Store) error { return s.UpsertAccount(ctx, "acc1") }},
		{"outbox", func(s *Store) error { return s.EnqueueCallback(ctx, "update_qr", map[string]any{"a": 1}) }},
		{"device", func(s *Store) error { return seedDevice(ctx, s) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := openTemp(t)
			if has, err := s.HasAnyData(ctx); err != nil || has {
				t.Fatalf("fresh store HasAnyData = %v, %v", has, err)
			}
			if err := c.seed(s); err != nil {
				t.Fatal(err)
			}
			if has, err := s.HasAnyData(ctx); err != nil || !has {
				t.Fatalf("after %s HasAnyData = %v, %v", c.name, has, err)
			}
		})
	}
}
