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
	// ErrCodeSendUnconfirmed is recorded when the send errored after the
	// message went out: WhatsApp may have delivered it.
	ErrCodeSendUnconfirmed = "send_unconfirmed"
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

// GetSend returns the send record for localID, or nil when there is none.
func (s *Store) GetSend(ctx context.Context, localID string) (*SendRecord, error) {
	var rec SendRecord
	err := s.DB.QueryRowContext(ctx, `SELECT `+sendColumns+` FROM sends WHERE local_message_id = ?`, localID).
		Scan(&rec.LocalID, &rec.SessionID, &rec.State, &rec.MessageID, &rec.ErrorCode)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// MarkSent records a successful send. It only touches the row while it is
// still queued for sessionID, so a late result from a session that lost the
// reservation cannot overwrite another session's claim. It reports whether a
// row was updated.
func (s *Store) MarkSent(ctx context.Context, localID, sessionID, messageID string) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE sends SET state = 'sent', message_id = ?, error_code = NULL,
		updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE local_message_id = ? AND session_id = ? AND state = 'queued'`, messageID, localID, sessionID)
	return affected(res, err)
}

// MarkFailed records a failed send, with the same guard as MarkSent.
func (s *Store) MarkFailed(ctx context.Context, localID, sessionID, code string) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE sends SET state = 'failed', error_code = ?,
		updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE local_message_id = ? AND session_id = ? AND state = 'queued'`, code, localID, sessionID)
	return affected(res, err)
}

// MarkUnconfirmed records a send whose outcome is unknown as failed with
// send_unconfirmed, keeping its message id so later receipts can still match.
// Same guard as MarkSent.
func (s *Store) MarkUnconfirmed(ctx context.Context, localID, sessionID, messageID string) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE sends SET state = 'failed', message_id = ?, error_code = ?,
		updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE local_message_id = ? AND session_id = ? AND state = 'queued'`,
		messageID, ErrCodeSendUnconfirmed, localID, sessionID)
	return affected(res, err)
}

func affected(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ListQueued returns every queued row, oldest first (boot recovery: queued
// jobs live in memory and are lost on restart).
func (s *Store) ListQueued(ctx context.Context) ([]SendRecord, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+sendColumns+` FROM sends WHERE state = 'queued'
		ORDER BY created_at, local_message_id`)
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
