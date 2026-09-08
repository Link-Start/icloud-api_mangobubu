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
	"icloud-api/internal/secure"
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
	go func() {
		env.server.RunAliasDeletionJobs()
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("alias deletion workers did not finish before database cleanup")
		}
	})
	return cancel, done
}

func waitTestAliasDeletionSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for deletion test barrier")
	}
}

func waitTestAliasDeletionJob(t *testing.T, env *adminAPITestEnv, adminID int64, jobID string) domain.AliasDeletionJob {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		job, err := env.store.GetAliasDeletionJob(context.Background(), jobID, adminID)
		if err == nil && !aliasDeletionJobActive(job.Status) {
			return job
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		select {
		case <-timer.C:
			t.Fatal("background deletion job did not finish")
		case <-ticker.C:
		}
	}
}

func decodeTestAliasDeletionJob(t *testing.T, response *httptest.ResponseRecorder) adminAPIAliasDeletionJobDTO {
	t.Helper()
	var body struct {
		Data adminAPIAliasDeletionJobDTO `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Data
}

func TestAliasDeletionJobReturnsBeforeAppleAndSurvivesRequestCancellation(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, admin := env.createSession(t, "async-delete-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "async-owner@icloud.com")
	first := adminAPITestCreateDeleteAlias(t, env, account.ID, "async-first@icloud.com")
	second := adminAPITestCreateDeleteAlias(t, env, account.ID, "async-second@icloud.com")
	startTestAliasDeletionJobs(t, env)
	entered, proceed, firstReported, finish := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	env.server.SetHMESyncService(&fakeHMESyncService{
		getSession: func(context.Context, int64) (hmesync.SessionInfo, error) {
			return hmesync.SessionInfo{Status: hmesync.StatusAuthenticated}, nil
		},
		deleteAliases: func(ctx context.Context, ids []int64) ([]hmesync.AliasDeletionOutcome, error) {
			calls.Add(1)
			close(entered)
			select {
			case <-proceed:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if err := env.store.DeleteAlias(ctx, ids[0]); err != nil {
				return nil, err
			}
			hmesync.ReportAliasDeletionProgress(ctx, hmesync.AliasDeletionOutcome{AliasID: ids[0]})
			close(firstReported)
			select {
			case <-finish:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			hmesync.ReportAliasDeletionProgress(ctx, hmesync.AliasDeletionOutcome{AliasID: ids[1], Err: hmesync.ErrRateLimited})
			return []hmesync.AliasDeletionOutcome{{AliasID: ids[0]}, {AliasID: ids[1], Err: hmesync.ErrRateLimited}}, nil
		},
	})
	body := adminAPITestJSON(t, map[string]any{"alias_ids": []int64{first.ID, second.ID}, "operation_id": testAliasDeletionJobID})
	requestContext, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	request := httptest.NewRequest(http.MethodDelete, "http://admin.example.test/admin/api/v1/aliases/batch?async=1", bytes.NewReader(body)).WithContext(requestContext)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://admin.example.test")
	request.Header.Set(adminAPICSRFHeader, csrf)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	responseDone := make(chan struct{})
	go func() {
		env.router.ServeHTTP(response, request)
		close(responseDone)
	}()
	waitTestAliasDeletionSignal(t, responseDone)
	if response.Code != http.StatusAccepted {
		t.Fatalf("async admission = %d %s", response.Code, response.Body.String())
	}
	accepted := decodeTestAliasDeletionJob(t, response)
	if accepted.JobID != testAliasDeletionJobID || accepted.Requested != 2 || accepted.Processed != 0 {
		t.Fatalf("admitted job = %#v", accepted)
	}
	cancelRequest() // Simulate the original browser/gateway connection closing.
	waitTestAliasDeletionSignal(t, entered)
	close(proceed)
	waitTestAliasDeletionSignal(t, firstReported)
	progress := env.request(t, http.MethodGet, "/admin/api/v1/aliases/batch/jobs/"+testAliasDeletionJobID, nil, "", []*http.Cookie{cookie}, "")
	if progress.Code != http.StatusOK {
		t.Fatalf("progress = %d %s", progress.Code, progress.Body.String())
	}
	partial := decodeTestAliasDeletionJob(t, progress)
	if partial.Status != domain.AliasDeletionJobRunning || partial.Processed != 1 || partial.Deleted != 1 || partial.Failed != 0 || len(partial.Results) != 1 {
		t.Fatalf("partial progress was not persisted immediately: %#v", partial)
	}
	assertAdminAliasDeleteAudit(t, env.store, first.ID, "success", "batch")
	// Already deleted IDs must not make the same idempotency key fail preflight.
	retry := env.request(t, http.MethodDelete, "/admin/api/v1/aliases/batch?async=1", body, "application/json", []*http.Cookie{cookie}, csrf)
	if retry.Code != http.StatusAccepted || decodeTestAliasDeletionJob(t, retry).JobID != testAliasDeletionJobID || calls.Load() != 1 {
		t.Fatalf("idempotent retry = %d %s calls=%d", retry.Code, retry.Body.String(), calls.Load())
	}
	conflict := env.request(t, http.MethodDelete, "/admin/api/v1/aliases/batch?async=1", adminAPITestJSON(t, map[string]any{
		"alias_ids": []int64{second.ID}, "operation_id": testAliasDeletionJobID,
	}), "application/json", []*http.Cookie{cookie}, csrf)
	if conflict.Code != http.StatusConflict || adminAPITestErrorCode(t, conflict) != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("changed idempotency payload = %d %s", conflict.Code, conflict.Body.String())
	}
	duplicate := env.request(t, http.MethodDelete, "/admin/api/v1/aliases/batch?async=1", adminAPITestJSON(t, map[string]any{
		"alias_ids": []int64{first.ID, second.ID}, "operation_id": "another-alias-delete-operation",
	}), "application/json", []*http.Cookie{cookie}, csrf)
	if duplicate.Code != http.StatusConflict || adminAPITestErrorCode(t, duplicate) != "BATCH_DELETE_IN_PROGRESS" {
		t.Fatalf("active job duplicate = %d %s", duplicate.Code, duplicate.Body.String())
	}
	close(finish)
	job := waitTestAliasDeletionJob(t, env, admin.ID, testAliasDeletionJobID)
	result := adminAPIAliasDeletionJobFromRecord(job)
	if result.Status != domain.AliasDeletionJobCompleted || result.Processed != 2 || result.Deleted != 1 || result.Failed != 1 || !result.Results[1].LocalRetained {
		t.Fatalf("completion = %#v", result)
	}
	assertAdminAliasDeleteAudit(t, env.store, second.ID, "failed", hmesync.CodeRateLimited)
	audits, err := env.store.ListAuditLogsFiltered(context.Background(), store.AuditLogFilter{AdminID: &admin.ID, Action: "delete", ResourceType: "alias"})
	if err != nil || len(audits) != 2 {
		t.Fatalf("duplicate callback/final fold duplicated audit: count=%d err=%v", len(audits), err)
	}
	for _, audit := range audits {
		if audit.RequestID != result.RequestID {
			t.Error("original request ID was lost from background audit")
		}
	}
	latest := env.request(t, http.MethodGet, "/admin/api/v1/aliases/batch/jobs/latest", nil, "", []*http.Cookie{cookie}, "")
	if latest.Code != http.StatusOK || decodeTestAliasDeletionJob(t, latest).Deleted != 1 {
		t.Fatalf("refresh did not recover saved completion: %s", latest.Body.String())
	}
	otherCookie, _, _ := env.createSession(t, "other-async-admin", "unused-password")
	private := env.request(t, http.MethodGet, "/admin/api/v1/aliases/batch/jobs/"+testAliasDeletionJobID, nil, "", []*http.Cookie{otherCookie}, "")
	if private.Code != http.StatusNotFound {
		t.Fatalf("another administrator read job: %d %s", private.Code, private.Body.String())
	}
}

func TestAliasDeletionJobShutdownPersistsConfirmedResults(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, admin := env.createSession(t, "async-stop-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "async-stop@icloud.com")
	first := adminAPITestCreateDeleteAlias(t, env, account.ID, "async-stop-first@icloud.com")
	second := adminAPITestCreateDeleteAlias(t, env, account.ID, "async-stop-second@icloud.com")
	stop, done := startTestAliasDeletionJobs(t, env)
	entered := make(chan struct{})
	env.server.SetHMESyncService(&fakeHMESyncService{
		getSession: func(context.Context, int64) (hmesync.SessionInfo, error) {
			return hmesync.SessionInfo{Status: hmesync.StatusAuthenticated}, nil
		},
		deleteAliases: func(ctx context.Context, ids []int64) ([]hmesync.AliasDeletionOutcome, error) {
			close(entered)
			<-ctx.Done()
			// A confirmed in-flight operation can finish its local persistence
			// after shutdown cancellation. Its audit must still be saved.
			if err := env.store.DeleteAlias(context.Background(), ids[0]); err != nil {
				return nil, err
			}
			hmesync.ReportAliasDeletionProgress(ctx, hmesync.AliasDeletionOutcome{AliasID: ids[0]})
			return []hmesync.AliasDeletionOutcome{{AliasID: ids[0]}}, ctx.Err()
		},
	})
	response := env.request(t, http.MethodDelete, "/admin/api/v1/aliases/batch?async=1", adminAPITestJSON(t, map[string]any{
		"alias_ids": []int64{first.ID, second.ID}, "operation_id": testAliasDeletionJobID,
	}), "application/json", []*http.Cookie{cookie}, csrf)
	if response.Code != http.StatusAccepted {
		t.Fatalf("admission = %d %s", response.Code, response.Body.String())
	}
	waitTestAliasDeletionSignal(t, entered)
	if endRotation, ok := env.server.beginAliasDeletionCredentialRotation(); ok {
		endRotation()
		t.Error("credential rotation was admitted during deletion")
	}
	stop()
	waitTestAliasDeletionSignal(t, done)
	job := waitTestAliasDeletionJob(t, env, admin.ID, testAliasDeletionJobID)
	result := adminAPIAliasDeletionJobFromRecord(job)
	if result.Status != domain.AliasDeletionJobInterrupted || result.Deleted != 1 || result.Processed != 1 || result.Failed != 1 || result.Results[1].LocalRetained || result.Results[1].Code != "BATCH_DELETE_INTERRUPTED" {
		t.Fatalf("shutdown result = %#v", result)
	}
	assertAdminAliasDeleteAudit(t, env.store, first.ID, "success", "batch")
	if _, err := env.store.GetAlias(context.Background(), second.ID); err != nil {
		t.Fatalf("shutdown removed unprocessed alias: %v", err)
	}
	if endRotation, ok := env.server.beginAliasDeletionCredentialRotation(); !ok {
		t.Error("stopped worker retained active reservation")
	} else {
		endRotation()
	}
	rejected := env.request(t, http.MethodDelete, "/admin/api/v1/aliases/batch?async=1", adminAPITestJSON(t, map[string]any{
		"alias_ids": []int64{second.ID}, "operation_id": "post-stop-deletion-operation",
	}), "application/json", []*http.Cookie{cookie}, csrf)
	if rejected.Code != http.StatusServiceUnavailable || adminAPITestErrorCode(t, rejected) != "BATCH_DELETE_UNAVAILABLE" {
		t.Fatalf("admission after shutdown = %d %s", rejected.Code, rejected.Body.String())
	}
}

func TestAliasDeletionJobAdmissionSurvivesDisconnectAfterPreflight(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, admin := env.createSession(t, "async-admission-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "async-admission@icloud.com")
	alias := adminAPITestCreateDeleteAlias(t, env, account.ID, "async-admission-alias@icloud.com")
	startTestAliasDeletionJobs(t, env)
	ctx, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	env.server.SetHMESyncService(&fakeHMESyncService{
		getSession: func(context.Context, int64) (hmesync.SessionInfo, error) {
			disconnect() // All eligibility reads finished; admission is next.
			return hmesync.SessionInfo{Status: hmesync.StatusAuthenticated}, nil
		},
		deleteAliases: func(workerContext context.Context, ids []int64) ([]hmesync.AliasDeletionOutcome, error) {
			if workerContext.Err() != nil {
				return nil, workerContext.Err()
			}
			return []hmesync.AliasDeletionOutcome{{AliasID: ids[0], Err: hmesync.ErrRateLimited}}, nil
		},
	})
	body := adminAPITestJSON(t, map[string]any{"alias_ids": []int64{alias.ID}, "operation_id": testAliasDeletionJobID})
	request := httptest.NewRequest(http.MethodDelete, "http://admin.example.test/admin/api/v1/aliases/batch?async=1", bytes.NewReader(body)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://admin.example.test")
	request.Header.Set(adminAPICSRFHeader, csrf)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	env.router.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("disconnect lost admission: %d %s", response.Code, response.Body.String())
	}
	job := waitTestAliasDeletionJob(t, env, admin.ID, testAliasDeletionJobID)
	if job.Status != domain.AliasDeletionJobCompleted || !job.Items[0].Done || job.Items[0].Code != hmesync.CodeRateLimited {
		t.Fatalf("admitted job lost its worker: %#v", job)
	}
}

func TestAliasDeletionJobProgressFailureStopsFurtherWork(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, admin := env.createSession(t, "async-persist-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "async-persist@icloud.com")
	first := adminAPITestCreateDeleteAlias(t, env, account.ID, "async-persist-first@icloud.com")
	second := adminAPITestCreateDeleteAlias(t, env, account.ID, "async-persist-second@icloud.com")
	startTestAliasDeletionJobs(t, env)
	_, err := env.store.DB().Exec(`CREATE TRIGGER fail_deletion_job_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(ABORT, 'test write failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	var cancelled atomic.Bool
	env.server.SetHMESyncService(&fakeHMESyncService{
		getSession: func(context.Context, int64) (hmesync.SessionInfo, error) {
			return hmesync.SessionInfo{Status: hmesync.StatusAuthenticated}, nil
		},
		deleteAliases: func(ctx context.Context, ids []int64) ([]hmesync.AliasDeletionOutcome, error) {
			if err := env.store.DeleteAlias(ctx, ids[0]); err != nil {
				return nil, err
			}
			hmesync.ReportAliasDeletionProgress(ctx, hmesync.AliasDeletionOutcome{AliasID: ids[0]})
			cancelled.Store(ctx.Err() != nil)
			return []hmesync.AliasDeletionOutcome{{AliasID: ids[0]}}, ctx.Err()
		},
	})
	response := env.request(t, http.MethodDelete, "/admin/api/v1/aliases/batch?async=1", adminAPITestJSON(t, map[string]any{
		"alias_ids": []int64{first.ID, second.ID}, "operation_id": testAliasDeletionJobID,
	}), "application/json", []*http.Cookie{cookie}, csrf)
	if response.Code != http.StatusAccepted {
		t.Fatalf("admission = %d %s", response.Code, response.Body.String())
	}
	job := waitTestAliasDeletionJob(t, env, admin.ID, testAliasDeletionJobID)
	if !cancelled.Load() || job.Status != domain.AliasDeletionJobInterrupted || job.Items[0].Done || job.Items[1].Done {
		t.Fatalf("uncommitted progress was published or next item continued: %#v cancelled=%v", job, cancelled.Load())
	}
	if _, err := env.store.GetAlias(context.Background(), second.ID); err != nil {
		t.Fatal("subsequent local alias lost after audit failure")
	}
}

func TestAliasDeletionJobStartupInterruptsWithoutReplaying(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, _, admin := env.createSession(t, "async-restart-admin", "unused-password")
	job := domain.AliasDeletionJob{
		ID: testAliasDeletionJobID, AdminID: admin.ID, Status: domain.AliasDeletionJobRunning,
		Items: []domain.AliasDeletionJobItem{
			{ID: 1, Address: "completed@icloud.com", Done: true, Deleted: true},
			{ID: 2, Address: "unknown@icloud.com"},
		},
	}
	if err := env.store.CreateAliasDeletionJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	startTestAliasDeletionJobs(t, env)
	response := env.request(t, http.MethodGet, "/admin/api/v1/aliases/batch/jobs/latest", nil, "", []*http.Cookie{cookie}, "")
	result := decodeTestAliasDeletionJob(t, response)
	if response.Code != http.StatusOK || result.Status != domain.AliasDeletionJobInterrupted || result.Deleted != 1 || result.Processed != 1 || result.Results[1].LocalRetained {
		t.Fatalf("restart result = %d %#v", response.Code, result)
	}
}

func TestAliasDeletionJobRechecksSessionAfterWaitingForRotationLock(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, _, admin := env.createSession(t, "async-revoked-admin", "unused-password")
	adminSession, err := env.store.GetSessionByHash(context.Background(), secure.HashToken(cookie.Value))
	if err != nil {
		t.Fatal(err)
	}
	job := domain.AliasDeletionJob{ID: testAliasDeletionJobID, AdminID: admin.ID, Status: domain.AliasDeletionJobQueued, Items: []domain.AliasDeletionJobItem{{ID: 1, Address: "revoked@icloud.com"}}}
	if err := env.store.CreateAliasDeletionJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	env.server.SetHMESyncService(&fakeHMESyncService{deleteAliases: func(context.Context, []int64) ([]hmesync.AliasDeletionOutcome, error) {
		calls.Add(1)
		return nil, errors.New("unexpected execution")
	}})
	env.server.credentialRotationMu.Lock()
	done := make(chan struct{})
	go func() {
		env.server.runAliasDeletionJob(context.Background(), job, adminSession, secure.HashToken(cookie.Value), "127.0.0.1")
		close(done)
	}()
	err = env.store.ChangeAdminPasswordAndRevokeSessions(context.Background(), admin.ID, admin.PasswordVersion, admin.PasswordHash)
	env.server.credentialRotationMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	waitTestAliasDeletionSignal(t, done)
	interrupted := waitTestAliasDeletionJob(t, env, admin.ID, testAliasDeletionJobID)
	if calls.Load() != 0 || interrupted.Status != domain.AliasDeletionJobInterrupted {
		t.Error("revoked queued session reached Apple")
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
		t.Fatalf("cancelled audit = %#v err=%v", audits, err)
	}
	apiErr := adminAPIBatchAliasDeleteError(context.Canceled)
	if apiErr.Code != "BATCH_DELETE_INTERRUPTED" || strings.Contains(apiErr.Message, "本地记录已保留") {
		t.Fatalf("cancellation misclassified = %#v", apiErr)
	}
}

func TestAliasDeletionJobDeadlineDoesNotMasqueradeAsAppleFailure(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, _, admin := env.createSession(t, "async-deadline-admin", "unused-password")
	adminSession, err := env.store.GetSessionByHash(context.Background(), secure.HashToken(cookie.Value))
	if err != nil {
		t.Fatal(err)
	}
	job := domain.AliasDeletionJob{
		ID: testAliasDeletionJobID, AdminID: admin.ID, Status: domain.AliasDeletionJobQueued,
		Items: []domain.AliasDeletionJobItem{{ID: 1, Address: "deadline@icloud.com"}},
	}
	if err := env.store.CreateAliasDeletionJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	env.server.SetHMESyncService(&fakeHMESyncService{deleteAliases: func(ctx context.Context, ids []int64) ([]hmesync.AliasDeletionOutcome, error) {
		<-ctx.Done()
		outcome := hmesync.AliasDeletionOutcome{AliasID: ids[0], Err: ctx.Err()}
		hmesync.ReportAliasDeletionProgress(ctx, outcome)
		return []hmesync.AliasDeletionOutcome{outcome}, nil
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	env.server.runAliasDeletionJob(ctx, job, adminSession, secure.HashToken(cookie.Value), "127.0.0.1")
	result := waitTestAliasDeletionJob(t, env, admin.ID, testAliasDeletionJobID)
	if result.Status != domain.AliasDeletionJobInterrupted || result.Items[0].Code != "BATCH_DELETE_INTERRUPTED" || result.Items[0].LocalRetained {
		t.Fatalf("job deadline was reported as an Apple failure: %#v", result)
	}
}

func TestAliasDeletionJobAdmissionLimitsAndCSRF(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, _ := env.createSession(t, "async-limits-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "async-limits@icloud.com")
	alias := adminAPITestCreateDeleteAlias(t, env, account.ID, "async-limits-alias@icloud.com")
	startTestAliasDeletionJobs(t, env)
	env.server.SetHMESyncService(&fakeHMESyncService{getSession: func(context.Context, int64) (hmesync.SessionInfo, error) {
		return hmesync.SessionInfo{Status: hmesync.StatusAuthenticated}, nil
	}})
	body := adminAPITestJSON(t, map[string]any{"alias_ids": []int64{alias.ID}, "operation_id": testAliasDeletionJobID})
	csrfFailure := env.request(t, http.MethodDelete, "/admin/api/v1/aliases/batch?async=1", body, "application/json", []*http.Cookie{cookie}, "")
	if csrfFailure.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF admitted job: %d", csrfFailure.Code)
	}
	runtime := &env.server.aliasDeletionJobs
	runtime.mu.Lock()
	for i := range aliasDeletionJobLimit {
		runtime.active[aliasDeletionJobKey{adminID: int64(i + 1), id: fmt.Sprint(i)}] = struct{}{}
	}
	runtime.mu.Unlock()
	busy := env.request(t, http.MethodDelete, "/admin/api/v1/aliases/batch?async=1", body, "application/json", []*http.Cookie{cookie}, csrf)
	if busy.Code != http.StatusTooManyRequests || adminAPITestErrorCode(t, busy) != "BATCH_DELETE_BUSY" {
		t.Fatalf("capacity response = %d %s", busy.Code, busy.Body.String())
	}
	runtime.mu.Lock()
	clear(runtime.active)
	runtime.mu.Unlock()
	endRotation, ok := env.server.beginAliasDeletionCredentialRotation()
	if !ok {
		t.Fatal("could not reserve rotation")
	}
	defer endRotation()
	rotationBusy := env.request(t, http.MethodDelete, "/admin/api/v1/aliases/batch?async=1", body, "application/json", []*http.Cookie{cookie}, csrf)
	if rotationBusy.Code != http.StatusConflict || adminAPITestErrorCode(t, rotationBusy) != "BATCH_DELETE_IN_PROGRESS" {
		t.Fatalf("rotation response = %d %s", rotationBusy.Code, rotationBusy.Body.String())
	}
}

func TestAliasDeletionJobConcurrentSameKeyReturnsOneJob(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, admin := env.createSession(t, "same-key-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "same-key@icloud.com")
	alias := adminAPITestCreateDeleteAlias(t, env, account.ID, "same-key-alias@icloud.com")
	startTestAliasDeletionJobs(t, env)
	preflight, proceed := make(chan struct{}), make(chan struct{})
	var signalOnce sync.Once
	var calls atomic.Int32
	env.server.SetHMESyncService(&fakeHMESyncService{
		getSession: func(ctx context.Context, _ int64) (hmesync.SessionInfo, error) {
			signalOnce.Do(func() { close(preflight) })
			select {
			case <-proceed:
				return hmesync.SessionInfo{Status: hmesync.StatusAuthenticated}, nil
			case <-ctx.Done():
				return hmesync.SessionInfo{}, ctx.Err()
			}
		},
		deleteAliases: func(ctx context.Context, ids []int64) ([]hmesync.AliasDeletionOutcome, error) {
			calls.Add(1)
			if err := env.store.DeleteAlias(ctx, ids[0]); err != nil {
				return nil, err
			}
			return []hmesync.AliasDeletionOutcome{{AliasID: ids[0]}}, nil
		},
	})
	body := adminAPITestJSON(t, map[string]any{"alias_ids": []int64{alias.ID}, "operation_id": testAliasDeletionJobID})
	const submissions = 8
	responses := make(chan *httptest.ResponseRecorder, submissions)
	var started sync.WaitGroup
	started.Add(submissions)
	for range submissions {
		go func() {
			started.Done()
			responses <- env.request(t, http.MethodDelete, "/admin/api/v1/aliases/batch?async=1", body, "application/json", []*http.Cookie{cookie}, csrf)
		}()
	}
	started.Wait()
	waitTestAliasDeletionSignal(t, preflight)
	close(proceed)
	for range submissions {
		select {
		case response := <-responses:
			if response.Code != http.StatusAccepted || decodeTestAliasDeletionJob(t, response).JobID != testAliasDeletionJobID {
				t.Errorf("same-key concurrent response = %d %s", response.Code, response.Body.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent admission stalled")
		}
	}
	job := waitTestAliasDeletionJob(t, env, admin.ID, testAliasDeletionJobID)
	if calls.Load() != 1 || job.Status != domain.AliasDeletionJobCompleted || !job.Items[0].Deleted {
		t.Fatalf("concurrent retry executed %d times, job=%#v", calls.Load(), job)
	}
}

func TestAliasDeletionJobSameKeyIsIndependentAcrossAdministrators(t *testing.T) {
	env := newAdminAPITestEnv(t)
	firstCookie, firstCSRF, firstAdmin := env.createSession(t, "shared-key-first-admin", "unused-password")
	secondCookie, secondCSRF, secondAdmin := env.createSession(t, "shared-key-second-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "shared-key@icloud.com")
	first := adminAPITestCreateDeleteAlias(t, env, account.ID, "shared-key-first@icloud.com")
	second := adminAPITestCreateDeleteAlias(t, env, account.ID, "shared-key-second@icloud.com")
	stop, stopped := startTestAliasDeletionJobs(t, env)
	started := make(chan int64, 2)
	finish := make(chan struct{})
	env.server.SetHMESyncService(&fakeHMESyncService{
		getSession: func(context.Context, int64) (hmesync.SessionInfo, error) {
			return hmesync.SessionInfo{Status: hmesync.StatusAuthenticated}, nil
		},
		deleteAliases: func(ctx context.Context, ids []int64) ([]hmesync.AliasDeletionOutcome, error) {
			started <- ids[0]
			select {
			case <-finish:
				return []hmesync.AliasDeletionOutcome{{AliasID: ids[0], Err: hmesync.ErrRateLimited}}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	})
	for _, input := range []struct {
		cookie *http.Cookie
		csrf   string
		id     int64
	}{{firstCookie, firstCSRF, first.ID}, {secondCookie, secondCSRF, second.ID}} {
		response := env.request(t, http.MethodDelete, "/admin/api/v1/aliases/batch?async=1", adminAPITestJSON(t, map[string]any{
			"alias_ids": []int64{input.id}, "operation_id": testAliasDeletionJobID,
		}), "application/json", []*http.Cookie{input.cookie}, input.csrf)
		if response.Code != http.StatusAccepted {
			t.Fatalf("owner-scoped key admission = %d %s", response.Code, response.Body.String())
		}
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("owner's task did not start")
		}
	}
	runtime := &env.server.aliasDeletionJobs
	runtime.mu.Lock()
	active := len(runtime.active)
	runtime.mu.Unlock()
	if active != 2 {
		t.Fatalf("same key collapsed runtime ownership: active=%d", active)
	}
	close(finish)
	firstJob := waitTestAliasDeletionJob(t, env, firstAdmin.ID, testAliasDeletionJobID)
	secondJob := waitTestAliasDeletionJob(t, env, secondAdmin.ID, testAliasDeletionJobID)
	if firstJob.Items[0].ID != first.ID || secondJob.Items[0].ID != second.ID || firstJob.Status != domain.AliasDeletionJobCompleted || secondJob.Status != domain.AliasDeletionJobCompleted {
		t.Error("same key crossed administrator identity or results")
	}
	stop()
	waitTestAliasDeletionSignal(t, stopped)
	runtime.mu.Lock()
	active = len(runtime.active)
	runtime.mu.Unlock()
	if active != 0 {
		t.Error("owner-scoped runtime key leaked after completion")
	}
}
