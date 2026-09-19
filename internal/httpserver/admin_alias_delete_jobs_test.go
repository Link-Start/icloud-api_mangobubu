package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-api/internal/domain"
	"icloud-api/internal/hmesync"
	"icloud-api/internal/store"
)

const testAliasDeletionJobID = "test-alias-deletion-operation-0001"

func startTestAliasDeletionJobs(t *testing.T, env *adminAPITestEnv) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if err := env.server.StartAliasDeletionJobs(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { env.server.RunAliasDeletionJobs(); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("deletion workers did not stop")
			return
		}
		// database/sql can finish a cancelled transaction in its own awaitDone
		// goroutine after the queue worker returns. Join that rollback before
		// the environment closes SQLite and TempDir removes its Windows file.
		deadline := time.Now().Add(5 * time.Second)
		for env.store.DB().Stats().InUse != 0 {
			if time.Now().After(deadline) {
				t.Errorf("database connections still in use after deletion shutdown: %#v", env.store.DB().Stats())
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
	return cancel, done
}

func waitTestAliasDeletionSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for deletion barrier")
	}
}

func waitTestAliasDeletionState(t *testing.T, env *adminAPITestEnv, adminID int64, jobID string, predicate func(domain.AliasDeletionJob) bool) domain.AliasDeletionJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		job, err := env.store.GetAliasDeletionJob(context.Background(), jobID, adminID)
		if err == nil && predicate(job) {
			return job
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not reach expected state: %#v err=%v", job, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitTestAliasDeletionJob(t *testing.T, env *adminAPITestEnv, adminID int64, jobID string) domain.AliasDeletionJob {
	t.Helper()
	return waitTestAliasDeletionState(t, env, adminID, jobID, func(job domain.AliasDeletionJob) bool { return !aliasDeletionJobActive(job.Status) })
}

func decodeTestAliasDeletionJob(t *testing.T, response *httptest.ResponseRecorder) adminAPIAliasDeletionJobDTO {
	t.Helper()
	var body struct{ Data adminAPIAliasDeletionJobDTO }
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Data
}

func submitTestDeletionJob(t *testing.T, env *adminAPITestEnv, cookie *http.Cookie, csrf, id string, aliases ...int64) adminAPIAliasDeletionJobDTO {
	t.Helper()
	response := env.request(t, http.MethodDelete, "/admin/api/v1/aliases/batch?async=1", adminAPITestJSON(t, map[string]any{"alias_ids": aliases, "operation_id": id}), "application/json", []*http.Cookie{cookie}, csrf)
	if response.Code != http.StatusAccepted {
		t.Fatalf("admission = %d %s", response.Code, response.Body.String())
	}
	return decodeTestAliasDeletionJob(t, response)
}

type queuedDeletionTestService struct {
	*fakeHMESyncService
	prepare func(context.Context, int64) (domain.AliasDeletionWork, error)
	execute func(context.Context, domain.AliasDeletionWork) error
}

func (f *queuedDeletionTestService) PrepareAliasDeletion(ctx context.Context, id int64) (domain.AliasDeletionWork, error) {
	return f.prepare(ctx, id)
}
func (f *queuedDeletionTestService) DeleteQueuedAlias(ctx context.Context, work domain.AliasDeletionWork) error {
	return f.execute(ctx, work)
}
func newQueuedDeletionTestService(env *adminAPITestEnv, execute func(context.Context, domain.AliasDeletionWork) error) *queuedDeletionTestService {
	return &queuedDeletionTestService{
		fakeHMESyncService: &fakeHMESyncService{getSession: func(context.Context, int64) (hmesync.SessionInfo, error) {
			return hmesync.SessionInfo{Status: hmesync.StatusAuthenticated}, nil
		}},
		prepare: func(ctx context.Context, id int64) (domain.AliasDeletionWork, error) {
			alias, err := env.store.GetAlias(ctx, id)
			if err != nil {
				return domain.AliasDeletionWork{}, err
			}
			account, err := env.store.GetAccount(ctx, alias.AccountID)
			if err != nil {
				return domain.AliasDeletionWork{}, err
			}
			return domain.AliasDeletionWork{AccountID: account.ID, AliasID: id, Address: alias.Address, AccountEmail: account.Email, AppleID: account.Email, AppleSubject: fmt.Sprintf("global:%d", account.ID)}, nil
		}, execute: execute,
	}
}

