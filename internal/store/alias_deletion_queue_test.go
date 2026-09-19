package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"icloud-api/internal/domain"
)

func TestAliasDeletionQueueFIFOAcrossJobsAndUnlimitedSubjects(t *testing.T) {
	t.Parallel()
	s := openAliasDeletionJobTestStore(t, ":memory:")
	admin := createAliasDeletionJobTestAdmin(t, s, "queue-owner")
	first := enqueueDeletionFixture(t, s, admin, "first", deletionTarget(1, "A"), deletionTarget(2, "A"))
	second := enqueueDeletionFixture(t, s, admin, "second", deletionTarget(3, "A"), deletionTarget(4, "B"))
	heads, err := s.ListAliasDeletionQueueHeads(t.Context(), time.Now())
	if err != nil || len(heads) != 2 || heads[0].AliasID != 1 || heads[1].AliasID != 4 {
		t.Fatalf("independent FIFO heads = %#v, %v", heads, err)
	}
	if _, err := s.ClaimAliasDeletionWork(t.Context(), first.Items[1].WorkID, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("later work overtook head: %v", err)
	}
	work := claimDeletionFixture(t, s, first.Items[0].WorkID)
	work.Status, work.NextRunAt, work.WaitReason = domain.AliasDeletionWorkWaiting, time.Now().Add(time.Hour), "quota"
	work.Used, work.Limit = 200, 200
	if err := s.SaveAliasDeletionWork(t.Context(), work); err != nil {
		t.Fatal(err)
	}
	heads, err = s.ListAliasDeletionQueueHeads(t.Context(), time.Now())
	if err != nil || len(heads) != 1 || heads[0].ID != second.Items[1].WorkID {
		t.Fatalf("quota wait blocked another account: %#v, %v", heads, err)
	}
	jobs, err := s.ListAliasDeletionJobs(t.Context(), admin.ID, 1)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("all active jobs listed despite history limit: %#v, %v", jobs, err)
	}
	got := mustGetAliasDeletionJob(t, s, first)
	if got.Items[0].WaitReason != "quota" || got.Items[0].Used != 200 || !got.Items[0].RetryAt.Equal(work.NextRunAt) {
		t.Fatalf("waiting snapshot = %#v", got.Items[0])
	}
	// Every additional subject is immediately claimable; there is no global cap.
	for index := int64(10); index < 20; index++ {
		job := enqueueDeletionFixture(t, s, admin, fmt.Sprintf("parallel-%d", index), deletionTarget(index, fmt.Sprint(index)))
		_ = claimDeletionFixture(t, s, job.Items[0].WorkID)
	}
}

func TestAliasDeletionQueueDeduplicatesAndCancelsOnlySubscriber(t *testing.T) {
	t.Parallel()
	s := openAliasDeletionJobTestStore(t, ":memory:")
	a := createAliasDeletionJobTestAdmin(t, s, "owner-a")
	b := createAliasDeletionJobTestAdmin(t, s, "owner-b")
	target := deletionTarget(1, "A")
	first := enqueueDeletionFixture(t, s, a, "first", target)
	target.Address = "  ALIAS-1@ICLOUD.COM "
	second := enqueueDeletionFixture(t, s, b, "second", target)
	if first.Items[0].WorkID != second.Items[0].WorkID {
		t.Fatal("duplicate remote mailbox created new work")
	}
	if _, err := s.CancelAliasDeletionJob(t.Context(), first.ID, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner cancellation = %v", err)
	}
	cancelled, err := s.CancelAliasDeletionJob(t.Context(), first.ID, a.ID)
	if err != nil || !cancelled.CancelRequested || !cancelled.Items[0].Done || cancelled.Items[0].Code != "BATCH_DELETE_CANCELLED" {
		t.Fatalf("cancelled = %#v, %v", cancelled, err)
	}
	wanted, err := s.AliasDeletionWorkWanted(t.Context(), first.Items[0].WorkID)
	if err != nil || !wanted {
		t.Fatalf("other subscription was cancelled: %v %v", wanted, err)
	}
	work := claimDeletionFixture(t, s, second.Items[0].WorkID)
	work.Status, work.Deleted, work.Code = domain.AliasDeletionWorkSucceeded, true, "DELETED"
	if err := s.SaveAliasDeletionWork(t.Context(), work); err != nil {
		t.Fatal(err)
	}
	got := mustGetAliasDeletionJob(t, s, second)
	if got.Status != domain.AliasDeletionJobCompleted || !got.Items[0].Deleted {
		t.Fatalf("shared completion = %#v", got)
	}
	if got := mustGetAliasDeletionJob(t, s, first); got.Items[0].Code != "BATCH_DELETE_CANCELLED" {
		t.Fatal("cancelled subscription overwritten")
	}
	if err := s.SaveAliasDeletionWork(t.Context(), work); !errors.Is(err, ErrAliasDeletionJobConflict) {
		t.Fatalf("duplicate completion = %v", err)
	}
	assertAliasDeletionJobAuditCount(t, s, 2)
}

