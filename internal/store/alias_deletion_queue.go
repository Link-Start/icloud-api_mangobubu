package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"icloud-api/internal/domain"
)

// The singleton is only held during short persistence transactions, never while
// talking to Apple. Its write lock serializes FIFO allocation and snapshot merges
// across processes in SQLite and PostgreSQL alike.
var aliasDeletionQueueSchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS alias_deletion_queue_sequence (
		id BIGINT PRIMARY KEY CHECK(id = 1), sequence BIGINT NOT NULL DEFAULT 0)`,
	`INSERT INTO alias_deletion_queue_sequence(id, sequence) VALUES(1, 0) ON CONFLICT(id) DO NOTHING`,
	createAliasDeletionJobClearancesTable,
	`CREATE TABLE IF NOT EXISTS alias_deletion_job_metadata (
		admin_id BIGINT NOT NULL, job_id TEXT NOT NULL, queue_version INTEGER NOT NULL DEFAULT 1,
		password_version BIGINT NOT NULL, username TEXT NOT NULL DEFAULT '', ip TEXT NOT NULL DEFAULT '',
		cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK(cancel_requested IN (0, 1)),
		PRIMARY KEY(admin_id, job_id),
		FOREIGN KEY(admin_id, job_id) REFERENCES alias_deletion_jobs(admin_id, id) ON DELETE CASCADE)`,
	`CREATE TABLE IF NOT EXISTS alias_deletion_work (
		id TEXT PRIMARY KEY, sequence BIGINT NOT NULL UNIQUE,
		account_id BIGINT NOT NULL, alias_id BIGINT NOT NULL, address TEXT NOT NULL,
		account_email TEXT NOT NULL DEFAULT '', apple_id TEXT NOT NULL DEFAULT '', apple_subject TEXT NOT NULL,
		status TEXT NOT NULL CHECK(status IN ('pending', 'running', 'waiting', 'paused', 'succeeded', 'failed', 'cancelled')),
		claim_token TEXT NOT NULL DEFAULT '', reconcile INTEGER NOT NULL DEFAULT 0 CHECK(reconcile IN (0, 1)),
		next_run_at BIGINT NOT NULL DEFAULT 0, wait_reason TEXT NOT NULL DEFAULT '', operation TEXT NOT NULL DEFAULT '',
		used INTEGER NOT NULL DEFAULT 0, quota_limit INTEGER NOT NULL DEFAULT 0, attempts INTEGER NOT NULL DEFAULT 0,
		http_status INTEGER NOT NULL DEFAULT 0, service_code TEXT NOT NULL DEFAULT '',
		deleted INTEGER NOT NULL DEFAULT 0 CHECK(deleted IN (0, 1)), code TEXT NOT NULL DEFAULT '', message TEXT NOT NULL DEFAULT '',
		local_retained INTEGER NOT NULL DEFAULT 0 CHECK(local_retained IN (0, 1)),
		created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS alias_deletion_work_active_address_idx
		ON alias_deletion_work(apple_subject, address) WHERE status IN ('pending', 'running', 'waiting', 'paused')`,
	`CREATE INDEX IF NOT EXISTS alias_deletion_work_subject_fifo_idx ON alias_deletion_work(apple_subject, sequence)`,
	`CREATE TABLE IF NOT EXISTS alias_deletion_job_work (
		admin_id BIGINT NOT NULL, job_id TEXT NOT NULL, alias_id BIGINT NOT NULL,
		work_id TEXT NOT NULL REFERENCES alias_deletion_work(id),
		active INTEGER NOT NULL DEFAULT 1 CHECK(active IN (0, 1)),
		audited INTEGER NOT NULL DEFAULT 0 CHECK(audited IN (0, 1)),
		PRIMARY KEY(admin_id, job_id, alias_id),
		FOREIGN KEY(admin_id, job_id) REFERENCES alias_deletion_jobs(admin_id, id) ON DELETE CASCADE)`,
	`CREATE INDEX IF NOT EXISTS alias_deletion_job_work_work_idx ON alias_deletion_job_work(work_id, active)`,
}

const deletionWorkColumns = `id, sequence, account_id, alias_id, address, account_email, apple_id, apple_subject,
	status, claim_token, reconcile, next_run_at, wait_reason, operation, used, quota_limit, attempts,
	http_status, service_code, deleted, code, message, local_retained, created_at, updated_at`

const deletionWorkActivePredicate = `status IN ('pending', 'running', 'waiting', 'paused')`

func (s *Store) beginDeletionQueueTx(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}
	if _, err := s.txExecContext(ctx, tx, `UPDATE alias_deletion_queue_sequence SET sequence = sequence WHERE id = 1`); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// EnqueueAliasDeletionJob atomically subscribes a job to existing active work or
// appends new work in request order. Repeating an owner/job ID returns its original
// snapshot and cannot change the request or consume another FIFO position.
func (s *Store) EnqueueAliasDeletionJob(ctx context.Context, job domain.AliasDeletionJob, targets []domain.AliasDeletionWork) (domain.AliasDeletionJob, error) {
	if job.Status == "" {
		job.Status = domain.AliasDeletionJobQueued
	}
	var err error
	job, _, err = normalizeAliasDeletionJob(job)
	if err != nil {
		return domain.AliasDeletionJob{}, err
	}
	tx, err := s.beginDeletionQueueTx(ctx)
	if err != nil {
		return domain.AliasDeletionJob{}, err
	}
	defer func() { _ = tx.Rollback() }()
	existing, err := s.getDeletionJobTx(ctx, tx, job.ID, job.AdminID)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return domain.AliasDeletionJob{}, err
	}
	var passwordVersion int64
	if err := s.txQueryRowContext(ctx, tx, `SELECT password_version FROM admins WHERE id = ?`, job.AdminID).Scan(&passwordVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.AliasDeletionJob{}, ErrCredentialsChanged
		}
		return domain.AliasDeletionJob{}, err
	}
	if job.AuthorizingPasswordVersion < 1 || passwordVersion != job.AuthorizingPasswordVersion {
		return domain.AliasDeletionJob{}, ErrCredentialsChanged
	}
	if err := s.pruneDeletionSubscriptionsTx(ctx, tx); err != nil {
		return domain.AliasDeletionJob{}, err
	}
	byAlias := make(map[int64]domain.AliasDeletionWork, len(targets))
	for _, target := range targets {
		if _, duplicate := byAlias[target.AliasID]; duplicate {
			return domain.AliasDeletionJob{}, errors.New("duplicate deletion target")
		}
		byAlias[target.AliasID] = target
	}
	now := s.now()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	job.QueueVersion = 1
	job.CancelRequested = false
	job.Username = truncate(job.Username, 128)
	job.IP = truncate(job.IP, 64)
	job.Status = domain.AliasDeletionJobQueued
	for index := range job.Items {
		item := &job.Items[index]
		if item.Done {
			continue
		}
		target, exists := byAlias[item.ID]
		if !exists || target.AccountID < 1 || strings.TrimSpace(target.AppleSubject) == "" || strings.TrimSpace(target.Address) == "" {
			return domain.AliasDeletionJob{}, fmt.Errorf("missing deletion target for alias %d", item.ID)
		}
		target.Address = strings.ToLower(strings.TrimSpace(sanitizePostgresText(target.Address)))
		target.AppleSubject = strings.TrimSpace(sanitizePostgresText(target.AppleSubject))
		work, err := scanAliasDeletionWork(s.txQueryRowContext(ctx, tx, `SELECT `+deletionWorkColumns+` FROM alias_deletion_work
			WHERE apple_subject = ? AND address = ? AND `+deletionWorkActivePredicate, target.AppleSubject, target.Address))
		if errors.Is(err, ErrNotFound) {
			work = target
			work.ID, err = newDeletionID()
			if err != nil {
				return domain.AliasDeletionJob{}, err
			}
			if err := s.txQueryRowContext(ctx, tx, `UPDATE alias_deletion_queue_sequence SET sequence = sequence + 1 WHERE id = 1 RETURNING sequence`).Scan(&work.Sequence); err != nil {
				return domain.AliasDeletionJob{}, err
			}
			work.Status = domain.AliasDeletionWorkPending
			work.ClaimToken = ""
			work.CreatedAt, work.UpdatedAt = now, now
			if err := s.insertDeletionWorkTx(ctx, tx, work); err != nil {
				return domain.AliasDeletionJob{}, err
			}
		} else if err != nil {
			return domain.AliasDeletionJob{}, err
		}
		item.AccountID = target.AccountID
		item.AccountEmail = work.AccountEmail
		item.AppleSubject = work.AppleSubject
		item.WorkID = work.ID
		applyDeletionWorkToItem(item, work)
	}
	job.Status = deletionJobStatus(job)
	_, itemsJSON, err := normalizeAliasDeletionJob(job)
	if err != nil {
		return domain.AliasDeletionJob{}, err
	}
	if _, err := s.txExecContext(ctx, tx, `INSERT INTO alias_deletion_jobs(id, admin_id, request_id, status, items_json, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`, job.ID, job.AdminID, job.RequestID, job.Status, itemsJSON, timestamp(job.CreatedAt), timestamp(job.UpdatedAt)); err != nil {
		return domain.AliasDeletionJob{}, aliasDeletionJobWriteError(err)
	}
	if _, err := s.txExecContext(ctx, tx, `INSERT INTO alias_deletion_job_metadata(admin_id, job_id, queue_version, password_version, username, ip, cancel_requested)
		VALUES(?, ?, 1, ?, ?, ?, 0)`, job.AdminID, job.ID, job.AuthorizingPasswordVersion, job.Username, job.IP); err != nil {
		return domain.AliasDeletionJob{}, err
	}
	for _, item := range job.Items {
		if item.WorkID == "" {
			continue
		}
		if _, err := s.txExecContext(ctx, tx, `INSERT INTO alias_deletion_job_work(admin_id, job_id, alias_id, work_id) VALUES(?, ?, ?, ?)`, job.AdminID, job.ID, item.ID, item.WorkID); err != nil {
			return domain.AliasDeletionJob{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.AliasDeletionJob{}, err
	}
	return job, nil
}

func (s *Store) ListAliasDeletionJobs(ctx context.Context, adminID int64, limit int) ([]domain.AliasDeletionJob, error) {
	if limit < 1 {
		limit = 20
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.queryContext(ctx, `SELECT `+aliasDeletionJobColumns+` FROM alias_deletion_jobs WHERE admin_id = ? AND
		(status IN ('queued', 'running') OR id IN (SELECT id FROM alias_deletion_jobs j WHERE j.admin_id = ?
		AND j.status IN ('completed', 'interrupted') AND NOT EXISTS(SELECT 1 FROM alias_deletion_job_clearances c
		WHERE c.admin_id = j.admin_id AND c.job_id = j.id)
		ORDER BY created_at DESC, id DESC LIMIT ?)) ORDER BY created_at DESC, id DESC`, adminID, adminID, limit)
	if err != nil {
		return nil, err
	}
	jobs := make([]domain.AliasDeletionJob, 0)
	for rows.Next() {
		job, err := scanAliasDeletionJob(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		jobs = append(jobs, job)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	for index := range jobs {
		if err := s.hydrateDeletionJobMetadata(ctx, &jobs[index]); err != nil {
			return nil, err
		}
	}
	return jobs, nil
}

// ListAliasDeletionQueueHeads returns at most one due item per Apple identity;
// a waiting/running head prevents later items of that identity from overtaking.
func (s *Store) ListAliasDeletionQueueHeads(ctx context.Context, now time.Time) ([]domain.AliasDeletionWork, error) {
	tx, err := s.beginDeletionQueueTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.pruneDeletionSubscriptionsTx(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := s.txQueryContext(ctx, tx, `SELECT `+deletionWorkColumns+` FROM alias_deletion_work w
		WHERE w.status IN ('pending', 'waiting', 'paused') AND w.next_run_at <= ?
		AND NOT EXISTS(SELECT 1 FROM alias_deletion_work earlier WHERE earlier.apple_subject = w.apple_subject
		AND earlier.sequence < w.sequence AND earlier.status IN ('pending', 'running', 'waiting', 'paused')) ORDER BY w.sequence`, timestamp(now))
	if err != nil {
		return nil, err
	}
	works, err := scanDeletionWorks(rows)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return works, nil
}

func (s *Store) ClaimAliasDeletionWork(ctx context.Context, id string, now time.Time) (domain.AliasDeletionWork, error) {
	tx, err := s.beginDeletionQueueTx(ctx)
	if err != nil {
		return domain.AliasDeletionWork{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.pruneDeletionSubscriptionsTx(ctx, tx); err != nil {
		return domain.AliasDeletionWork{}, err
	}
	work, err := scanAliasDeletionWork(s.txQueryRowContext(ctx, tx, `SELECT `+deletionWorkColumns+` FROM alias_deletion_work w
		WHERE id = ? AND status IN ('pending', 'waiting', 'paused') AND next_run_at <= ?
		AND NOT EXISTS(SELECT 1 FROM alias_deletion_work earlier WHERE earlier.apple_subject = w.apple_subject
		AND earlier.sequence < w.sequence AND earlier.status IN ('pending', 'running', 'waiting', 'paused'))`, id, timestamp(now)))
	if errors.Is(err, ErrNotFound) {
		if err := tx.Commit(); err != nil {
			return domain.AliasDeletionWork{}, err
		}
		return domain.AliasDeletionWork{}, ErrNotFound
	}
	if err != nil {
		return domain.AliasDeletionWork{}, err
	}
	work.ClaimToken, err = newDeletionID()
	if err != nil {
		return domain.AliasDeletionWork{}, err
	}
	work.Status = domain.AliasDeletionWorkRunning
	work.Attempts++
	work.UpdatedAt = now
	if err := s.updateDeletionWorkTx(ctx, tx, work); err != nil {
		return domain.AliasDeletionWork{}, err
	}
	if err := s.publishDeletionWorkTx(ctx, tx, work); err != nil {
		return domain.AliasDeletionWork{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.AliasDeletionWork{}, err
	}
	return work, nil
}

// SaveAliasDeletionWork fences late workers with a per-claim token and writes
// the result, every subscribed snapshot and each terminal audit in one commit.
func (s *Store) SaveAliasDeletionWork(ctx context.Context, work domain.AliasDeletionWork) error {
	switch work.Status {
	case domain.AliasDeletionWorkPending, domain.AliasDeletionWorkRunning, domain.AliasDeletionWorkWaiting, domain.AliasDeletionWorkPaused,
		domain.AliasDeletionWorkSucceeded, domain.AliasDeletionWorkFailed, domain.AliasDeletionWorkCancelled:
	default:
		return errors.New("invalid deletion work status")
	}
	tx, err := s.beginDeletionQueueTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanAliasDeletionWork(s.txQueryRowContext(ctx, tx, `SELECT `+deletionWorkColumns+` FROM alias_deletion_work WHERE id = ?`, work.ID))
	if err != nil {
		return err
	}
	if current.Status != domain.AliasDeletionWorkRunning || current.ClaimToken == "" || current.ClaimToken != work.ClaimToken {
		return ErrAliasDeletionJobConflict
	}
	if err := s.pruneDeletionSubscriptionsTx(ctx, tx); err != nil {
		return err
	}
	wanted, err := s.deletionWorkWantedTx(ctx, tx, work.ID)
	if err != nil {
		return err
	}
	// Identity and ordering belong to the enqueuer, never to a worker result.
	work.Sequence, work.AccountID, work.AliasID = current.Sequence, current.AccountID, current.AliasID
	work.Address, work.AccountEmail, work.AppleID, work.AppleSubject = current.Address, current.AccountEmail, current.AppleID, current.AppleSubject
	work.CreatedAt, work.Attempts = current.CreatedAt, current.Attempts
	if !wanted && !deletionWorkTerminal(work.Status) && !work.Reconcile {
		cancelDeletionWork(&work)
	}
	if work.Status != domain.AliasDeletionWorkRunning {
		work.ClaimToken = ""
	}
	work.UpdatedAt = s.now()
	work.Code = truncate(strings.TrimSpace(work.Code), 64)
	work.Message = truncate(strings.TrimSpace(work.Message), 1024)
	work.WaitReason = truncate(work.WaitReason, 128)
	work.Operation = truncate(work.Operation, 64)
	work.ServiceCode = truncate(work.ServiceCode, 128)
	if err := s.updateDeletionWorkTx(ctx, tx, work); err != nil {
		return err
	}
	if err := s.publishDeletionWorkTx(ctx, tx, work); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CancelAliasDeletionJob(ctx context.Context, id string, adminID int64) (domain.AliasDeletionJob, error) {
	tx, err := s.beginDeletionQueueTx(ctx)
	if err != nil {
		return domain.AliasDeletionJob{}, err
	}
	defer func() { _ = tx.Rollback() }()
	job, err := s.getDeletionJobTx(ctx, tx, id, adminID)
	if err != nil {
		return domain.AliasDeletionJob{}, err
	}
	if job.QueueVersion == 1 && job.Status != domain.AliasDeletionJobCompleted && job.Status != domain.AliasDeletionJobInterrupted {
		if err := s.cancelDeletionJobTx(ctx, tx, job, false); err != nil {
			return domain.AliasDeletionJob{}, err
		}
		if err := s.cancelUnwantedDeletionWorkTx(ctx, tx); err != nil {
			return domain.AliasDeletionJob{}, err
		}
		job, err = s.getDeletionJobTx(ctx, tx, id, adminID)
		if err != nil {
			return domain.AliasDeletionJob{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.AliasDeletionJob{}, err
	}
	return job, nil
}

func (s *Store) AliasDeletionWorkWanted(ctx context.Context, workID string) (bool, error) {
	tx, err := s.beginDeletionQueueTx(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.pruneDeletionSubscriptionsTx(ctx, tx); err != nil {
		return false, err
	}
	wanted, err := s.deletionWorkWantedTx(ctx, tx, workID)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return wanted, nil
}

// RecoverAliasDeletionQueue resumes versioned work only. An uncertain in-flight
// request is marked for remote reconciliation before any further mutation.
func (s *Store) RecoverAliasDeletionQueue(ctx context.Context) error {
	tx, err := s.beginDeletionQueueTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now()
	if _, err := s.txExecContext(ctx, tx, `UPDATE alias_deletion_jobs SET status = 'interrupted', updated_at = ?
		WHERE status IN ('queued', 'running') AND NOT EXISTS(SELECT 1 FROM alias_deletion_job_metadata m
		WHERE m.admin_id = alias_deletion_jobs.admin_id AND m.job_id = alias_deletion_jobs.id AND m.queue_version = 1)`, timestamp(now)); err != nil {
		return err
	}
	rows, err := s.txQueryContext(ctx, tx, `SELECT `+deletionWorkColumns+` FROM alias_deletion_work WHERE status = 'running'`)
	if err != nil {
		return err
	}
	works, err := scanDeletionWorks(rows)
	if err != nil {
		return err
	}
	for _, work := range works {
		work.Status = domain.AliasDeletionWorkPending
		work.ClaimToken = ""
		work.Reconcile = true
		work.NextRunAt = time.Time{}
		work.WaitReason = "reconcile"
		work.UpdatedAt = now
		if err := s.updateDeletionWorkTx(ctx, tx, work); err != nil {
			return err
		}
		if err := s.publishDeletionWorkTx(ctx, tx, work); err != nil {
			return err
		}
	}
	if err := s.pruneDeletionSubscriptionsTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) deletionWorkWantedTx(ctx context.Context, tx *sql.Tx, workID string) (bool, error) {
	var count int
	err := s.txQueryRowContext(ctx, tx, `SELECT COUNT(*) FROM alias_deletion_job_work r
		JOIN alias_deletion_job_metadata m ON m.admin_id = r.admin_id AND m.job_id = r.job_id
		JOIN admins a ON a.id = m.admin_id AND a.password_version = m.password_version
		WHERE r.work_id = ? AND r.active = 1 AND m.cancel_requested = 0`, workID).Scan(&count)
	return count > 0, err
}

func (s *Store) pruneDeletionSubscriptionsTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := s.txQueryContext(ctx, tx, `SELECT m.admin_id, m.job_id FROM alias_deletion_job_metadata m
		JOIN alias_deletion_jobs j ON j.admin_id = m.admin_id AND j.id = m.job_id
		LEFT JOIN admins a ON a.id = m.admin_id
		WHERE j.status IN ('queued', 'running') AND m.cancel_requested = 0 AND (a.id IS NULL OR a.password_version <> m.password_version)`)
	if err != nil {
		return err
	}
	type identity struct {
		adminID int64
		jobID   string
	}
	identities := make([]identity, 0)
	for rows.Next() {
		var id identity
		if err := rows.Scan(&id.adminID, &id.jobID); err != nil {
			_ = rows.Close()
			return err
		}
		identities = append(identities, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, id := range identities {
		job, err := s.getDeletionJobTx(ctx, tx, id.jobID, id.adminID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if job.Status == domain.AliasDeletionJobCompleted || job.Status == domain.AliasDeletionJobInterrupted {
			continue
		}
		if err := s.cancelDeletionJobTx(ctx, tx, job, true); err != nil {
			return err
		}
	}
	return s.cancelUnwantedDeletionWorkTx(ctx, tx)
}

func (s *Store) cancelDeletionJobTx(ctx context.Context, tx *sql.Tx, job domain.AliasDeletionJob, interrupted bool) error {
	job.CancelRequested = true
	if _, err := s.txExecContext(ctx, tx, `UPDATE alias_deletion_job_metadata SET cancel_requested = 1 WHERE admin_id = ? AND job_id = ?`, job.AdminID, job.ID); err != nil {
		return err
	}
	if _, err := s.txExecContext(ctx, tx, `UPDATE alias_deletion_job_work SET active = 0 WHERE admin_id = ? AND job_id = ?`, job.AdminID, job.ID); err != nil {
		return err
	}
	for index := range job.Items {
		item := &job.Items[index]
		if item.Done || item.WorkID == "" {
			continue
		}
		var status string
		var reconcile int
		if err := s.txQueryRowContext(ctx, tx, `SELECT status, reconcile FROM alias_deletion_work WHERE id = ?`, item.WorkID).Scan(&status, &reconcile); err != nil {
			return err
		}
		if status == domain.AliasDeletionWorkRunning || reconcile == 1 {
			continue
		}
		item.Done, item.Deleted, item.LocalRetained = true, false, true
		item.State = domain.AliasDeletionWorkCancelled
		item.Code = "BATCH_DELETE_CANCELLED"
		item.Message = "已取消剩余删除，本地记录保留"
		item.RetryAt, item.WaitReason = time.Time{}, ""
	}
	if interrupted {
		job.Status = domain.AliasDeletionJobInterrupted
	} else {
		job.Status = deletionJobStatus(job)
	}
	job.UpdatedAt = s.now()
	if err := s.writeDeletionSnapshotTx(ctx, tx, job); err != nil {
		return err
	}
	return s.auditDeletionItemsTx(ctx, tx, job)
}

func (s *Store) cancelUnwantedDeletionWorkTx(ctx context.Context, tx *sql.Tx) error {
	// Reconciliation survives the last subscriber disappearing: remote state may
	// already have changed and must be recorded before this record is terminal.
	rows, err := s.txQueryContext(ctx, tx, `SELECT `+deletionWorkColumns+` FROM alias_deletion_work w
		WHERE status IN ('pending', 'waiting', 'paused') AND reconcile = 0
		AND NOT EXISTS(SELECT 1 FROM alias_deletion_job_work r WHERE r.work_id = w.id AND r.active = 1)`)
	if err != nil {
		return err
	}
	works, err := scanDeletionWorks(rows)
	if err != nil {
		return err
	}
	for _, work := range works {
		cancelDeletionWork(&work)
		work.UpdatedAt = s.now()
		if err := s.updateDeletionWorkTx(ctx, tx, work); err != nil {
			return err
		}
		if err := s.publishDeletionWorkTx(ctx, tx, work); err != nil {
			return err
		}
	}
	return nil
}

func cancelDeletionWork(work *domain.AliasDeletionWork) {
	work.Status = domain.AliasDeletionWorkCancelled
	work.ClaimToken = ""
	work.NextRunAt = time.Time{}
	work.WaitReason = ""
	work.Deleted = false
	work.LocalRetained = true
	work.Code = "BATCH_DELETE_CANCELLED"
	work.Message = "已取消剩余删除，本地记录保留"
}

func (s *Store) publishDeletionWorkTx(ctx context.Context, tx *sql.Tx, work domain.AliasDeletionWork) error {
	if deletionWorkTerminal(work.Status) {
		if _, err := s.txExecContext(ctx, tx, `UPDATE alias_deletion_job_work SET active = 0 WHERE work_id = ?`, work.ID); err != nil {
			return err
		}
	}
	rows, err := s.txQueryContext(ctx, tx, `SELECT DISTINCT admin_id, job_id FROM alias_deletion_job_work WHERE work_id = ?`, work.ID)
	if err != nil {
		return err
	}
	type identity struct {
		adminID int64
		jobID   string
	}
	identities := make([]identity, 0)
	for rows.Next() {
		var id identity
		if err := rows.Scan(&id.adminID, &id.jobID); err != nil {
			_ = rows.Close()
			return err
		}
		identities = append(identities, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, id := range identities {
		job, err := s.getDeletionJobTx(ctx, tx, id.jobID, id.adminID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		for index := range job.Items {
			if job.Items[index].WorkID == work.ID && !job.Items[index].Done {
				applyDeletionWorkToItem(&job.Items[index], work)
			}
		}
		job.Status = deletionJobStatus(job)
		job.UpdatedAt = work.UpdatedAt
		if err := s.writeDeletionSnapshotTx(ctx, tx, job); err != nil {
			return err
		}
		if err := s.auditDeletionItemsTx(ctx, tx, job); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) writeDeletionSnapshotTx(ctx context.Context, tx *sql.Tx, job domain.AliasDeletionJob) error {
	_, itemsJSON, err := normalizeAliasDeletionJob(job)
	if err != nil {
		return err
	}
	_, err = s.txExecContext(ctx, tx, `UPDATE alias_deletion_jobs SET items_json = ?, status = ?, updated_at = ? WHERE admin_id = ? AND id = ?`,
		itemsJSON, job.Status, timestamp(job.UpdatedAt), job.AdminID, job.ID)
	return err
}

func (s *Store) auditDeletionItemsTx(ctx context.Context, tx *sql.Tx, job domain.AliasDeletionJob) error {
	// Query outstanding audit markers once. Rechecking every previously finished
	// item on every save would turn a 1,000-item job into 500,000 writes.
	rows, err := s.txQueryContext(ctx, tx, `SELECT alias_id FROM alias_deletion_job_work WHERE admin_id = ? AND job_id = ? AND audited = 0`, job.AdminID, job.ID)
	if err != nil {
		return err
	}
	unaudited := make(map[int64]bool)
	for rows.Next() {
		var aliasID int64
		if err := rows.Scan(&aliasID); err != nil {
			_ = rows.Close()
			return err
		}
		unaudited[aliasID] = true
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, item := range job.Items {
		if !item.Done || item.WorkID == "" || !unaudited[item.ID] {
			continue
		}
		result, err := s.txExecContext(ctx, tx, `UPDATE alias_deletion_job_work SET audited = 1 WHERE admin_id = ? AND job_id = ? AND alias_id = ? AND audited = 0`, job.AdminID, job.ID, item.ID)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			continue
		}
		resultLabel := "failure"
		if item.Deleted {
			resultLabel = "success"
		}
		detail, err := json.Marshal(map[string]any{"job_id": job.ID, "address": item.Address, "code": item.Code, "message": item.Message, "local_retained": item.LocalRetained})
		if err != nil {
			return err
		}
		if _, err := s.createAuditLogTx(ctx, tx, domain.AuditLog{
			AdminID: &job.AdminID, Username: job.Username, Action: "delete", ResourceType: "alias",
			ResourceID: strconv.FormatInt(item.ID, 10), Result: resultLabel, IP: job.IP, RequestID: job.RequestID,
			Detail: string(detail), CreatedAt: job.UpdatedAt,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) getDeletionJobTx(ctx context.Context, tx *sql.Tx, id string, adminID int64) (domain.AliasDeletionJob, error) {
	job, err := scanAliasDeletionJob(s.txQueryRowContext(ctx, tx, `SELECT `+aliasDeletionJobColumns+` FROM alias_deletion_jobs WHERE id = ? AND admin_id = ?`, id, adminID))
	if err != nil {
		return domain.AliasDeletionJob{}, err
	}
	err = scanDeletionJobMetadata(s.txQueryRowContext(ctx, tx, `SELECT queue_version, password_version, username, ip, cancel_requested FROM alias_deletion_job_metadata WHERE admin_id = ? AND job_id = ?`, adminID, id), &job)
	return job, err
}

func (s *Store) hydrateDeletionJobMetadata(ctx context.Context, job *domain.AliasDeletionJob) error {
	return scanDeletionJobMetadata(s.queryRowContext(ctx, `SELECT queue_version, password_version, username, ip, cancel_requested FROM alias_deletion_job_metadata WHERE admin_id = ? AND job_id = ?`, job.AdminID, job.ID), job)
}

func scanDeletionJobMetadata(scanner rowScanner, job *domain.AliasDeletionJob) error {
	var cancelled int
	err := scanner.Scan(&job.QueueVersion, &job.AuthorizingPasswordVersion, &job.Username, &job.IP, &cancelled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	job.CancelRequested = cancelled == 1
	return err
}

func (s *Store) insertDeletionWorkTx(ctx context.Context, tx *sql.Tx, work domain.AliasDeletionWork) error {
	_, err := s.txExecContext(ctx, tx, `INSERT INTO alias_deletion_work(`+deletionWorkColumns+`) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, deletionWorkValues(work)...)
	return err
}

