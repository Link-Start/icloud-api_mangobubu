package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"icloud-api/internal/domain"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestAliasDeletionJobOwnerIsolationAndLatest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openAliasDeletionJobTestStore(t, ":memory:")
	owner := createAliasDeletionJobTestAdmin(t, s, "owner")
	other := createAliasDeletionJobTestAdmin(t, s, "other")
	for _, read := range []func() (domain.AliasDeletionJob, error){
		func() (domain.AliasDeletionJob, error) { return s.GetAliasDeletionJob(ctx, "missing", owner.ID) },
		func() (domain.AliasDeletionJob, error) { return s.GetLatestAliasDeletionJob(ctx, owner.ID) },
		func() (domain.AliasDeletionJob, error) { return s.GetActiveAliasDeletionJob(ctx, owner.ID) },
	} {
		if _, err := read(); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing job error = %v, want ErrNotFound", err)
		}
	}

	older := aliasDeletionJobTestFixture("z-older", owner.ID)
	older.Status = domain.AliasDeletionJobCompleted
	newer := aliasDeletionJobTestFixture("a-newer", owner.ID)
	newer.Status = domain.AliasDeletionJobInterrupted
	newer.CreatedAt = older.CreatedAt.Add(time.Nanosecond)
	newer.UpdatedAt = newer.CreatedAt
	for _, job := range []domain.AliasDeletionJob{older, newer} {
		mustCreateAliasDeletionJob(t, s, job)
	}
	got, err := s.GetLatestAliasDeletionJob(ctx, owner.ID)
	if err != nil || got.ID != newer.ID || !got.CreatedAt.Equal(newer.CreatedAt) {
		t.Fatalf("nanosecond-ordered latest = %#v, err = %v", got, err)
	}
	tied := newer
	tied.ID = "b-newer"
	mustCreateAliasDeletionJob(t, s, tied)
	otherJob := aliasDeletionJobTestFixture("other-job", other.ID)
	otherJob.CreatedAt = newer.CreatedAt.Add(time.Hour)
	mustCreateAliasDeletionJob(t, s, otherJob)
	for range 3 {
		got, err = s.GetLatestAliasDeletionJob(ctx, owner.ID)
		if err != nil || got.ID != tied.ID {
			t.Fatalf("owner-isolated tied latest = %#v, err = %v", got, err)
		}
	}

	for _, id := range []string{otherJob.ID, older.ID} {
		adminID := owner.ID
		if id == older.ID {
			adminID = other.ID
		}
		if _, err := s.GetAliasDeletionJob(ctx, id, adminID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-owner get %q error = %v", id, err)
		}
		wrongOwner := aliasDeletionJobTestFixture(id, adminID)
		if err := s.SaveAliasDeletionJob(ctx, wrongOwner, &domain.AuditLog{Action: "wrong-owner"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-owner save %q error = %v", id, err)
		}
	}
	missing := aliasDeletionJobTestFixture("missing", owner.ID)
	if err := s.SaveAliasDeletionJob(ctx, missing, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing save error = %v", err)
	}
	if _, err := s.GetActiveAliasDeletionJob(ctx, owner.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("terminal-only owner's active lookup error = %v", err)
	}
	assertAliasDeletionJobEqual(t, mustGetAliasDeletionJob(t, s, otherJob), otherJob)
	assertAliasDeletionJobAuditCount(t, s, 0)
}

