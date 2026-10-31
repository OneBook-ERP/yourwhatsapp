package store

import (
	"context"
	"database/sql"
	"errors"
)

const (
	SendQueued = "queued"
	SendSent   = "sent"
	SendFailed = "failed"

	// ErrCodeSendFailed is the error code recorded for sends that did not complete.
	ErrCodeSendFailed = "send_failed"
)

type SendRecord struct {
	LocalID, SessionID, State, MessageID, ErrorCode string
}

const sendColumns = `local_message_id, session_id, state, COALESCE(message_id, ''), COALESCE(error_code, '')`

// ReserveSend atomically claims localID for sessionID. If a queued or sent row
// already exists (on any session) it is returned and nothing is written;
// otherwise the row is inserted or overwritten as queued and nil is returned.
func (s *Store) ReserveSend(ctx context.Context, localID, sessionID string) (*SendRecord, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var rec SendRecord
	err = tx.QueryRowContext(ctx, `SELECT `+sendColumns+` FROM sends WHERE local_message_id = ?`, localID).
		Scan(&rec.LocalID, &rec.SessionID, &rec.State, &rec.MessageID, &rec.ErrorCode)
	switch {
	case err == nil && rec.State != SendFailed:
		return &rec, nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO sends (local_message_id, session_id, state) VALUES (?, ?, 'queued')
		ON CONFLICT(local_message_id) DO UPDATE SET
			session_id = excluded.session_id, state = 'queued', message_id = NULL, error_code = NULL,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')`, localID, sessionID)
	if err != nil {
		return nil, err
	}
	return nil, tx.Commit()
}

func (s *Store) MarkSent(ctx context.Context, localID, messageID string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE sends SET state = 'sent', message_id = ?, error_code = NULL,
		updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE local_message_id = ?`, messageID, localID)
	return err
}

func (s *Store) MarkFailed(ctx context.Context, localID, code string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE sends SET state = 'failed', error_code = ?,
		updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE local_message_id = ?`, code, localID)
	return err
}

// FailQueued marks every queued row failed/send_failed (boot recovery: queued
// jobs live in memory and are lost on restart) and returns the affected rows.
func (s *Store) FailQueued(ctx context.Context) ([]SendRecord, error) {
	rows, err := s.DB.QueryContext(ctx, `UPDATE sends SET state = 'failed', error_code = ?,
		updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE state = 'queued'
		RETURNING `+sendColumns+` `, ErrCodeSendFailed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SendRecord
	for rows.Next() {
		var r SendRecord
		if err := rows.Scan(&r.LocalID, &r.SessionID, &r.State, &r.MessageID, &r.ErrorCode); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
