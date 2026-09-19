package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"icloud-api/internal/domain"
)

const (
	aliasDeletionQuotaLimit  = 200
	aliasDeletionQuotaWindow = time.Hour + 5*time.Second
	// Reservations precede a bounded, individual Apple operation. Keep an
	// abandoned reservation for an extra hour beyond Apple's rolling window:
	// a process crash between sending the request and committing its accounting
	// must not immediately make its slot reusable. The finite lease also keeps
	// abandoned synchronous requests from consuming quota forever.
	aliasDeletionReservationLifetime = 2 * time.Hour
)

// These tables intentionally have no account, alias, or job foreign key. Apple
// limits belong to the verified principal and survive deleting local objects.
var quotaSchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS alias_deletion_quota_principals (
		principal TEXT NOT NULL PRIMARY KEY,
		cooldown_until BIGINT NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE IF NOT EXISTS alias_deletion_quota_reservations (
		principal TEXT NOT NULL REFERENCES alias_deletion_quota_principals(principal) ON DELETE CASCADE,
		reservation_id TEXT NOT NULL,
		state TEXT NOT NULL CHECK(state IN ('reserved', 'sent', 'committed')),
		accounted_at BIGINT NOT NULL,
		expires_at BIGINT NOT NULL,
		PRIMARY KEY(principal, reservation_id)
	)`,
	`CREATE INDEX IF NOT EXISTS alias_deletion_quota_expiry_idx
		ON alias_deletion_quota_reservations(principal, expires_at)`,
}

type aliasDeletionQuotaQueryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type aliasDeletionQuotaSnapshot struct {
	used           int
	earliestExpiry sql.NullInt64
	cooldownUntil  int64
}

func (v aliasDeletionQuotaSnapshot) result(now time.Time, ownsReservation bool) domain.AliasDeletionQuota {
	result := domain.AliasDeletionQuota{Used: v.used, Limit: aliasDeletionQuotaLimit}
	if !ownsReservation && v.used >= aliasDeletionQuotaLimit && v.earliestExpiry.Valid {
		result.RetryAt = timeFromTimestamp(v.earliestExpiry.Int64)
	}
	if v.cooldownUntil > timestamp(now) &&
		(result.RetryAt.IsZero() || v.cooldownUntil > timestamp(result.RetryAt)) {
		result.RetryAt = timeFromTimestamp(v.cooldownUntil)
	}
	return result
}

func validateAliasDeletionQuotaKey(principal, reservationID string, needsReservation bool) error {
	if strings.TrimSpace(principal) == "" {
		return errors.New("Apple deletion quota principal is required")
	}
	if needsReservation && strings.TrimSpace(reservationID) == "" {
		return errors.New("Apple deletion quota reservation ID is required")
	}
	return nil
}

// lockAliasDeletionQuotaPrincipal obtains a database write lock before any
// reads. PostgreSQL serializes this principal's requests on its row; SQLite
// avoids a deferred read transaction that could fail while upgrading to write.
func (s *Store) lockAliasDeletionQuotaPrincipal(ctx context.Context, tx *sql.Tx, principal string) error {
	_, err := s.txExecContext(ctx, tx, `
		INSERT INTO alias_deletion_quota_principals(principal, cooldown_until)
		VALUES(?, 0)
		ON CONFLICT(principal) DO UPDATE SET cooldown_until = alias_deletion_quota_principals.cooldown_until`, principal)
	if err != nil {
		return fmt.Errorf("lock Apple deletion quota: %w", err)
	}
	return nil
}

func (s *Store) readAliasDeletionQuota(
	ctx context.Context, queryer aliasDeletionQuotaQueryRower, principal string, now time.Time,
) (aliasDeletionQuotaSnapshot, error) {
	var snapshot aliasDeletionQuotaSnapshot
	err := queryer.QueryRowContext(ctx, s.rebind(`
		SELECT p.cooldown_until, COUNT(r.reservation_id), MIN(r.expires_at)
		FROM alias_deletion_quota_principals p
		LEFT JOIN alias_deletion_quota_reservations r
			ON r.principal = p.principal AND r.expires_at > ?
		WHERE p.principal = ?
		GROUP BY p.principal, p.cooldown_until`), timestamp(now), principal).Scan(
		&snapshot.cooldownUntil, &snapshot.used, &snapshot.earliestExpiry,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshot, nil
	}
	if err != nil {
		return snapshot, fmt.Errorf("read Apple deletion quota: %w", err)
	}
	return snapshot, nil
}

// ReserveAliasDeletionQuota atomically acquires one slot before deactivating an
// alias. A nonzero RetryAt means no new slot was acquired. An existing live
// reservation is idempotent, including when it already consumed the last slot.
// An expired abandoned reservation must obtain capacity again before reuse.
// Committed IDs remain recorded for idempotency; callers must use a new ID for
// each distinct deletion request and reconcile ambiguous results before retry.
// No transaction is held while the caller communicates with Apple.
func (s *Store) ReserveAliasDeletionQuota(
	ctx context.Context, principal, reservationID string, now time.Time,
) (domain.AliasDeletionQuota, error) {
	if err := validateAliasDeletionQuotaKey(principal, reservationID, true); err != nil {
		return domain.AliasDeletionQuota{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return domain.AliasDeletionQuota{}, fmt.Errorf("begin Apple deletion reservation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockAliasDeletionQuotaPrincipal(ctx, tx, principal); err != nil {
		return domain.AliasDeletionQuota{}, err
	}
	var state string
	var expiresAt int64
	err = s.txQueryRowContext(ctx, tx, `
		SELECT state, expires_at FROM alias_deletion_quota_reservations
		WHERE principal = ? AND reservation_id = ?`, principal, reservationID).Scan(&state, &expiresAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.AliasDeletionQuota{}, fmt.Errorf("read Apple deletion reservation: %w", err)
	}
	ownsReservation := err == nil && (state != "reserved" || expiresAt > timestamp(now))
	snapshot, err := s.readAliasDeletionQuota(ctx, tx, principal, now)
	if err != nil {
		return domain.AliasDeletionQuota{}, err
	}
	quota := snapshot.result(now, ownsReservation)
	if !quota.RetryAt.IsZero() || ownsReservation {
		if err := tx.Commit(); err != nil {
			return domain.AliasDeletionQuota{}, fmt.Errorf("commit Apple deletion reservation lookup: %w", err)
		}
		return quota, nil
	}
	if _, err := s.txExecContext(ctx, tx, `
		INSERT INTO alias_deletion_quota_reservations(principal, reservation_id, state, accounted_at, expires_at)
		VALUES(?, ?, 'reserved', ?, ?)
		ON CONFLICT(principal, reservation_id) DO UPDATE SET
			state = 'reserved', accounted_at = excluded.accounted_at, expires_at = excluded.expires_at`,
		principal, reservationID, timestamp(now), timestamp(now.Add(aliasDeletionReservationLifetime)),
	); err != nil {
		return domain.AliasDeletionQuota{}, fmt.Errorf("reserve Apple deletion quota: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.AliasDeletionQuota{}, fmt.Errorf("commit Apple deletion reservation: %w", err)
	}
	quota.Used++
	return quota, nil
}

// MarkAliasDeletionQuotaSent durably marks a mutation immediately before the
// network call. Its conservative two-hour lease covers the request deadline
// plus the rolling window if the process crashes before receiving a response.
// Call CommitAliasDeletionQuota with the response time to start the normal
// rolling window once the outcome (including a transport error) is available.
// A sent reservation can no longer be released as an unsent operation.
func (s *Store) MarkAliasDeletionQuotaSent(ctx context.Context, principal, reservationID string, now time.Time) error {
	if err := validateAliasDeletionQuotaKey(principal, reservationID, true); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin Apple deletion sent checkpoint: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockAliasDeletionQuotaPrincipal(ctx, tx, principal); err != nil {
		return err
	}
	var state string
	var accountedAt, expiresAt int64
	if err := s.txQueryRowContext(ctx, tx, `
		SELECT state, accounted_at, expires_at FROM alias_deletion_quota_reservations
		WHERE principal = ? AND reservation_id = ?`, principal, reservationID).Scan(&state, &accountedAt, &expiresAt); err != nil {
		return fmt.Errorf("read Apple deletion sent checkpoint: %w", err)
	}
	if state == "committed" {
		return errors.New("Apple deletion reservation has already been committed")
	}
	if expiresAt <= timestamp(now) {
		return errors.New("Apple deletion reservation expired before sending")
	}
	accountedAt = max(accountedAt, timestamp(now))
	expiresAt = max(expiresAt, timestamp(timeFromTimestamp(accountedAt).Add(aliasDeletionReservationLifetime)))
	if _, err := s.txExecContext(ctx, tx, `
		UPDATE alias_deletion_quota_reservations
		SET state = 'sent', accounted_at = ?, expires_at = ?
		WHERE principal = ? AND reservation_id = ?`, accountedAt, expiresAt, principal, reservationID,
	); err != nil {
		return fmt.Errorf("mark Apple deletion request sent: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Apple deletion sent checkpoint: %w", err)
	}
	return nil
}

// CommitAliasDeletionQuota keeps a sent or ambiguous deletion in the rolling
// window starting no earlier than now. Calling it again may extend accounting,
// but never moves a committed event backwards or creates a second charge.
func (s *Store) CommitAliasDeletionQuota(ctx context.Context, principal, reservationID string, now time.Time) error {
	if err := validateAliasDeletionQuotaKey(principal, reservationID, true); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin Apple deletion quota commit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockAliasDeletionQuotaPrincipal(ctx, tx, principal); err != nil {
		return err
	}
	var accountedAt int64
	if err := s.txQueryRowContext(ctx, tx, `
		SELECT accounted_at FROM alias_deletion_quota_reservations
		WHERE principal = ? AND reservation_id = ?`, principal, reservationID).Scan(&accountedAt); err != nil {
		return fmt.Errorf("read Apple deletion accounting: %w", err)
	}
	accountedAt = max(accountedAt, timestamp(now))
	if _, err := s.txExecContext(ctx, tx, `
		UPDATE alias_deletion_quota_reservations
		SET state = 'committed', accounted_at = ?, expires_at = ?
		WHERE principal = ? AND reservation_id = ?`,
		accountedAt, timestamp(timeFromTimestamp(accountedAt).Add(aliasDeletionQuotaWindow)), principal, reservationID,
	); err != nil {
		return fmt.Errorf("commit Apple deletion accounting: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Apple deletion quota transaction: %w", err)
	}
	return nil
}

// ReleaseAliasDeletionQuota releases only an unsent reservation. Committed
// requests continue counting even if their task is cancelled or alias removed.
func (s *Store) ReleaseAliasDeletionQuota(ctx context.Context, principal, reservationID string) error {
	if err := validateAliasDeletionQuotaKey(principal, reservationID, true); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin Apple deletion quota release: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockAliasDeletionQuotaPrincipal(ctx, tx, principal); err != nil {
		return err
	}
	_, err = s.txExecContext(ctx, tx, `DELETE FROM alias_deletion_quota_reservations
		WHERE principal = ? AND reservation_id = ? AND state = 'reserved'`, principal, reservationID)
	if err != nil {
		return fmt.Errorf("release Apple deletion quota: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Apple deletion quota release: %w", err)
	}
	return nil
}

// DeferAliasDeletionQuota persists the latest upstream cooldown for a principal.
func (s *Store) DeferAliasDeletionQuota(ctx context.Context, principal string, until time.Time) error {
	if err := validateAliasDeletionQuotaKey(principal, "", false); err != nil {
		return err
	}
	if until.IsZero() {
		return nil
	}
	_, err := s.execContext(ctx, `
		INSERT INTO alias_deletion_quota_principals(principal, cooldown_until) VALUES(?, ?)
		ON CONFLICT(principal) DO UPDATE SET cooldown_until = CASE
			WHEN excluded.cooldown_until > alias_deletion_quota_principals.cooldown_until
			THEN excluded.cooldown_until ELSE alias_deletion_quota_principals.cooldown_until END`, principal, timestamp(until))
	if err != nil {
		return fmt.Errorf("defer Apple deletion quota: %w", err)
	}
	return nil
}

// GetAliasDeletionQuota reads one consistent snapshot without creating rows or
// acquiring a write lock. This is suitable for queue progress polling.
func (s *Store) GetAliasDeletionQuota(ctx context.Context, principal string, now time.Time) (domain.AliasDeletionQuota, error) {
	if err := validateAliasDeletionQuotaKey(principal, "", false); err != nil {
		return domain.AliasDeletionQuota{}, err
	}
	snapshot, err := s.readAliasDeletionQuota(ctx, s.db, principal, now)
	if err != nil {
		return domain.AliasDeletionQuota{}, err
	}
	return snapshot.result(now, false), nil
}