func TestAliasDeletionQueueCancelsPendingButRecordsInFlightResult(t *testing.T) {
	t.Parallel()
	s := openAliasDeletionJobTestStore(t, ":memory:")
	admin := createAliasDeletionJobTestAdmin(t, s, "owner")
	job := enqueueDeletionFixture(t, s, admin, "cancel", deletionTarget(1, "A"), deletionTarget(2, "A"))
	work := claimDeletionFixture(t, s, job.Items[0].WorkID)
	cancelled, err := s.CancelAliasDeletionJob(t.Context(), job.ID, admin.ID)
	if err != nil || cancelled.Items[0].Done || !cancelled.Items[1].Done {
		t.Fatalf("in-flight/pending cancellation = %#v, %v", cancelled, err)
	}
	if wanted, err := s.AliasDeletionWorkWanted(t.Context(), work.ID); err != nil || wanted {
		t.Fatalf("cancelled work still wanted: %t %v", wanted, err)
	}
	work.Status, work.Deleted, work.Code = domain.AliasDeletionWorkSucceeded, true, "DELETED"
	if err := s.SaveAliasDeletionWork(t.Context(), work); err != nil {
		t.Fatal(err)
	}
	got := mustGetAliasDeletionJob(t, s, job)
	if !got.Items[0].Deleted || got.Items[0].LocalRetained || got.Status != domain.AliasDeletionJobCompleted {
		t.Fatalf("in-flight actual result lost: %#v", got)
	}
	heads, err := s.ListAliasDeletionQueueHeads(t.Context(), time.Now())
	if err != nil || len(heads) != 0 {
		t.Fatalf("cancelled tail remains runnable: %#v %v", heads, err)
	}
}