func TestAliasDeletionJobConflictAndActiveLimit(t *testing.T) {
	t.Parallel()
	for _, initial := range []string{domain.AliasDeletionJobQueued, domain.AliasDeletionJobRunning} {
		for _, terminal := range []string{domain.AliasDeletionJobCompleted, domain.AliasDeletionJobInterrupted} {
			t.Run(initial+"-to-"+terminal, func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				s := openAliasDeletionJobTestStore(t, ":memory:")
				owner := createAliasDeletionJobTestAdmin(t, s, "owner")
				other := createAliasDeletionJobTestAdmin(t, s, "other")
				job := aliasDeletionJobTestFixture("original", owner.ID)
				job.Status = initial
				mustCreateAliasDeletionJob(t, s, job)
				for _, conflicting := range []domain.AliasDeletionJob{
					job,
					aliasDeletionJobTestFixture("second-active", owner.ID),
				} {
					if err := s.CreateAliasDeletionJob(ctx, conflicting); !errors.Is(err, ErrAliasDeletionJobConflict) {
						t.Fatalf("conflicting create error = %v", err)
					}
				}
				// Operation IDs are scoped to their administrator, including
				// while both owners' jobs are active.
				otherJob := aliasDeletionJobTestFixture(job.ID, other.ID)
				mustCreateAliasDeletionJob(t, s, otherJob)
				got, err := s.GetActiveAliasDeletionJob(ctx, owner.ID)
				if err != nil || got.ID != job.ID {
					t.Fatalf("active lookup = %#v, err = %v", got, err)
				}
				job.Status = terminal
				job.UpdatedAt = job.UpdatedAt.Add(time.Second)
				if err := s.SaveAliasDeletionJob(ctx, job, nil); err != nil {
					t.Fatalf("save terminal snapshot: %v", err)
				}
				if _, err := s.GetActiveAliasDeletionJob(ctx, owner.ID); !errors.Is(err, ErrNotFound) {
					t.Fatalf("active lookup after terminal save error = %v", err)
				}
				if err := s.CreateAliasDeletionJob(ctx, job); !errors.Is(err, ErrAliasDeletionJobConflict) {
					t.Fatalf("duplicate terminal job ID error = %v", err)
				}
				next := aliasDeletionJobTestFixture("next-active", owner.ID)
				mustCreateAliasDeletionJob(t, s, next)
				for _, staleStatus := range []string{
					domain.AliasDeletionJobQueued, domain.AliasDeletionJobRunning,
					domain.AliasDeletionJobCompleted, domain.AliasDeletionJobInterrupted,
				} {
					stale := job
					stale.Status = staleStatus
					stale.UpdatedAt = stale.UpdatedAt.Add(time.Hour)
					if err := s.SaveAliasDeletionJob(ctx, stale, &domain.AuditLog{Action: "stale-save"}); !errors.Is(err, ErrAliasDeletionJobConflict) {
						t.Fatalf("overwrite terminal with %q error = %v", staleStatus, err)
					}
				}
				assertAliasDeletionJobEqual(t, mustGetAliasDeletionJob(t, s, job), job)
				assertAliasDeletionJobEqual(t, mustGetAliasDeletionJob(t, s, otherJob), otherJob)
				assertAliasDeletionJobAuditCount(t, s, 0)
			})
		}
	}
}

