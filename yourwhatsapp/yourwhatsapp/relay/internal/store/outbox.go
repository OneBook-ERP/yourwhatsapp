package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// timeLayout is the on-disk format of outbox timestamps (matches the column defaults).
const timeLayout = "2006-01-02T15:04:05.000Z"

type OutboxRow struct {
	ID                       int64
	Method                   string
	Payload                  []byte
	Attempts                 int
	NextAttemptAt, CreatedAt time.Time
	LastError                string
}

const outboxColumns = `id, method, payload, attempts, next_attempt_at, COALESCE(last_error, ''), created_at`

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

type rowScanner interface{ Scan(dest ...any) error }

func scanOutbox(sc rowScanner) (OutboxRow, error) {
	var r OutboxRow
	var payload, next, created string
	if err := sc.Scan(&r.ID, &r.Method, &payload, &r.Attempts, &next, &r.LastError, &created); err != nil {
		return OutboxRow{}, err
	}
	r.Payload = []byte(payload)
	var err error
	if r.NextAttemptAt, err = time.Parse(timeLayout, next); err != nil {
		return OutboxRow{}, err
	}
	if r.CreatedAt, err = time.Parse(timeLayout, created); err != nil {
		return OutboxRow{}, err
	}
	return r, nil
}

// EnqueueCallback JSON-marshals payload and appends it to the outbox, due immediately.
func (s *Store) EnqueueCallback(ctx context.Context, method string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO outbox (method, payload) VALUES (?, ?)`, method, string(b))
	return err
}

// HeadCallback returns the lowest-id outbox row, or nil when the outbox is empty.
func (s *Store) HeadCallback(ctx context.Context) (*OutboxRow, error) {
	r, err := scanOutbox(s.DB.QueryRowContext(ctx, `SELECT `+outboxColumns+` FROM outbox ORDER BY id LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) DeleteCallback(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM outbox WHERE id = ?`, id)
	return err
}

// RetryCallback increments attempts and schedules the next attempt.
func (s *Store) RetryCallback(ctx context.Context, id int64, next time.Time, errMsg string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE outbox SET attempts = attempts + 1, next_attempt_at = ?, last_error = ? WHERE id = ?`,
		formatTime(next), errMsg, id)
	return err
}

// KillCallback moves the row to outbox_dead, recording errMsg.
func (s *Store) KillCallback(ctx context.Context, id int64, errMsg string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_dead (id, method, payload, attempts, next_attempt_at, last_error, created_at)
		SELECT id, method, payload, attempts, next_attempt_at, ?, created_at FROM outbox WHERE id = ?`, errMsg, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ListDead returns up to limit dead-lettered rows, oldest first.
func (s *Store) ListDead(ctx context.Context, limit int) ([]OutboxRow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+outboxColumns+` FROM outbox_dead ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxRow
	for rows.Next() {
		r, err := scanOutbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// OutboxCounts returns the callbacks waiting to be delivered and the dead-lettered ones.
func (s *Store) OutboxCounts(ctx context.Context) (pending, dead int, err error) {
	err = s.DB.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM outbox), (SELECT COUNT(*) FROM outbox_dead)`).Scan(&pending, &dead)
	return pending, dead, err
}