func TestAliasDeletionQueueRestartReconcilesOnlyNewInFlightWork(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "queue.db")
	s := openAliasDeletionJobTestStore(t, path)
	admin := createAliasDeletionJobTestAdmin(t, s, "owner")
	legacy := aliasDeletionJobTestFixture("legacy", admin.ID)
	mustCreateAliasDeletionJob(t, s, legacy)
	job := enqueueDeletionFixture(t, s, admin, "new", deletionTarget(1, "A"), deletionTarget(2, "B"), deletionTarget(3, "C"))
	oldClaim := claimDeletionFixture(t, s, job.Items[0].WorkID)
	waiting := claimDeletionFixture(t, s, job.Items[1].WorkID)
	waiting.Status, waiting.NextRunAt, waiting.WaitReason = domain.AliasDeletionWorkWaiting, time.Now().Add(time.Hour), "quota"
	if err := s.SaveAliasDeletionWork(t.Context(), waiting); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openAliasDeletionJobTestStore(t, path)
	if err := s.RecoverAliasDeletionQueue(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := mustGetAliasDeletionJob(t, s, legacy); got.Status != domain.AliasDeletionJobInterrupted {
		t.Fatalf("legacy resumed: %#v", got)
	}
	if err := s.InterruptAliasDeletionJobs(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := mustGetAliasDeletionJob(t, s, job)
	if got.Status == domain.AliasDeletionJobInterrupted || got.Items[0].State != domain.AliasDeletionWorkPending || !got.Items[1].RetryAt.Equal(waiting.NextRunAt) {
		t.Fatalf("recovered snapshot = %#v", got)
	}
	recovered := claimDeletionFixture(t, s, job.Items[0].WorkID)
	if !recovered.Reconcile || recovered.ClaimToken == oldClaim.ClaimToken || recovered.Attempts != 2 {
		t.Fatalf("recovery claim = %#v", recovered)
	}
	oldClaim.Status, oldClaim.Deleted = domain.AliasDeletionWorkSucceeded, true
	if err := s.SaveAliasDeletionWork(t.Context(), oldClaim); !errors.Is(err, ErrAliasDeletionJobConflict) {
		t.Fatalf("stale pre-restart worker overwrote recovery: %v", err)
	}
}

func TestAliasDeletionQueueRevokesStaleAuthorizationAndDeletedOwner(t *testing.T) {
	t.Parallel()
	s := openAliasDeletionJobTestStore(t, ":memory:")
	a := createAliasDeletionJobTestAdmin(t, s, "a")
	b := createAliasDeletionJobTestAdmin(t, s, "b")
	first := enqueueDeletionFixture(t, s, a, "first", deletionTarget(1, "A"), deletionTarget(2, "A"))
	shared := enqueueDeletionFixture(t, s, b, "shared", deletionTarget(1, "A"))
	if _, err := s.execContext(t.Context(), `UPDATE admins SET password_version = password_version + 1 WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	heads, err := s.ListAliasDeletionQueueHeads(t.Context(), time.Now())
	if err != nil || len(heads) != 1 || heads[0].ID != shared.Items[0].WorkID {
		t.Fatalf("valid subscriber lost work: %#v %v", heads, err)
	}
	got := mustGetAliasDeletionJob(t, s, first)
	if !got.CancelRequested || got.Status != domain.AliasDeletionJobInterrupted || !got.Items[1].Done {
		t.Fatalf("stale authorization not revoked: %#v", got)
	}
	if _, err := s.execContext(t.Context(), `DELETE FROM admins WHERE id = ?`, b.ID); err != nil {
		t.Fatal(err)
	}
	heads, err = s.ListAliasDeletionQueueHeads(t.Context(), time.Now())
	if err != nil || len(heads) != 0 {
		t.Fatalf("deleted owner left runnable work: %#v %v", heads, err)
	}
	var count int
	if err := s.queryRowContext(t.Context(), `SELECT COUNT(*) FROM alias_deletion_work`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("owner deletion removed execution records: %d %v", count, err)
	}
	job := domain.AliasDeletionJob{ID: "stale", AdminID: a.ID, AuthorizingPasswordVersion: a.PasswordVersion, Items: []domain.AliasDeletionJobItem{{ID: 10}}}
	if _, err := s.EnqueueAliasDeletionJob(t.Context(), job, []domain.AliasDeletionWork{deletionTarget(10, "A")}); !errors.Is(err, ErrCredentialsChanged) {
		t.Fatalf("stale enqueue allowed: %v", err)
	}
}

func TestAliasDeletionQueueConcurrentClaimsAndSnapshotMerges(t *testing.T) {
	t.Parallel()
	s := openAliasDeletionJobTestStore(t, filepath.Join(t.TempDir(), "parallel.db"))
	admin := createAliasDeletionJobTestAdmin(t, s, "owner")
	job := enqueueDeletionFixture(t, s, admin, "job", deletionTarget(1, "A"), deletionTarget(2, "B"))
	const count = 8
	var wg sync.WaitGroup
	claims := make(chan domain.AliasDeletionWork, count)
	errorsCh := make(chan error, count)
	for range count {
		wg.Go(func() {
			work, err := s.ClaimAliasDeletionWork(t.Context(), job.Items[0].WorkID, time.Now())
			if err == nil {
				claims <- work
			} else if !errors.Is(err, ErrNotFound) {
				errorsCh <- err
			}
		})
	}
	wg.Wait()
	close(claims)
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
	if len(claims) != 1 {
		t.Fatalf("same work claimed %d times", len(claims))
	}
	first := <-claims
	second := claimDeletionFixture(t, s, job.Items[1].WorkID)
	errorsCh = make(chan error, 2)
	for _, work := range []domain.AliasDeletionWork{first, second} {
		wg.Go(func() {
			work.Status, work.Deleted = domain.AliasDeletionWorkSucceeded, true
			errorsCh <- s.SaveAliasDeletionWork(t.Context(), work)
		})
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	got := mustGetAliasDeletionJob(t, s, job)
	if got.Status != domain.AliasDeletionJobCompleted || !got.Items[0].Deleted || !got.Items[1].Deleted {
		t.Fatalf("parallel subject progress lost: %#v", got)
	}
	assertAliasDeletionJobAuditCount(t, s, 2)
}

func TestAliasDeletionQueueIdempotentRequestAndAtomicAuditFailure(t *testing.T) {
	t.Parallel()
	s := openAliasDeletionJobTestStore(t, ":memory:")
	admin := createAliasDeletionJobTestAdmin(t, s, "owner")
	job := enqueueDeletionFixture(t, s, admin, "request", deletionTarget(2, "A"), deletionTarget(1, "A"))
	repeated := enqueueDeletionFixture(t, s, admin, "request", deletionTarget(1, "A"), deletionTarget(2, "A"))
	if repeated.Items[0].ID != 2 || repeated.Items[0].WorkID != job.Items[0].WorkID {
		t.Fatal("idempotent enqueue changed request order")
	}
	work := claimDeletionFixture(t, s, job.Items[0].WorkID)
	if _, err := s.execContext(t.Context(), `CREATE TRIGGER reject_queue_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(ABORT, 'injected audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	work.Status, work.Deleted = domain.AliasDeletionWorkSucceeded, true
	if err := s.SaveAliasDeletionWork(t.Context(), work); err == nil {
		t.Fatal("injected audit failure ignored")
	}
	if got := mustGetAliasDeletionJob(t, s, job); got.Items[0].Done {
		t.Fatal("audit failure committed partial snapshot")
	}
	if _, err := s.execContext(t.Context(), `DROP TRIGGER reject_queue_audit`); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAliasDeletionWork(t.Context(), work); err != nil {
		t.Fatal(err)
	}
	assertAliasDeletionJobAuditCount(t, s, 1)
}

func deletionTarget(aliasID int64, subject string) domain.AliasDeletionWork {
	return domain.AliasDeletionWork{AccountID: int64(subject[0]), AliasID: aliasID, Address: fmt.Sprintf("alias-%d@icloud.com", aliasID), AccountEmail: subject + "@icloud.com", AppleID: subject + "@icloud.com", AppleSubject: subject}
}

func enqueueDeletionFixture(t *testing.T, s *Store, admin domain.Admin, id string, targets ...domain.AliasDeletionWork) domain.AliasDeletionJob {
	t.Helper()
	job := domain.AliasDeletionJob{ID: id, AdminID: admin.ID, AuthorizingPasswordVersion: admin.PasswordVersion, Username: admin.Username, RequestID: "req-" + id}
	for _, target := range targets {
		job.Items = append(job.Items, domain.AliasDeletionJobItem{ID: target.AliasID, Address: target.Address})
	}
	got, err := s.EnqueueAliasDeletionJob(context.Background(), job, targets)
	if err != nil {
		t.Fatalf("enqueue %q: %v", id, err)
	}
	return got
}

func claimDeletionFixture(t *testing.T, s *Store, id string) domain.AliasDeletionWork {
	t.Helper()
	work, err := s.ClaimAliasDeletionWork(t.Context(), id, time.Now())
	if err != nil {
		t.Fatalf("claim %q: %v", id, err)
	}
	return work
}