func (s *Store) updateDeletionWorkTx(ctx context.Context, tx *sql.Tx, work domain.AliasDeletionWork) error {
	values := deletionWorkValues(work)
	args := append(append([]any{}, values[1:]...), work.ID)
	_, err := s.txExecContext(ctx, tx, `UPDATE alias_deletion_work SET sequence = ?, account_id = ?, alias_id = ?, address = ?, account_email = ?, apple_id = ?, apple_subject = ?,
		status = ?, claim_token = ?, reconcile = ?, next_run_at = ?, wait_reason = ?, operation = ?, used = ?, quota_limit = ?, attempts = ?,
		http_status = ?, service_code = ?, deleted = ?, code = ?, message = ?, local_retained = ?, created_at = ?, updated_at = ? WHERE id = ?`, args...)
	return err
}

func deletionWorkValues(work domain.AliasDeletionWork) []any {
	var nextRun int64
	if !work.NextRunAt.IsZero() {
		nextRun = timestamp(work.NextRunAt)
	}
	return []any{work.ID, work.Sequence, work.AccountID, work.AliasID, work.Address, work.AccountEmail, work.AppleID, work.AppleSubject,
		work.Status, work.ClaimToken, boolInt(work.Reconcile), nextRun, work.WaitReason, work.Operation, work.Used, work.Limit, work.Attempts,
		work.HTTPStatus, work.ServiceCode, boolInt(work.Deleted), work.Code, work.Message, boolInt(work.LocalRetained), timestamp(work.CreatedAt), timestamp(work.UpdatedAt)}
}

