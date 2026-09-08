package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"icloud-api/internal/domain"

	"github.com/jackc/pgx/v5/pgconn"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrAliasDeletionJobConflict covers duplicate job IDs, an existing active job
// for the owner, and attempts to overwrite a terminal progress snapshot.
var ErrAliasDeletionJobConflict = errors.New("alias deletion job conflict")

const maxAliasDeletionJobItems = 1000

const aliasDeletionJobColumns = `id, admin_id, request_id, status, items_json, created_at, updated_at`

// CreateAliasDeletionJob records a bounded progress snapshot. Callers normally
// supply both timestamps; omitted timestamps default to the creation time.
// Retries never replace an existing job, even when its original owner retries.
func (s *Store) CreateAliasDeletionJob(ctx context.Context, job domain.AliasDeletionJob) error {
	if strings.TrimSpace(job.Status) == "" {
		job.Status = domain.AliasDeletionJobQueued
	}
	job, itemsJSON, err := normalizeAliasDeletionJob(job)
	if err != nil {
		return fmt.Errorf("create alias deletion job: %w", err)
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = s.now()
	}
	if job.UpdatedAt.IsZero() {
		job.UpdatedAt = job.CreatedAt
	}
	_, err = s.execContext(ctx, `
		INSERT INTO alias_deletion_jobs(id, admin_id, request_id, status, items_json, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.AdminID, job.RequestID, job.Status, itemsJSON,
		timestamp(job.CreatedAt), timestamp(job.UpdatedAt))
	if err != nil {
		return fmt.Errorf("create alias deletion job: %w", aliasDeletionJobWriteError(err))
	}
	return nil
}

// GetAliasDeletionJob deliberately treats another owner's job as missing.
func (s *Store) GetAliasDeletionJob(ctx context.Context, id string, adminID int64) (domain.AliasDeletionJob, error) {
	return scanAliasDeletionJob(s.queryRowContext(ctx,
		`SELECT `+aliasDeletionJobColumns+` FROM alias_deletion_jobs WHERE id = ? AND admin_id = ?`,
		strings.TrimSpace(id), adminID))
}

// GetLatestAliasDeletionJob orders by the existing Unix-nanosecond timestamp
// representation, with a deterministic job-ID tie break for equal timestamps.
func (s *Store) GetLatestAliasDeletionJob(ctx context.Context, adminID int64) (domain.AliasDeletionJob, error) {
	return scanAliasDeletionJob(s.queryRowContext(ctx,
		`SELECT `+aliasDeletionJobColumns+` FROM alias_deletion_jobs
		 WHERE admin_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`, adminID))
}

// GetActiveAliasDeletionJob is independent of the latest historical job so
// callers can check for in-flight work before inspecting possibly deleted IDs.
func (s *Store) GetActiveAliasDeletionJob(ctx context.Context, adminID int64) (domain.AliasDeletionJob, error) {
	return scanAliasDeletionJob(s.queryRowContext(ctx,
		`SELECT `+aliasDeletionJobColumns+` FROM alias_deletion_jobs
		 WHERE admin_id = ? AND status IN ('queued', 'running')
		 ORDER BY created_at DESC, id DESC LIMIT 1`, adminID))
}

// SaveAliasDeletionJob atomically replaces progress and optionally appends its
// audit event. Identity, request ID, and creation time are immutable. The
// active-state predicate prevents a late worker from overwriting completion or
// startup interruption; a rejected write also leaves the audit log untouched.
func (s *Store) SaveAliasDeletionJob(ctx context.Context, job domain.AliasDeletionJob, audit *domain.AuditLog) error {
	job, itemsJSON, err := normalizeAliasDeletionJob(job)
	if err != nil {
		return fmt.Errorf("save alias deletion job: %w", err)
	}
	if job.UpdatedAt.IsZero() {
		job.UpdatedAt = s.now()
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin alias deletion job save: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := s.txExecContext(ctx, tx, `
		UPDATE alias_deletion_jobs SET items_json = ?, status = ?, updated_at = ?
		WHERE id = ? AND admin_id = ? AND status IN ('queued', 'running')`,
		itemsJSON, job.Status, timestamp(job.UpdatedAt), job.ID, job.AdminID)
	if err != nil {
		return fmt.Errorf("save alias deletion job: %w", aliasDeletionJobWriteError(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read alias deletion job save result: %w", err)
	}
	if affected == 0 {
		var exists int
		if err := s.txQueryRowContext(ctx, tx,
			`SELECT 1 FROM alias_deletion_jobs WHERE id = ? AND admin_id = ?`,
			job.ID, job.AdminID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("check alias deletion job owner: %w", err)
		}
		return ErrAliasDeletionJobConflict
	}
	if audit != nil {
		if _, err := s.createAuditLogTx(ctx, tx, *audit); err != nil {
			return fmt.Errorf("save alias deletion job audit: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit alias deletion job save: %w", aliasDeletionJobWriteError(err))
	}
	return nil
}

// InterruptAliasDeletionJobs is an explicit startup operation, not a resumer.
// In particular, it neither rewrites item results nor claims unfinished work.
func (s *Store) InterruptAliasDeletionJobs(ctx context.Context) error {
	_, err := s.execContext(ctx, `
		UPDATE alias_deletion_jobs SET status = ?, updated_at = ?
		WHERE status IN ('queued', 'running')`,
		domain.AliasDeletionJobInterrupted, timestamp(s.now()))
	if err != nil {
		return fmt.Errorf("interrupt alias deletion jobs: %w", err)
	}
	return nil
}

func scanAliasDeletionJob(scanner rowScanner) (domain.AliasDeletionJob, error) {
	var job domain.AliasDeletionJob
	var itemsJSON string
	var createdAt, updatedAt int64
	if err := scanner.Scan(&job.ID, &job.AdminID, &job.RequestID, &job.Status,
		&itemsJSON, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.AliasDeletionJob{}, ErrNotFound
		}
		return domain.AliasDeletionJob{}, fmt.Errorf("scan alias deletion job: %w", err)
	}
	if err := json.Unmarshal([]byte(itemsJSON), &job.Items); err != nil {
		return domain.AliasDeletionJob{}, fmt.Errorf("decode alias deletion job items: %w", err)
	}
	if job.Items == nil {
		job.Items = []domain.AliasDeletionJobItem{}
	}
	job.CreatedAt = timeFromTimestamp(createdAt)
	job.UpdatedAt = timeFromTimestamp(updatedAt)
	return job, nil
}

// Normalize a private copy: Save must not mutate the worker's in-memory slice.
// Messages are bounded display text supplied by the caller, not raw Apple
// responses; credentials have no place in this persistence contract.
func normalizeAliasDeletionJob(job domain.AliasDeletionJob) (domain.AliasDeletionJob, string, error) {
	job.ID = strings.TrimSpace(job.ID)
	if job.ID == "" || !utf8.ValidString(job.ID) || strings.ContainsRune(job.ID, 0) || utf8.RuneCountInString(job.ID) > 128 {
		return job, "", errors.New("job ID must contain 1 to 128 valid text characters")
	}
	if job.AdminID < 1 {
		return job, "", errors.New("admin ID must be positive")
	}
	job.RequestID = truncate(strings.TrimSpace(job.RequestID), 128)
	job.Status = strings.TrimSpace(job.Status)
	switch job.Status {
	case domain.AliasDeletionJobQueued, domain.AliasDeletionJobRunning,
		domain.AliasDeletionJobCompleted, domain.AliasDeletionJobInterrupted:
	default:
		return job, "", errors.New("invalid alias deletion job status")
	}
	if len(job.Items) < 1 || len(job.Items) > maxAliasDeletionJobItems {
		return job, "", fmt.Errorf("job must contain 1 to %d items", maxAliasDeletionJobItems)
	}
	items := make([]domain.AliasDeletionJobItem, len(job.Items))
	seen := make(map[int64]struct{}, len(job.Items))
	for index, item := range job.Items {
		if item.ID < 1 {
			return job, "", fmt.Errorf("item %d: alias ID must be positive", index)
		}
		if _, exists := seen[item.ID]; exists {
			return job, "", fmt.Errorf("item %d: duplicate alias ID", index)
		}
		seen[item.ID] = struct{}{}
		item.Address = strings.TrimSpace(sanitizePostgresText(item.Address))
		if utf8.RuneCountInString(item.Address) > 320 {
			return job, "", fmt.Errorf("item %d: address exceeds 320 characters", index)
		}
		item.Code = truncate(strings.TrimSpace(item.Code), 64)
		item.Message = truncate(strings.TrimSpace(item.Message), 1024)
		items[index] = item
	}
	job.Items = items
	itemsJSON, err := marshalJSONList(job.Items)
	if err != nil {
		return job, "", fmt.Errorf("encode alias deletion job items: %w", err)
	}
	return job, itemsJSON, nil
}

// Use driver error codes, never message matching: foreign-key violations,
// storage failures and unrelated constraints are not retry/idempotency conflicts.
func aliasDeletionJobWriteError(err error) error {
	var postgresErr *pgconn.PgError
	if errors.As(err, &postgresErr) && postgresErr.Code == "23505" {
		return ErrAliasDeletionJobConflict
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() {
		case sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY, sqlite3.SQLITE_CONSTRAINT_UNIQUE:
			return ErrAliasDeletionJobConflict
		}
	}
	return err
}