func TestAliasDeletionJobReturnsBeforeAppleAndSurvivesRequestCancellation(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, admin := env.createSession(t, "queue-disconnect-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "queue-disconnect@icloud.com")
	first := adminAPITestCreateDeleteAlias(t, env, account.ID, "queue-disconnect-first@icloud.com")
	second := adminAPITestCreateDeleteAlias(t, env, account.ID, "queue-disconnect-second@icloud.com")
	firstEntered, releaseFirst, secondEntered, releaseSecond := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	env.server.SetHMESyncService(newQueuedDeletionTestService(env, func(ctx context.Context, work domain.AliasDeletionWork) error {
		calls.Add(1)
		if work.AliasID == first.ID {
			close(firstEntered)
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return ctx.Err()
			}
			return env.store.DeleteAlias(ctx, work.AliasID)
		}
		close(secondEntered)
		select {
		case <-releaseSecond:
		case <-ctx.Done():
			return ctx.Err()
		}
		return hmesync.ErrUpstream
	}))
	startTestAliasDeletionJobs(t, env)
	requestContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := adminAPITestJSON(t, map[string]any{"alias_ids": []int64{first.ID, second.ID}, "operation_id": testAliasDeletionJobID})
	request := httptest.NewRequest(http.MethodDelete, "http://admin.example.test/admin/api/v1/aliases/batch?async=1", bytes.NewReader(body)).WithContext(requestContext)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://admin.example.test")
	request.Header.Set(adminAPICSRFHeader, csrf)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	env.router.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("admission = %d %s", response.Code, response.Body.String())
	}
	cancel()
	waitTestAliasDeletionSignal(t, firstEntered)
	close(releaseFirst)
	waitTestAliasDeletionSignal(t, secondEntered)
	progress := env.request(t, http.MethodGet, "/admin/api/v1/aliases/batch/jobs/"+testAliasDeletionJobID, nil, "", []*http.Cookie{cookie}, "")
	partial := decodeTestAliasDeletionJob(t, progress)
	if partial.Processed != 1 || partial.Deleted != 1 || partial.Pending != 1 || partial.Failed != 0 {
		t.Fatalf("partial=%#v", partial)
	}
	retry := submitTestDeletionJob(t, env, cookie, csrf, testAliasDeletionJobID, first.ID, second.ID)
	if retry.JobID != testAliasDeletionJobID || calls.Load() != 2 {
		t.Fatalf("retry=%#v calls=%d", retry, calls.Load())
	}
	conflicting := env.request(t, http.MethodDelete, "/admin/api/v1/aliases/batch?async=1", adminAPITestJSON(t, map[string]any{"alias_ids": []int64{second.ID}, "operation_id": testAliasDeletionJobID}), "application/json", []*http.Cookie{cookie}, csrf)
	if conflicting.Code != http.StatusConflict || adminAPITestErrorCode(t, conflicting) != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("conflict=%d %s", conflicting.Code, conflicting.Body.String())
	}
	close(releaseSecond)
	final := adminAPIAliasDeletionJobFromRecord(waitTestAliasDeletionJob(t, env, admin.ID, testAliasDeletionJobID))
	if final.Processed != 2 || final.Deleted != 1 || final.Failed != 1 || final.Pending != 0 {
		t.Fatalf("final=%#v", final)
	}
	audits, err := env.store.ListAuditLogs(context.Background(), 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	confirmed := false
	for _, audit := range audits {
		if audit.ResourceID == fmt.Sprint(first.ID) && audit.Result == "success" && strings.Contains(audit.Detail, testAliasDeletionJobID) {
			confirmed = true
		}
	}
	if !confirmed {
		t.Fatal("confirmed deletion audit missing")
	}
}