func scanAliasDeletionWork(scanner rowScanner) (domain.AliasDeletionWork, error) {
	var work domain.AliasDeletionWork
	var reconcile, deleted, retained int
	var nextRun, created, updated int64
	if err := scanner.Scan(&work.ID, &work.Sequence, &work.AccountID, &work.AliasID, &work.Address, &work.AccountEmail, &work.AppleID, &work.AppleSubject,
		&work.Status, &work.ClaimToken, &reconcile, &nextRun, &work.WaitReason, &work.Operation, &work.Used, &work.Limit, &work.Attempts,
		&work.HTTPStatus, &work.ServiceCode, &deleted, &work.Code, &work.Message, &retained, &created, &updated); err != nil {
		return domain.AliasDeletionWork{}, err
	}
	work.Reconcile, work.Deleted, work.LocalRetained = reconcile == 1, deleted == 1, retained == 1
	if nextRun != 0 {
		work.NextRunAt = timeFromTimestamp(nextRun)
	}
	work.CreatedAt, work.UpdatedAt = timeFromTimestamp(created), timeFromTimestamp(updated)
	return work, nil
}

func scanDeletionWorks(rows *sql.Rows) ([]domain.AliasDeletionWork, error) {
	defer rows.Close()
	works := make([]domain.AliasDeletionWork, 0)
	for rows.Next() {
		work, err := scanAliasDeletionWork(rows)
		if err != nil {
			return nil, err
		}
		works = append(works, work)
	}
	return works, rows.Err()
}

