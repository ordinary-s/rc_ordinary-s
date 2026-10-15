package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/ordinary-s/rc_ordinary-s/internal/domain"
)

var (
	ErrNotFound        = errors.New("not found")
	ErrConflict        = errors.New("idempotency conflict")
	ErrNotEligible     = errors.New("notification is not eligible for manual resolution")
	ErrSignatureReplay = errors.New("signature nonce was already used")
)

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	db.SetMaxOpenConns(24)
	db.SetMaxIdleConns(8)
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// UseSignatureNonce atomically reserves a verified request nonce until expiry.
func (s *Store) UseSignatureNonce(ctx context.Context, keyID, nonce string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_signature_nonce (key_id, nonce, expires_at) VALUES (?, ?, ?)`, keyID, nonce, expiresAt.UTC())
	if err == nil {
		return nil
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		return ErrSignatureReplay
	}
	return fmt.Errorf("reserve signature nonce: %w", err)
}

// DeleteExpiredSignatureNonces performs a bounded cleanup using the expiry index.
func (s *Store) DeleteExpiredSignatureNonces(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM api_signature_nonce WHERE expires_at <= UTC_TIMESTAMP(6) ORDER BY expires_at LIMIT 1000`); err != nil {
		return fmt.Errorf("delete expired signature nonces: %w", err)
	}
	return nil
}

func (s *Store) GetDestination(ctx context.Context, id string) (domain.Destination, error) {
	var d domain.Destination
	err := s.db.QueryRowContext(ctx, `SELECT destination_id, target_url, fast_fail_enabled, COALESCE(updated_by, ''), COALESCE(update_reason, ''), updated_at
		FROM destination WHERE destination_id = ?`, id).
		Scan(&d.ID, &d.URL, &d.FastFailEnabled, &d.UpdatedBy, &d.UpdateReason, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Destination{}, ErrNotFound
	}
	if err != nil {
		return domain.Destination{}, fmt.Errorf("get destination: %w", err)
	}
	return d, nil
}

func (s *Store) SeedDestination(ctx context.Context, id, targetURL string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO destination (destination_id, target_url, fast_fail_enabled, updated_at)
		VALUES (?, ?, FALSE, UTC_TIMESTAMP(6))
		ON DUPLICATE KEY UPDATE target_url = VALUES(target_url)`, id, targetURL)
	if err != nil {
		return fmt.Errorf("seed destination: %w", err)
	}
	return nil
}

// Ingest atomically persists a source event's idempotency key and notification row.
func (s *Store) Ingest(ctx context.Context, n domain.Notification) (domain.Notification, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Notification{}, false, fmt.Errorf("begin ingest transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO notification
		(task_id, client_id, idempotency_key, source_event_id, request_fingerprint, destination_id, http_method, request_headers, request_body, trace_id, status, http_attempts, created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'PENDING', 0, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6), ?)`,
		n.ID, n.ClientID, n.IdempotencyKey, n.SourceEventID, n.Fingerprint, n.DestinationID, n.Method, string(n.HeadersJSON), n.Body, n.TraceID, n.ExpiresAt.UTC())
	if err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			_ = tx.Rollback()
			existing, lookupErr := s.findBySourceEvent(ctx, n.ClientID, n.SourceEventID)
			if errors.Is(lookupErr, ErrNotFound) {
				return domain.Notification{}, false, ErrConflict
			}
			if lookupErr != nil {
				return domain.Notification{}, false, lookupErr
			}
			if !equalBytes(existing.Fingerprint, n.Fingerprint) {
				return domain.Notification{}, false, ErrConflict
			}
			return existing, false, nil
		}
		return domain.Notification{}, false, fmt.Errorf("insert ingested notification: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Notification{}, false, fmt.Errorf("commit ingest transaction: %w", err)
	}
	n.Status = domain.StatusPending
	n.HTTPAttempts = 0
	n.CreatedAt = time.Now().UTC()
	return n, true, nil
}

func (s *Store) findBySourceEvent(ctx context.Context, clientID, eventID string) (domain.Notification, error) {
	var n domain.Notification
	err := s.db.QueryRowContext(ctx, `SELECT task_id, client_id, idempotency_key, request_fingerprint, destination_id, http_method,
		request_headers, request_body, trace_id, status, http_attempts, COALESCE(result_reason, ''), COALESCE(http_status, 0), created_at, expires_at, next_attempt_at,
		COALESCE(resolved_by, ''), COALESCE(resolution, ''), resolved_at
		FROM notification WHERE client_id = ? AND source_event_id = ?`, clientID, eventID).Scan(
		&n.ID, &n.ClientID, &n.IdempotencyKey, &n.Fingerprint, &n.DestinationID, &n.Method,
		&n.HeadersJSON, &n.Body, &n.TraceID, &n.Status, &n.HTTPAttempts, &n.ResultReason, &n.HTTPStatus,
		&n.CreatedAt, &n.ExpiresAt, &n.NextAttemptAt, &n.ResolvedBy, &n.Resolution, &n.ResolvedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Notification{}, ErrNotFound
	}
	if err != nil {
		return domain.Notification{}, fmt.Errorf("read source event notification: %w", err)
	}
	return n, nil
}