func TestAliasDeletionJobConcurrentCreateAllowsOneActiveOwner(t *testing.T) {
	t.Parallel()
	s := openAliasDeletionJobTestStore(t, filepath.Join(t.TempDir(), "concurrent.db"))
	owner := createAliasDeletionJobTestAdmin(t, s, "owner")
	const attempts = 8
	results := make(chan error, attempts)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for index := range attempts {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			job := aliasDeletionJobTestFixture(fmt.Sprintf("job-%d", index), owner.ID)
			results <- s.CreateAliasDeletionJob(context.Background(), job)
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	created := 0
	for err := range results {
		if err == nil {
			created++
		} else if !errors.Is(err, ErrAliasDeletionJobConflict) {
			t.Fatalf("concurrent create error = %v", err)
		}
	}
	if created != 1 {
		t.Fatalf("successful concurrent creates = %d, want 1", created)
	}
}

func TestAliasDeletionJobRestartPreservesPartialResults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restart.db")
	s := openAliasDeletionJobTestStore(t, path)
	owner := createAliasDeletionJobTestAdmin(t, s, "owner")
	other := createAliasDeletionJobTestAdmin(t, s, "other")
	job := aliasDeletionJobTestFixture("partial", owner.ID)
	mustCreateAliasDeletionJob(t, s, job)
	job.Status = domain.AliasDeletionJobRunning
	job.Items[0].Done = true
	job.Items[0].Deleted = true
	job.Items[0].Code = "DELETED"
	job.Items[0].Message = "Deleted from Apple and locally"
	job.Items[1].Done = true
	job.Items[1].LocalRetained = true
	job.Items[1].Code = "APPLE_DELETE_FAILED"
	job.Items[1].Message = "Local alias retained"
	job.UpdatedAt = job.UpdatedAt.Add(time.Second)
	if err := s.SaveAliasDeletionJob(ctx, job, nil); err != nil {
		t.Fatalf("save partial result: %v", err)
	}
	queued := aliasDeletionJobTestFixture("queued", other.ID)
	mustCreateAliasDeletionJob(t, s, queued)
	completed := aliasDeletionJobTestFixture("completed", owner.ID)
	completed.Status = domain.AliasDeletionJobCompleted
	completed.CreatedAt = job.CreatedAt.Add(time.Minute)
	completed.UpdatedAt = completed.CreatedAt
	mustCreateAliasDeletionJob(t, s, completed)
	interrupted := aliasDeletionJobTestFixture("already-interrupted", other.ID)
	interrupted.Status = domain.AliasDeletionJobInterrupted
	mustCreateAliasDeletionJob(t, s, interrupted)
	var originalJSON string
	if err := s.db.QueryRowContext(ctx, `SELECT items_json FROM alias_deletion_jobs WHERE id = ?`, job.ID).Scan(&originalJSON); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close before restart: %v", err)
	}

	restarted := openAliasDeletionJobTestStore(t, path)
	// Migration converges schema only. The startup caller explicitly interrupts
	// unfinished jobs before accepting new work.
	active, err := restarted.GetActiveAliasDeletionJob(ctx, owner.ID)
	if err != nil || active.ID != job.ID || active.Status != domain.AliasDeletionJobRunning {
		t.Fatalf("active lookup independent of newer history = %#v, err = %v", active, err)
	}
	interruptAt := job.UpdatedAt.Add(time.Hour)
	restarted.now = func() time.Time { return interruptAt }
	if err := restarted.InterruptAliasDeletionJobs(ctx); err != nil {
		t.Fatalf("interrupt after restart: %v", err)
	}
	for _, previous := range []domain.AliasDeletionJob{job, queued} {
		want := previous
		want.Status = domain.AliasDeletionJobInterrupted
		want.UpdatedAt = interruptAt
		assertAliasDeletionJobEqual(t, mustGetAliasDeletionJob(t, restarted, previous), want)
		if _, err := restarted.GetActiveAliasDeletionJob(ctx, previous.AdminID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("active lookup after interruption error = %v", err)
		}
	}
	var retainedJSON string
	if err := restarted.db.QueryRowContext(ctx, `SELECT items_json FROM alias_deletion_jobs WHERE id = ?`, job.ID).Scan(&retainedJSON); err != nil {
		t.Fatal(err)
	}
	if retainedJSON != originalJSON {
		t.Fatal("startup interruption rewrote partial item results")
	}
	assertAliasDeletionJobEqual(t, mustGetAliasDeletionJob(t, restarted, completed), completed)
	assertAliasDeletionJobEqual(t, mustGetAliasDeletionJob(t, restarted, interrupted), interrupted)
	// Repeated interruption leaves the first interruption timestamp unchanged.
	restarted.now = func() time.Time { return interruptAt.Add(time.Hour) }
	if err := restarted.InterruptAliasDeletionJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mustGetAliasDeletionJob(t, restarted, job); !got.UpdatedAt.Equal(interruptAt) {
		t.Fatalf("repeated interruption changed timestamp: %v", got.UpdatedAt)
	}
	job.Status = domain.AliasDeletionJobCompleted
	if err := restarted.SaveAliasDeletionJob(ctx, job, &domain.AuditLog{Action: "late-worker"}); !errors.Is(err, ErrAliasDeletionJobConflict) {
		t.Fatalf("late worker save error = %v", err)
	}
	assertAliasDeletionJobAuditCount(t, restarted, 0)
	mustCreateAliasDeletionJob(t, restarted, aliasDeletionJobTestFixture("new-after-restart", owner.ID))
}