func applyDeletionWorkToItem(item *domain.AliasDeletionJobItem, work domain.AliasDeletionWork) {
	item.AccountEmail, item.AppleSubject = work.AccountEmail, work.AppleSubject
	item.State, item.RetryAt, item.WaitReason = work.Status, work.NextRunAt, work.WaitReason
	item.Used, item.Limit, item.Operation = work.Used, work.Limit, work.Operation
	item.HTTPStatus, item.ServiceCode = work.HTTPStatus, work.ServiceCode
	item.Done = deletionWorkTerminal(work.Status)
	item.Deleted, item.Code, item.Message, item.LocalRetained = work.Deleted, work.Code, work.Message, work.LocalRetained
}

func deletionWorkTerminal(status string) bool {
	return status == domain.AliasDeletionWorkSucceeded || status == domain.AliasDeletionWorkFailed || status == domain.AliasDeletionWorkCancelled
}

func deletionJobStatus(job domain.AliasDeletionJob) string {
	if job.Status == domain.AliasDeletionJobInterrupted {
		return job.Status
	}
	allDone, started := true, false
	for _, item := range job.Items {
		allDone = allDone && item.Done
		started = started || item.Done || (item.State != "" && item.State != domain.AliasDeletionWorkPending)
	}
	if allDone {
		return domain.AliasDeletionJobCompleted
	}
	if started {
		return domain.AliasDeletionJobRunning
	}
	return domain.AliasDeletionJobQueued
}

func newDeletionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
