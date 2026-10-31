package store

import (
	"context"
	"testing"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func TestOpenCreatesTables(t *testing.T) {
	s, dir := openTemp(t)
	for _, name := range []string{"accounts", "sends", "outbox", "outbox_dead", "relay_config", "whatsmeow_device"} {
		var got string
		err := s.DB.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&got)
		if err != nil {
			t.Errorf("table %s missing: %v", name, err)
		}
	}
	s2, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("second Open failed: %v", err)
	}
	s2.Close()
}

func TestAccountsRoundTrip(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := s.UpsertAccount(ctx, "a"); err != nil {
			t.Fatal(err)
		}
	}
	s.UpsertAccount(ctx, "b")
	s.UpsertAccount(ctx, "c")

	list, _ := s.ListRestorable(ctx)
	if len(list) != 0 {
		t.Fatalf("unpaired accounts must not restore: %+v", list)
	}

	s.SetAccountJID(ctx, "a", "111@s.whatsapp.net")
	s.SetAccountJID(ctx, "b", "222@s.whatsapp.net")
	s.SetAccountPaused(ctx, "b", true)
	s.SetAccountProxy(ctx, "a", "http://p:1")

	list, err := s.ListRestorable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].SessionID != "a" || list[0].JID != "111@s.whatsapp.net" || list[0].Proxy != "http://p:1" {
		t.Fatalf("unexpected restorable: %+v", list)
	}

	s.SetAccountJID(ctx, "a", "")
	if list, _ = s.ListRestorable(ctx); len(list) != 0 {
		t.Fatalf("empty jid must be excluded: %+v", list)
	}

	acc, ok, err := s.GetAccount(ctx, "b")
	if err != nil || !ok || !acc.Paused {
		t.Fatalf("GetAccount b: %+v %v %v", acc, ok, err)
	}
	if _, ok, _ := s.GetAccount(ctx, "zzz"); ok {
		t.Fatal("unknown account found")
	}
}