func TestAliasDeletionQueueAccountsHaveNoGlobalConcurrencyLimit(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, admin := env.createSession(t, "queue-parallel-admin", "unused-password")
	const count = 7
	ids := make([]int64, 0, count)
	for i := range count {
		account := adminAPITestCreateAccount(t, env, fmt.Sprintf("parallel-%d@icloud.com", i))
		alias := adminAPITestCreateDeleteAlias(t, env, account.ID, fmt.Sprintf("parallel-alias-%d@icloud.com", i))
		ids = append(ids, alias.ID)
	}
	entered, finish := make(chan int64, count), make(chan struct{})
	env.server.SetHMESyncService(newQueuedDeletionTestService(env, func(ctx context.Context, work domain.AliasDeletionWork) error {
		entered <- work.AccountID
		select {
		case <-finish:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}))
	startTestAliasDeletionJobs(t, env)
	// Multiple submissions by one administrator are independently accepted.
	for i, id := range ids {
		submitTestDeletionJob(t, env, cookie, csrf, fmt.Sprintf("parallel-job-%016d", i), id)
	}
	for range count {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("accounts were serialized or capped")
		}
	}
	env.server.aliasDeletionJobs.mu.Lock()
	active := len(env.server.aliasDeletionJobs.active)
	env.server.aliasDeletionJobs.mu.Unlock()
	if active != count {
		t.Fatalf("active accounts=%d want=%d", active, count)
	}
	if release, ok := env.server.beginAliasDeletionCredentialRotation(); ok {
		release()
		t.Fatal("rotation crossed executing requests")
	}
	close(finish)
	for i := range count {
		waitTestAliasDeletionJob(t, env, admin.ID, fmt.Sprintf("parallel-job-%016d", i))
	}
}