func (s *Store) GetNotification(ctx context.Context, id string) (domain.Notification, error) {
	var n domain.Notification
	err := s.db.QueryRowContext(ctx, `SELECT task_id, client_id, idempotency_key, request_fingerprint, destination_id, http_method,
		request_headers, request_body, trace_id, status, http_attempts, COALESCE(result_reason, ''), COALESCE(http_status, 0), created_at, expires_at, next_attempt_at,
		COALESCE(resolved_by, ''), COALESCE(resolution, ''), resolved_at
		FROM notification WHERE task_id = ?`, id).Scan(
		&n.ID, &n.ClientID, &n.IdempotencyKey, &n.Fingerprint, &n.DestinationID, &n.Method,
		&n.HeadersJSON, &n.Body, &n.TraceID, &n.Status, &n.HTTPAttempts, &n.ResultReason, &n.HTTPStatus,
		&n.CreatedAt, &n.ExpiresAt, &n.NextAttemptAt, &n.ResolvedBy, &n.Resolution, &n.ResolvedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Notification{}, ErrNotFound
	}
	if err != nil {
		return domain.Notification{}, fmt.Errorf("get notification: %w", err)
	}
	return n, nil
}

func (s *Store) ListNotifications(ctx context.Context, clientID, status string, limit, offset int) ([]domain.Notification, error) {
	query := `SELECT task_id, destination_id, trace_id, status, http_attempts, COALESCE(result_reason, ''), COALESCE(http_status, 0), created_at, expires_at
		FROM notification WHERE client_id = ?`
	args := []any{clientID}
	if status != "" {
		query += " AND status = ?"
		args = append(args, status)
	}
	query += " ORDER BY created_at DESC, task_id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}
	defer rows.Close()
	result := make([]domain.Notification, 0)
	for rows.Next() {
		var n domain.Notification
		if err := rows.Scan(&n.ID, &n.DestinationID, &n.TraceID, &n.Status, &n.HTTPAttempts, &n.ResultReason, &n.HTTPStatus, &n.CreatedAt, &n.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan notification: %w", err)
		}
		result = append(result, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate notifications: %w", err)
	}
	return result, nil
}

func (s *Store) ListAttempts(ctx context.Context, taskID string) ([]domain.Attempt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT attempt_no, state, COALESCE(http_status, 0), COALESCE(error_summary, ''), started_at, finished_at
		FROM delivery_attempt WHERE task_id = ? ORDER BY attempt_no ASC LIMIT 3`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	defer rows.Close()
	result := make([]domain.Attempt, 0, 3)
	for rows.Next() {
		var a domain.Attempt
		if err := rows.Scan(&a.Number, &a.State, &a.HTTPStatus, &a.Reason, &a.StartedAt, &a.FinishedAt); err != nil {
			return nil, fmt.Errorf("scan attempt: %w", err)
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

func (s *Store) StartAttempt(ctx context.Context, taskID string, attemptNo int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin attempt transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE notification SET status = 'PROCESSING', http_attempts = ?, next_attempt_at = NULL
		WHERE task_id = ? AND status IN ('PENDING', 'PROCESSING') AND http_attempts = ?`, attemptNo, taskID, attemptNo-1)
	if err != nil {
		return fmt.Errorf("reserve HTTP attempt: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read attempt reservation count: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("reserve attempt %d: task state or attempt count changed", attemptNo)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO delivery_attempt (task_id, attempt_no, state, started_at)
		VALUES (?, ?, 'STARTED', UTC_TIMESTAMP(6))`, taskID, attemptNo)
	if err != nil {
		return fmt.Errorf("insert delivery attempt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit attempt reservation: %w", err)
	}
	return nil
}

func (s *Store) CompleteAttempt(ctx context.Context, taskID string, attemptNo, httpStatus int, attemptState, nextTaskStatus, reason string, nextAttemptAt *time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin attempt result transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE delivery_attempt SET state = ?, http_status = ?, error_summary = ?, finished_at = UTC_TIMESTAMP(6)
		WHERE task_id = ? AND attempt_no = ? AND state = 'STARTED'`, attemptState, httpStatus, reason, taskID, attemptNo)
	if err != nil {
		return fmt.Errorf("save attempt result: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		if err != nil {
			return fmt.Errorf("read attempt result count: %w", err)
		}
		var storedAttemptState, storedAttemptReason, storedTaskStatus, storedTaskReason string
		var storedAttemptHTTP, storedTaskHTTP sql.NullInt64
		var storedNextAttempt sql.NullTime
		var storedAttempts int
		if err := tx.QueryRowContext(ctx, `SELECT a.state, a.http_status, a.error_summary,
			n.status, n.http_status, n.result_reason, n.http_attempts, n.next_attempt_at
			FROM delivery_attempt a JOIN notification n ON n.task_id = a.task_id
			WHERE a.task_id = ? AND a.attempt_no = ?`, taskID, attemptNo).Scan(
			&storedAttemptState, &storedAttemptHTTP, &storedAttemptReason,
			&storedTaskStatus, &storedTaskHTTP, &storedTaskReason, &storedAttempts, &storedNextAttempt,
		); err != nil {
			return fmt.Errorf("read previously saved attempt result: %w", err)
		}
		dueMatches := nextAttemptAt == nil && !storedNextAttempt.Valid || nextAttemptAt != nil && storedNextAttempt.Valid && storedNextAttempt.Time.Equal(*nextAttemptAt)
		if storedAttemptState == attemptState && storedAttemptHTTP.Valid && int(storedAttemptHTTP.Int64) == httpStatus &&
			storedAttemptReason == reason && storedTaskStatus == nextTaskStatus && storedTaskHTTP.Valid &&
			int(storedTaskHTTP.Int64) == httpStatus && storedTaskReason == reason && storedAttempts == attemptNo && dueMatches {
			return nil
		}
		return fmt.Errorf("attempt %d result differs from the previously saved result", attemptNo)
	}
	result, err = tx.ExecContext(ctx, `UPDATE notification SET status = ?, http_status = ?, result_reason = ?, next_attempt_at = ?, updated_at = UTC_TIMESTAMP(6)
		WHERE task_id = ? AND status = 'PROCESSING' AND http_attempts = ?`, nextTaskStatus, httpStatus, reason, nextAttemptAt, taskID, attemptNo)
	if err != nil {
		return fmt.Errorf("save notification result: %w", err)
	}
	count, err = result.RowsAffected()
	if err != nil || count != 1 {
		if err != nil {
			return fmt.Errorf("read notification result count: %w", err)
		}
		return fmt.Errorf("notification state changed before attempt result")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit attempt result: %w", err)
	}
	return nil
}

func (s *Store) FinishWithoutAttempt(ctx context.Context, taskID, status, reason string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE notification SET status = ?, result_reason = ?, next_attempt_at = NULL, updated_at = UTC_TIMESTAMP(6)
		WHERE task_id = ? AND status IN ('PENDING', 'PROCESSING')`, status, reason, taskID)
	if err != nil {
		return fmt.Errorf("save terminal notification result: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read terminal update count: %w", err)
	}
	if count == 0 {
		n, err := s.GetNotification(ctx, taskID)
		if err != nil {
			return err
		}
		if domain.IsTerminal(n.Status) {
			return nil
		}
		return fmt.Errorf("notification %s is not eligible for terminal update", taskID)
	}
	return nil
}

func (s *Store) MarkInFlightUncertain(ctx context.Context, taskID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin uncertain recovery: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `UPDATE delivery_attempt SET state = 'UNKNOWN', error_summary = 'PROCESS_INTERRUPTED_RESULT_UNKNOWN',
		finished_at = UTC_TIMESTAMP(6) WHERE task_id = ? AND state = 'STARTED'`, taskID)
	if err != nil {
		return fmt.Errorf("mark interrupted attempt unknown: %w", err)
	}
	_, err = tx.ExecContext(ctx, `UPDATE notification SET status = 'UNCERTAIN', result_reason = 'PROCESS_INTERRUPTED_RESULT_UNKNOWN',
		updated_at = UTC_TIMESTAMP(6) WHERE task_id = ? AND status = 'PROCESSING'`, taskID)
	if err != nil {
		return fmt.Errorf("mark interrupted notification uncertain: %w", err)
	}
	return tx.Commit()
}

func (s *Store) UpdateFastFail(ctx context.Context, id string, enabled bool, actor, reason string) (domain.Destination, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE destination SET fast_fail_enabled = ?, updated_by = ?, update_reason = ?, updated_at = UTC_TIMESTAMP(6)
		WHERE destination_id = ?`, enabled, actor, reason, id)
	if err != nil {
		return domain.Destination{}, fmt.Errorf("update destination fast-fail: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return domain.Destination{}, fmt.Errorf("read fast-fail update count: %w", err)
	}
	if count != 1 {
		return domain.Destination{}, ErrNotFound
	}
	return s.GetDestination(ctx, id)
}

func (s *Store) Resolve(ctx context.Context, taskID, actor, resolution string) (domain.Notification, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE notification SET resolved_by = ?, resolution = ?, resolved_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6)
		WHERE task_id = ? AND status IN ('FAILED', 'FAST_FAILED', 'EXPIRED', 'UNCERTAIN')`, actor, resolution, taskID)
	if err != nil {
		return domain.Notification{}, fmt.Errorf("save manual resolution: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return domain.Notification{}, fmt.Errorf("read resolution update count: %w", err)
	}
	if count != 1 {
		n, getErr := s.GetNotification(ctx, taskID)
		if getErr != nil {
			return domain.Notification{}, getErr
		}
		if n.Status == domain.StatusFailed || n.Status == domain.StatusFastFailed || n.Status == domain.StatusExpired || n.Status == domain.StatusUncertain {
			return n, nil
		}
		return n, ErrNotEligible
	}
	return s.GetNotification(ctx, taskID)
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