func TestAliasDeletionJobAuditAndProgressAreAtomic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openAliasDeletionJobTestStore(t, ":memory:")
	owner := createAliasDeletionJobTestAdmin(t, s, "owner")
	original := aliasDeletionJobTestFixture("atomic", owner.ID)
	mustCreateAliasDeletionJob(t, s, original)
	update := original
	update.Items = append([]domain.AliasDeletionJobItem(nil), original.Items...)
	update.Items[0].Done = true
	update.Items[0].Deleted = true
	update.Status = domain.AliasDeletionJobRunning
	update.RequestID = "must-not-replace-request-id"
	update.CreatedAt = update.CreatedAt.Add(time.Hour)
	update.UpdatedAt = update.UpdatedAt.Add(time.Second)
	// This trigger proves the snapshot update is visible inside the audit's
	// transaction. A subsequent foreign-key failure must roll that update back.
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER alias_deletion_audit_requires_progress
		BEFORE INSERT ON audit_logs BEGIN
			SELECT CASE WHEN NOT EXISTS (
				SELECT 1 FROM alias_deletion_jobs
				WHERE id = NEW.resource_id AND status = 'running'
				AND updated_at = NEW.created_at AND json_extract(items_json, '$[0].done') = 1
			) THEN RAISE(ABORT, 'audit did not observe saved progress') END;
		END`); err != nil {
		t.Fatalf("install audit visibility check: %v", err)
	}
	missingAdmin := owner.ID + 1000
	audit := domain.AuditLog{
		AdminID: &missingAdmin, Action: "alias_delete_item", ResourceType: "alias_deletion_job",
		ResourceID: original.ID, RequestID: original.RequestID, CreatedAt: update.UpdatedAt,
		Result: "ok", Detail: "Confirmed first item deletion",
	}
	if err := s.SaveAliasDeletionJob(ctx, update, &audit); err == nil || errors.Is(err, ErrAliasDeletionJobConflict) {
		t.Fatalf("audit foreign-key failure error = %v", err)
	}
	assertAliasDeletionJobEqual(t, mustGetAliasDeletionJob(t, s, original), original)
	assertAliasDeletionJobAuditCount(t, s, 0)

	audit.AdminID = &owner.ID
	if err := s.SaveAliasDeletionJob(ctx, update, &audit); err != nil {
		t.Fatalf("save progress and audit: %v", err)
	}
	want := update
	want.RequestID = original.RequestID
	want.CreatedAt = original.CreatedAt
	assertAliasDeletionJobEqual(t, mustGetAliasDeletionJob(t, s, original), want)
	logs, err := s.ListAuditLogs(ctx, 10, 0)
	if err != nil || len(logs) != 1 || logs[0].AdminID == nil || *logs[0].AdminID != owner.ID ||
		logs[0].RequestID != original.RequestID || logs[0].ResourceID != original.ID || logs[0].Detail != audit.Detail {
		t.Fatalf("saved audit logs = %#v, err = %v", logs, err)
	}
	if audit.ID != 0 {
		t.Fatal("save mutated caller's audit record")
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER alias_deletion_reject_progress
		BEFORE UPDATE ON alias_deletion_jobs BEGIN SELECT RAISE(ABORT, 'injected progress failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAliasDeletionJob(ctx, update, &audit); err == nil {
		t.Fatal("injected progress failure was ignored")
	}
	assertAliasDeletionJobEqual(t, mustGetAliasDeletionJob(t, s, original), want)
	assertAliasDeletionJobAuditCount(t, s, 1)
}

func TestAliasDeletionJobValidationAndNormalization(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openAliasDeletionJobTestStore(t, ":memory:")
	owner := createAliasDeletionJobTestAdmin(t, s, "owner")
	for _, test := range []struct {
		name string
		edit func(*domain.AliasDeletionJob)
	}{
		{"empty ID", func(j *domain.AliasDeletionJob) { j.ID = " \t" }},
		{"long ID", func(j *domain.AliasDeletionJob) { j.ID = strings.Repeat("x", 129) }},
		{"NUL ID", func(j *domain.AliasDeletionJob) { j.ID = "job\x00id" }},
		{"invalid UTF8 ID", func(j *domain.AliasDeletionJob) { j.ID = "job\xff" }},
		{"zero owner", func(j *domain.AliasDeletionJob) { j.AdminID = 0 }},
		{"negative owner", func(j *domain.AliasDeletionJob) { j.AdminID = -1 }},
		{"invalid status", func(j *domain.AliasDeletionJob) { j.Status = "resuming" }},
		{"nil items", func(j *domain.AliasDeletionJob) { j.Items = nil }},
		{"empty items", func(j *domain.AliasDeletionJob) { j.Items = []domain.AliasDeletionJobItem{} }},
		{"too many items", func(j *domain.AliasDeletionJob) { j.Items = make([]domain.AliasDeletionJobItem, 1001) }},
		{"zero alias ID", func(j *domain.AliasDeletionJob) { j.Items[0].ID = 0 }},
		{"negative alias ID", func(j *domain.AliasDeletionJob) { j.Items[0].ID = -1 }},
		{"duplicate alias ID", func(j *domain.AliasDeletionJob) { j.Items[1].ID = j.Items[0].ID }},
		{"long address", func(j *domain.AliasDeletionJob) { j.Items[0].Address = strings.Repeat("x", 321) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := aliasDeletionJobTestFixture("invalid", owner.ID)
			test.edit(&job)
			if err := s.CreateAliasDeletionJob(ctx, job); err == nil || errors.Is(err, ErrAliasDeletionJobConflict) {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
	if _, err := s.GetLatestAliasDeletionJob(ctx, owner.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("validation persisted a row: %v", err)
	}
	missingOwner := aliasDeletionJobTestFixture("missing-owner", owner.ID+1000)
	if err := s.CreateAliasDeletionJob(ctx, missingOwner); err == nil || errors.Is(err, ErrAliasDeletionJobConflict) {
		t.Fatalf("owner foreign-key error = %v", err)
	}

	job := aliasDeletionJobTestFixture(" normalized ", owner.ID)
	job.Status = " "
	job.RequestID = " " + strings.Repeat("r", 200) + " "
	job.CreatedAt, job.UpdatedAt = time.Time{}, time.Time{}
	job.Items[0].Address = " first@icloud.com "
	job.Items[0].Code = strings.Repeat("C", 80)
	job.Items[0].Message = " " + strings.Repeat("中", 1100) + " "
	job.Items[1].Message = "\x00\xfftext"
	before := append([]domain.AliasDeletionJobItem(nil), job.Items...)
	now := time.Date(2026, 9, 8, 10, 0, 0, 123456789, time.UTC)
	s.now = func() time.Time { return now }
	mustCreateAliasDeletionJob(t, s, job)
	if !reflect.DeepEqual(job.Items, before) {
		t.Fatal("normalization mutated caller's items")
	}
	got, err := s.GetAliasDeletionJob(ctx, "normalized", owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.AliasDeletionJobQueued || got.RequestID != strings.Repeat("r", 128) ||
		!got.CreatedAt.Equal(now) || !got.UpdatedAt.Equal(now) || got.Items[0].Address != "first@icloud.com" ||
		got.Items[0].Code != strings.Repeat("C", 64) || utf8.RuneCountInString(got.Items[0].Message) != 1024 ||
		got.Items[1].Message != "\uFFFD\uFFFDtext" {
		t.Fatalf("normalized job = %#v", got)
	}
	got.Status = domain.AliasDeletionJobCompleted
	if err := s.SaveAliasDeletionJob(ctx, got, nil); err != nil {
		t.Fatal(err)
	}
	maxJob := aliasDeletionJobTestFixture(strings.Repeat("j", 128), owner.ID)
	maxJob.RequestID = strings.Repeat("r", 128)
	maxJob.Items = make([]domain.AliasDeletionJobItem, 1000)
	for index := range maxJob.Items {
		maxJob.Items[index] = domain.AliasDeletionJobItem{ID: int64(index + 1)}
	}
	mustCreateAliasDeletionJob(t, s, maxJob)
	assertAliasDeletionJobEqual(t, mustGetAliasDeletionJob(t, s, maxJob), maxJob)
}

func TestAliasDeletionJobJSONContract(t *testing.T) {
	t.Parallel()
	job := aliasDeletionJobTestFixture("json-contract", 1)
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	assertAliasDeletionJobJSONKeys(t, fields, "id", "admin_id", "request_id", "status", "items", "created_at", "updated_at")
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(fields["items"], &items); err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		assertAliasDeletionJobJSONKeys(t, item, "id", "address", "done", "deleted", "code", "message", "local_retained")
	}
}

func TestAliasDeletionJobWriteErrorUsesDriverCodes(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"23505", "23503", "23502", "23514", "40001"} {
		driverErr := &pgconn.PgError{Code: code, Message: "localized database error"}
		wrapped := fmt.Errorf("wrapped: %w", driverErr)
		got := aliasDeletionJobWriteError(wrapped)
		if code == "23505" {
			if !errors.Is(got, ErrAliasDeletionJobConflict) {
				t.Fatalf("unique violation error = %v", got)
			}
		} else if !errors.Is(got, driverErr) || errors.Is(got, ErrAliasDeletionJobConflict) {
			t.Fatalf("non-unique code %s was reclassified: %v", code, got)
		}
	}
	for _, message := range []string{"unique constraint", "duplicate key", "unique violation", "disk failure"} {
		original := errors.New(message)
		if got := aliasDeletionJobWriteError(original); got != original {
			t.Fatalf("untyped error %q was reclassified: %v", message, got)
		}
	}
	if got := aliasDeletionJobWriteError(nil); got != nil {
		t.Fatalf("nil error = %v", got)
	}
}

func openAliasDeletionJobTestStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open alias deletion test store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func createAliasDeletionJobTestAdmin(t *testing.T, s *Store, username string) domain.Admin {
	t.Helper()
	admin, err := s.CreateAdmin(context.Background(), username, "test-hash")
	if err != nil {
		t.Fatalf("create alias deletion test admin: %v", err)
	}
	return admin
}

func aliasDeletionJobTestFixture(id string, adminID int64) domain.AliasDeletionJob {
	now := time.Date(2026, 9, 8, 9, 0, 0, 123456789, time.UTC)
	return domain.AliasDeletionJob{
		ID: id, AdminID: adminID, RequestID: "request-" + id,
		Status: domain.AliasDeletionJobQueued, CreatedAt: now, UpdatedAt: now,
		Items: []domain.AliasDeletionJobItem{
			{ID: 101, Address: "first@icloud.com"},
			{ID: 102, Address: "second@icloud.com"},
			{ID: 103, Address: "pending@icloud.com"},
		},
	}
}

func mustCreateAliasDeletionJob(t *testing.T, s *Store, job domain.AliasDeletionJob) {
	t.Helper()
	if err := s.CreateAliasDeletionJob(context.Background(), job); err != nil {
		t.Fatalf("create alias deletion job %q: %v", job.ID, err)
	}
}

func mustGetAliasDeletionJob(t *testing.T, s *Store, job domain.AliasDeletionJob) domain.AliasDeletionJob {
	t.Helper()
	got, err := s.GetAliasDeletionJob(context.Background(), job.ID, job.AdminID)
	if err != nil {
		t.Fatalf("get alias deletion job %q: %v", job.ID, err)
	}
	return got
}

func assertAliasDeletionJobEqual(t *testing.T, got, want domain.AliasDeletionJob) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("alias deletion job = %#v, want %#v", got, want)
	}
}

func assertAliasDeletionJobAuditCount(t *testing.T, s *Store, want int) {
	t.Helper()
	var got int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_logs`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("audit count = %d, want %d", got, want)
	}
}

func assertAliasDeletionJobJSONKeys(t *testing.T, fields map[string]json.RawMessage, names ...string) {
	t.Helper()
	if len(fields) != len(names) {
		t.Fatalf("JSON fields = %v, want only %v", fields, names)
	}
	for _, name := range names {
		if _, ok := fields[name]; !ok {
			t.Fatalf("missing snake-case JSON field %q", name)
		}
	}
}