func TestAliasDeletionQueueFIFOAndSharedWorkCancellation(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, admin := env.createSession(t, "queue-shared-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "queue-shared@icloud.com")
	first := adminAPITestCreateDeleteAlias(t, env, account.ID, "queue-shared-first@icloud.com")
	second := adminAPITestCreateDeleteAlias(t, env, account.ID, "queue-shared-second@icloud.com")
	base := time.Now().UTC()
	var clockOffset atomic.Int64
	env.server.now = func() time.Time { return base.Add(time.Duration(clockOffset.Load())) }
	var callsMu sync.Mutex
	var calls []int64
	var firstAttempts atomic.Int32
	env.server.SetHMESyncService(newQueuedDeletionTestService(env, func(_ context.Context, work domain.AliasDeletionWork) error {
		callsMu.Lock()
		calls = append(calls, work.AliasID)
		callsMu.Unlock()
		if work.AliasID == first.ID && firstAttempts.Add(1) == 1 {
			return &hmesync.AliasDeletionWaitError{Reason: "quota", RetryAt: base.Add(time.Hour), Used: 200, Limit: 200}
		}
		return nil
	}))
	startTestAliasDeletionJobs(t, env)
	submitTestDeletionJob(t, env, cookie, csrf, testAliasDeletionJobID, first.ID)
	waiting := waitTestAliasDeletionState(t, env, admin.ID, testAliasDeletionJobID, func(job domain.AliasDeletionJob) bool { return job.Items[0].State == domain.AliasDeletionWorkWaiting })
	appendedID, duplicateID := "appended-job-00000001", "duplicate-job-0000001"
	submitTestDeletionJob(t, env, cookie, csrf, appendedID, second.ID)
	duplicate := submitTestDeletionJob(t, env, cookie, csrf, duplicateID, first.ID)
	if duplicate.Pending != 1 {
		t.Fatalf("duplicate lost waiting item: %#v", duplicate)
	}
	cancelled := env.request(t, http.MethodPost, "/admin/api/v1/aliases/batch/jobs/"+testAliasDeletionJobID+"/cancel", nil, "", []*http.Cookie{cookie}, csrf)
	result := decodeTestAliasDeletionJob(t, cancelled)
	if cancelled.Code != http.StatusOK || result.Cancelled != 1 || result.Failed != 0 || result.Processed != 1 {
		t.Fatalf("cancel=%d %#v", cancelled.Code, result)
	}
	existing, err := env.store.GetAliasDeletionJob(context.Background(), duplicateID, admin.ID)
	if err != nil || existing.Items[0].WorkID != waiting.Items[0].WorkID || existing.Items[0].Done {
		t.Fatalf("shared work lost: %#v err=%v", existing, err)
	}
	env.server.aliasDeletionJobs.mu.Lock()
	active := len(env.server.aliasDeletionJobs.active)
	env.server.aliasDeletionJobs.mu.Unlock()
	if active != 0 {
		t.Fatal("waiting quota retained an execution slot")
	}
	release, ok := env.server.beginAliasDeletionCredentialRotation()
	if !ok {
		t.Fatal("quota wait prevented rotation")
	}
	release()
	clockOffset.Store(int64(time.Hour + time.Second))
	env.server.wakeAliasDeletionQueue()
	final := waitTestAliasDeletionJob(t, env, admin.ID, duplicateID)
	waitTestAliasDeletionJob(t, env, admin.ID, appendedID)
	callsMu.Lock()
	defer callsMu.Unlock()
	if !final.Items[0].Deleted || fmt.Sprint(calls) != fmt.Sprint([]int64{first.ID, first.ID, second.ID}) {
		t.Fatalf("FIFO/shared calls=%v job=%#v", calls, final)
	}
}

func TestAliasDeletionQueueCancelledQuotaWaitFinishesImmediately(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, admin := env.createSession(t, "queue-cancel-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "queue-cancel@icloud.com")
	alias := adminAPITestCreateDeleteAlias(t, env, account.ID, "queue-cancel-alias@icloud.com")
	env.server.SetHMESyncService(newQueuedDeletionTestService(env, func(context.Context, domain.AliasDeletionWork) error {
		return &hmesync.AliasDeletionWaitError{Reason: "quota", RetryAt: time.Now().Add(time.Hour), Used: 200, Limit: 200}
	}))
	startTestAliasDeletionJobs(t, env)
	submitTestDeletionJob(t, env, cookie, csrf, testAliasDeletionJobID, alias.ID)
	waitTestAliasDeletionState(t, env, admin.ID, testAliasDeletionJobID, func(job domain.AliasDeletionJob) bool { return job.Items[0].State == domain.AliasDeletionWorkWaiting })
	response := env.request(t, http.MethodPost, "/admin/api/v1/aliases/batch/jobs/"+testAliasDeletionJobID+"/cancel", nil, "", []*http.Cookie{cookie}, csrf)
	result := decodeTestAliasDeletionJob(t, response)
	if result.Status != domain.AliasDeletionJobCompleted || result.Cancelled != 1 || result.Pending != 0 || result.Failed != 0 {
		t.Fatalf("cancelled quota wait retained queue: %#v", result)
	}
}

func TestAliasDeletionQueueRestartReconcilesMissingLocalAlias(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, admin := env.createSession(t, "queue-restart-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "queue-restart@icloud.com")
	alias := adminAPITestCreateDeleteAlias(t, env, account.ID, "queue-restart-alias@icloud.com")
	entered := make(chan struct{})
	var calls atomic.Int32
	var reconciled atomic.Bool
	env.server.SetHMESyncService(newQueuedDeletionTestService(env, func(ctx context.Context, work domain.AliasDeletionWork) error {
		if calls.Add(1) == 1 {
			if err := env.store.DeleteAlias(ctx, work.AliasID); err != nil {
				return err
			}
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		}
		reconciled.Store(work.Reconcile && work.Address == alias.Address)
		return nil
	}))
	stop, stopped := startTestAliasDeletionJobs(t, env)
	submitTestDeletionJob(t, env, cookie, csrf, testAliasDeletionJobID, alias.ID)
	waitTestAliasDeletionSignal(t, entered)
	stop()
	waitTestAliasDeletionSignal(t, stopped)
	pending, err := env.store.GetAliasDeletionJob(context.Background(), testAliasDeletionJobID, admin.ID)
	if err != nil || pending.Items[0].Done || pending.Status == domain.AliasDeletionJobInterrupted {
		t.Fatalf("shutdown discarded durable queue: %#v %v", pending, err)
	}
	env.server.aliasDeletionJobs = aliasDeletionJobRuntime{}
	startTestAliasDeletionJobs(t, env)
	job := waitTestAliasDeletionJob(t, env, admin.ID, testAliasDeletionJobID)
	if !job.Items[0].Deleted || !reconciled.Load() || calls.Load() != 2 {
		t.Fatalf("restart failed to reconcile: %#v calls=%d", job, calls.Load())
	}
}

func TestAliasDeletionQueueCancelledRecoveredWorkStillReconciles(t *testing.T) {
	env := newAdminAPITestEnv(t)
	_, _, admin := env.createSession(t, "queue-reconcile-cancel-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "reconcile-cancel@icloud.com")
	alias := adminAPITestCreateDeleteAlias(t, env, account.ID, "reconcile-cancel-alias@icloud.com")
	var checked atomic.Bool
	service := newQueuedDeletionTestService(env, func(ctx context.Context, work domain.AliasDeletionWork) error {
		wanted, err := env.store.AliasDeletionWorkWanted(ctx, work.ID)
		if err != nil {
			return err
		}
		checked.Store(work.Reconcile && !wanted)
		return nil // Directory confirmed an already deleted remote alias.
	})
	env.server.SetHMESyncService(service)
	target, err := service.PrepareAliasDeletion(context.Background(), alias.ID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := env.store.EnqueueAliasDeletionJob(context.Background(), domain.AliasDeletionJob{ID: testAliasDeletionJobID, AdminID: admin.ID, QueueVersion: 1, AuthorizingPasswordVersion: admin.PasswordVersion, Username: admin.Username, Items: []domain.AliasDeletionJobItem{{ID: alias.ID, Address: alias.Address}}}, []domain.AliasDeletionWork{target})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.ClaimAliasDeletionWork(context.Background(), job.Items[0].WorkID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.CancelAliasDeletionJob(context.Background(), job.ID, admin.ID); err != nil {
		t.Fatal(err)
	}
	startTestAliasDeletionJobs(t, env)
	final := waitTestAliasDeletionJob(t, env, admin.ID, testAliasDeletionJobID)
	if !checked.Load() || !final.Items[0].Deleted {
		t.Fatalf("cancel skipped required reconciliation: %#v checked=%v", final, checked.Load())
	}
}

func TestAliasDeletionQueueProgressFailureStopsSubject(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, admin := env.createSession(t, "queue-save-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "queue-save@icloud.com")
	first := adminAPITestCreateDeleteAlias(t, env, account.ID, "queue-save-first@icloud.com")
	second := adminAPITestCreateDeleteAlias(t, env, account.ID, "queue-save-second@icloud.com")
	var calls atomic.Int32
	env.server.SetHMESyncService(newQueuedDeletionTestService(env, func(ctx context.Context, work domain.AliasDeletionWork) error {
		calls.Add(1)
		return env.store.DeleteAlias(ctx, work.AliasID)
	}))
	startTestAliasDeletionJobs(t, env)
	if _, err := env.store.DB().Exec("CREATE TRIGGER fail_deletion_job_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(ABORT, 'test write failure'); END"); err != nil {
		t.Fatal(err)
	}
	submitTestDeletionJob(t, env, cookie, csrf, testAliasDeletionJobID, first.ID, second.ID)
	deadline := time.Now().Add(5 * time.Second)
	for {
		env.server.aliasDeletionJobs.mu.Lock()
		blocked := len(env.server.aliasDeletionJobs.blocked)
		env.server.aliasDeletionJobs.mu.Unlock()
		if blocked == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("save failure did not stop subject")
		}
		time.Sleep(5 * time.Millisecond)
	}
	job, err := env.store.GetAliasDeletionJob(context.Background(), testAliasDeletionJobID, admin.ID)
	if err != nil || job.Items[0].Done || job.Items[1].Done || calls.Load() != 1 {
		t.Fatalf("uncommitted progress published or next item executed: %#v calls=%d err=%v", job, calls.Load(), err)
	}
	if _, err := env.store.GetAlias(context.Background(), second.ID); err != nil {
		t.Fatal("next item was deleted after failed checkpoint")
	}
}

func TestAliasDeletionJobStartupInterruptsLegacyWithoutReplaying(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, _, admin := env.createSession(t, "legacy-restart-admin", "unused-password")
	job := domain.AliasDeletionJob{ID: testAliasDeletionJobID, AdminID: admin.ID, Status: domain.AliasDeletionJobRunning, Items: []domain.AliasDeletionJobItem{{ID: 1, Address: "completed@icloud.com", Done: true, Deleted: true}, {ID: 2, Address: "unknown@icloud.com"}}}
	if err := env.store.CreateAliasDeletionJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	startTestAliasDeletionJobs(t, env)
	response := env.request(t, http.MethodGet, "/admin/api/v1/aliases/batch/jobs/latest", nil, "", []*http.Cookie{cookie}, "")
	result := decodeTestAliasDeletionJob(t, response)
	if result.Status != domain.AliasDeletionJobInterrupted || result.Deleted != 1 || result.Processed != 1 || result.Results[1].LocalRetained {
		t.Fatalf("legacy restart=%#v", result)
	}
}

func TestAliasDeletionJobListAndCancelAreOwnerScopedAndCSRFProtected(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, admin := env.createSession(t, "queue-owner-admin", "unused-password")
	other, otherCSRF, _ := env.createSession(t, "queue-other-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "queue-owner@icloud.com")
	alias := adminAPITestCreateDeleteAlias(t, env, account.ID, "queue-owner-alias@icloud.com")
	env.server.SetHMESyncService(newQueuedDeletionTestService(env, func(context.Context, domain.AliasDeletionWork) error {
		return &hmesync.AliasDeletionWaitError{Reason: "quota", RetryAt: time.Now().Add(time.Hour), Used: 200, Limit: 200}
	}))
	startTestAliasDeletionJobs(t, env)
	body := adminAPITestJSON(t, map[string]any{"alias_ids": []int64{alias.ID}, "operation_id": testAliasDeletionJobID})
	missing := env.request(t, http.MethodDelete, "/admin/api/v1/aliases/batch?async=1", body, "application/json", []*http.Cookie{cookie}, "")
	if missing.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF admitted job=%d", missing.Code)
	}
	submitTestDeletionJob(t, env, cookie, csrf, testAliasDeletionJobID, alias.ID)
	waitTestAliasDeletionState(t, env, admin.ID, testAliasDeletionJobID, func(job domain.AliasDeletionJob) bool { return job.Items[0].State == domain.AliasDeletionWorkWaiting })
	for _, test := range []struct {
		cookie *http.Cookie
		csrf   string
		want   int
	}{{other, otherCSRF, http.StatusNotFound}, {cookie, "", http.StatusForbidden}} {
		response := env.request(t, http.MethodPost, "/admin/api/v1/aliases/batch/jobs/"+testAliasDeletionJobID+"/cancel", nil, "", []*http.Cookie{test.cookie}, test.csrf)
		if response.Code != test.want {
			t.Fatalf("cancel scope/CSRF=%d want=%d", response.Code, test.want)
		}
	}
	list := env.request(t, http.MethodGet, "/admin/api/v1/aliases/batch/jobs", nil, "", []*http.Cookie{cookie}, "")
	var bodyList struct {
		Data struct{ Jobs []adminAPIAliasDeletionJobDTO }
	}
	if err := json.Unmarshal(list.Body.Bytes(), &bodyList); err != nil {
		t.Fatal(err)
	}
	if list.Code != http.StatusOK || len(bodyList.Data.Jobs) != 1 || bodyList.Data.Jobs[0].Pending != 1 {
		t.Fatalf("list=%d %s", list.Code, list.Body.String())
	}
	otherList := env.request(t, http.MethodGet, "/admin/api/v1/aliases/batch/jobs", nil, "", []*http.Cookie{other}, "")
	if strings.Contains(otherList.Body.String(), testAliasDeletionJobID) {
		t.Fatal("list leaked another owner's job")
	}
}

func TestAliasDeletionJobConcurrentSameKeyReturnsOneJob(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, admin := env.createSession(t, "same-key-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "same-key@icloud.com")
	alias := adminAPITestCreateDeleteAlias(t, env, account.ID, "same-key-alias@icloud.com")
	var calls atomic.Int32
	env.server.SetHMESyncService(newQueuedDeletionTestService(env, func(ctx context.Context, work domain.AliasDeletionWork) error {
		calls.Add(1)
		return env.store.DeleteAlias(ctx, work.AliasID)
	}))
	startTestAliasDeletionJobs(t, env)
	body := adminAPITestJSON(t, map[string]any{"alias_ids": []int64{alias.ID}, "operation_id": testAliasDeletionJobID})
	const count = 8
	responses := make(chan *httptest.ResponseRecorder, count)
	for range count {
		go func() {
			responses <- env.request(t, http.MethodDelete, "/admin/api/v1/aliases/batch?async=1", body, "application/json", []*http.Cookie{cookie}, csrf)
		}()
	}
	for range count {
		select {
		case response := <-responses:
			if response.Code != http.StatusAccepted {
				t.Fatalf("retry=%d %s", response.Code, response.Body.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent admission stalled")
		}
	}
	waitTestAliasDeletionJob(t, env, admin.ID, testAliasDeletionJobID)
	if calls.Load() != 1 {
		t.Fatalf("repeated idempotency key executed %d times", calls.Load())
	}
}

func TestAuditSurvivesCancelledHTTPRequest(t *testing.T) {
	env := newAdminAPITestEnv(t)
	_, _, admin := env.createSession(t, "audit-cancel-admin", "unused-password")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodDelete, "/admin/api/v1/aliases/1", nil).WithContext(ctx)
	c.Set(requestIDKey, "original-cancelled-request")
	env.server.audit(c, &admin.ID, admin.Username, "delete", "alias", "1", "success", "batch")
	audits, err := env.store.ListAuditLogs(context.Background(), 10, 0)
	if err != nil || len(audits) != 1 || audits[0].RequestID != "original-cancelled-request" {
		t.Fatalf("cancelled audit=%#v err=%v", audits, err)
	}
	apiErr := adminAPIBatchAliasDeleteError(context.Canceled)
	if apiErr.Code != "BATCH_DELETE_INTERRUPTED" || strings.Contains(apiErr.Message, "本地记录已保留") {
		t.Fatalf("cancel classification=%#v", apiErr)
	}
}

func TestAliasDeletionQuotaWaitDoesNotBlockAnotherAccount(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, admin := env.createSession(t, "quota-independent-admin", "unused-password")
	firstAccount := adminAPITestCreateAccount(t, env, "quota-wait-owner@icloud.com")
	secondAccount := adminAPITestCreateAccount(t, env, "quota-ready-owner@icloud.com")
	first := adminAPITestCreateDeleteAlias(t, env, firstAccount.ID, "quota-wait-alias@icloud.com")
	second := adminAPITestCreateDeleteAlias(t, env, secondAccount.ID, "quota-ready-alias@icloud.com")
	env.server.SetHMESyncService(newQueuedDeletionTestService(env, func(_ context.Context, work domain.AliasDeletionWork) error {
		if work.AccountID == firstAccount.ID {
			return &hmesync.AliasDeletionWaitError{Reason: "quota", RetryAt: time.Now().Add(time.Hour), Used: 200, Limit: 200}
		}
		return nil
	}))
	startTestAliasDeletionJobs(t, env)
	submitTestDeletionJob(t, env, cookie, csrf, testAliasDeletionJobID, first.ID)
	waitTestAliasDeletionState(t, env, admin.ID, testAliasDeletionJobID, func(job domain.AliasDeletionJob) bool { return job.Items[0].State == domain.AliasDeletionWorkWaiting })
	const readyJob = "quota-ready-job-000001"
	submitTestDeletionJob(t, env, cookie, csrf, readyJob, second.ID)
	ready := waitTestAliasDeletionJob(t, env, admin.ID, readyJob)
	waiting, err := env.store.GetAliasDeletionJob(context.Background(), testAliasDeletionJobID, admin.ID)
	if err != nil || !ready.Items[0].Deleted || waiting.Items[0].Done {
		t.Fatalf("waiting account blocked another: ready=%#v waiting=%#v err=%v", ready, waiting, err)
	}
	latest := env.request(t, http.MethodGet, "/admin/api/v1/aliases/batch/jobs/latest", nil, "", []*http.Cookie{cookie}, "")
	if result := decodeTestAliasDeletionJob(t, latest); result.JobID != testAliasDeletionJobID {
		t.Fatalf("latest hid active queue behind recent completion: %#v", result)
	}
}

func TestAliasDeletionQueueLoginPauseCancellationUsesMutationEvidence(t *testing.T) {
	for _, scenario := range []struct {
		name              string
		mutationAttempted bool
		priorReconcile    bool
	}{
		{name: "login missing before mutation"},
		{name: "mutation sent before login expired", mutationAttempted: true},
		{name: "earlier attempt still needs reconciliation", priorReconcile: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			env := newAdminAPITestEnv(t)
			cookie, csrf, admin := env.createSession(t, "login-pause-cancel-admin", "unused-password")
			account := adminAPITestCreateAccount(t, env, "login-pause-cancel@icloud.com")
			alias := adminAPITestCreateDeleteAlias(t, env, account.ID, "login-pause-cancel-alias@icloud.com")
			base := time.Now().UTC()
			var clockOffset atomic.Int64
			env.server.now = func() time.Time { return base.Add(time.Duration(clockOffset.Load())) }
			var calls atomic.Int32
			var confirmedReconciliation atomic.Bool
			service := newQueuedDeletionTestService(env, func(ctx context.Context, work domain.AliasDeletionWork) error {
				if calls.Add(1) == 1 {
					return &hmesync.AliasDeletionAttemptError{Err: hmesync.ErrLoginRequired, MutationAttempted: scenario.mutationAttempted}
				}
				wanted, err := env.store.AliasDeletionWorkWanted(ctx, work.ID)
				if err != nil {
					return err
				}
				confirmedReconciliation.Store(work.Reconcile && !wanted)
				return nil
			})
			env.server.SetHMESyncService(service)
			target, err := service.PrepareAliasDeletion(context.Background(), alias.ID)
			if err != nil {
				t.Fatal(err)
			}
			target.Reconcile = scenario.priorReconcile
			_, err = env.store.EnqueueAliasDeletionJob(context.Background(), domain.AliasDeletionJob{
				ID: testAliasDeletionJobID, AdminID: admin.ID, AuthorizingPasswordVersion: admin.PasswordVersion,
				Username: admin.Username, Items: []domain.AliasDeletionJobItem{{ID: alias.ID, Address: alias.Address}},
			}, []domain.AliasDeletionWork{target})
			if err != nil {
				t.Fatal(err)
			}
			startTestAliasDeletionJobs(t, env)
			waitTestAliasDeletionState(t, env, admin.ID, testAliasDeletionJobID, func(job domain.AliasDeletionJob) bool { return job.Items[0].State == domain.AliasDeletionWorkPaused })
			response := env.request(t, http.MethodPost, "/admin/api/v1/aliases/batch/jobs/"+testAliasDeletionJobID+"/cancel", nil, "", []*http.Cookie{cookie}, csrf)
			result := decodeTestAliasDeletionJob(t, response)
			if response.Code != http.StatusOK {
				t.Fatalf("cancel = %d %s", response.Code, response.Body.String())
			}
			if !scenario.mutationAttempted && !scenario.priorReconcile {
				if result.Status != domain.AliasDeletionJobCompleted || result.Cancelled != 1 || result.Pending != 0 || result.Processed != 1 || calls.Load() != 1 {
					t.Fatalf("login-only attempt retained a cancelled queue: %#v calls=%d", result, calls.Load())
				}
				return
			}
			if result.Pending != 1 || result.Cancelled != 0 || !aliasDeletionJobActive(result.Status) {
				t.Fatalf("possible remote mutation was cancelled without reconciliation: %#v", result)
			}
			clockOffset.Store(int64(time.Minute + time.Second))
			env.server.wakeAliasDeletionQueue()
			final := waitTestAliasDeletionJob(t, env, admin.ID, testAliasDeletionJobID)
			if !final.Items[0].Deleted || calls.Load() != 2 || !confirmedReconciliation.Load() {
				t.Fatalf("cancelled attempt lost reconciliation evidence: %#v calls=%d", final, calls.Load())
			}
		})
	}
}
