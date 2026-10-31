package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrRelayConfigExists means the relay is already claimed.
var ErrRelayConfigExists = errors.New("relay config already exists")

// RelayConfig is the relay's claimed identity: the key Frappe authenticates
// with and the callback target. It is a credential, like the rest of relay.db.
type RelayConfig struct {
	APIKey      string
	CallbackURL string
	SiteName    string
	ClaimedAt   time.Time
}

// GetRelayConfig returns the stored claim, or nil when the relay is unclaimed.
func (s *Store) GetRelayConfig(ctx context.Context) (*RelayConfig, error) {
	var rc RelayConfig
	var claimedAt string
	err := s.DB.QueryRowContext(ctx,
		`SELECT api_key, callback_url, site_name, claimed_at FROM relay_config WHERE id = 1`,
	).Scan(&rc.APIKey, &rc.CallbackURL, &rc.SiteName, &claimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if rc.ClaimedAt, err = time.Parse(time.RFC3339Nano, claimedAt); err != nil {
		return nil, fmt.Errorf("parse claimed_at: %w", err)
	}
	return &rc, nil
}

// InsertRelayConfig stores the claim. The single-row primary key makes it
// atomic: when a row already exists it returns ErrRelayConfigExists.
func (s *Store) InsertRelayConfig(ctx context.Context, rc RelayConfig) error {
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO relay_config (id, api_key, callback_url, site_name, claimed_at)
		 VALUES (1, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		rc.APIKey, rc.CallbackURL, rc.SiteName, rc.ClaimedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrRelayConfigExists
	}
	return nil
}

// HasAnyData reports whether the store holds linked numbers or relay state:
// accounts, whatsmeow devices, or queued callbacks.
func (s *Store) HasAnyData(ctx context.Context) (bool, error) {
	var has bool
	err := s.DB.QueryRowContext(ctx, `SELECT
		EXISTS (SELECT 1 FROM accounts) OR
		EXISTS (SELECT 1 FROM whatsmeow_device) OR
		EXISTS (SELECT 1 FROM outbox)`).Scan(&has)
	return has, err
}

// UpdateRelayKey replaces the stored API key of a claimed relay.
func (s *Store) UpdateRelayKey(ctx context.Context, apiKey string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE relay_config SET api_key = ? WHERE id = 1`, apiKey)
	return err
}

// UpdateRelayCallback replaces the stored callback target of a claimed relay.
func (s *Store) UpdateRelayCallback(ctx context.Context, callbackURL, siteName string) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE relay_config SET callback_url = ?, site_name = ? WHERE id = 1`, callbackURL, siteName)
	return err
}
