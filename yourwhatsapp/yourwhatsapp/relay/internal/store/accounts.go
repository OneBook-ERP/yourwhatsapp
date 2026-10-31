package store

import (
	"context"
	"database/sql"
	"errors"
)

type Account struct {
	SessionID string
	JID       string
	Proxy     string
	Paused    bool
}

func (s *Store) UpsertAccount(ctx context.Context, sessionID string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO accounts (session_id) VALUES (?) ON CONFLICT(session_id) DO NOTHING`, sessionID)
	return err
}

// SetAccountJID stores the paired JID; an empty string clears it (NULL).
func (s *Store) SetAccountJID(ctx context.Context, sessionID, jid string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE accounts SET jid = NULLIF(?, '') WHERE session_id = ?`, jid, sessionID)
	return err
}

func (s *Store) SetAccountPaused(ctx context.Context, sessionID string, paused bool) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE accounts SET paused = ? WHERE session_id = ?`, paused, sessionID)
	return err
}

// SetAccountProxy stores the proxy URL; an empty string clears it (NULL).
func (s *Store) SetAccountProxy(ctx context.Context, sessionID, proxy string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE accounts SET proxy = NULLIF(?, '') WHERE session_id = ?`, proxy, sessionID)
	return err
}

const accountColumns = `session_id, COALESCE(jid, ''), COALESCE(proxy, ''), paused`

func (s *Store) GetAccount(ctx context.Context, sessionID string) (Account, bool, error) {
	var a Account
	err := s.DB.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE session_id = ?`, sessionID).
		Scan(&a.SessionID, &a.JID, &a.Proxy, &a.Paused)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, err
	}
	return a, true, nil
}

// ListRestorable returns paired, unpaused accounts to bring up at boot.
func (s *Store) ListRestorable(ctx context.Context) ([]Account, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE jid IS NOT NULL AND paused = 0 ORDER BY session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.SessionID, &a.JID, &a.Proxy, &a.Paused); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
