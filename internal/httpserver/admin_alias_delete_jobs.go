package httpserver

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/hmesync"
	"icloud-api/internal/store"
)

const (
	aliasDeletionItemTimeout      = 5 * time.Minute
	aliasDeletionSaveTimeout      = 5 * time.Second
	aliasDeletionAdmissionTimeout = 2 * time.Minute
)

type aliasDeletionJobRuntime struct {
	mu              sync.Mutex
	ctx             context.Context
	active          map[string]struct{}
	blocked         map[string]struct{}
	wake            chan struct{}
	admission       chan struct{}
	stopping        bool
	rotationPending bool
	wg              sync.WaitGroup
}

type adminAPIAliasDeletionJobDTO struct {
	adminAPIAliasBatchDeleteDTO
	JobID           string                            `json:"job_id"`
	Status          string                            `json:"status"`
	Processed       int                               `json:"processed"`
	Pending         int                               `json:"pending"`
	Cancelled       int                               `json:"cancelled"`
	CancelRequested bool                              `json:"cancel_requested"`
	RequestID       string                            `json:"request_id"`
	CreatedAt       string                            `json:"created_at"`
	UpdatedAt       string                            `json:"updated_at"`
	Deferred        int                               `json:"deferred,omitempty"`
	Waits           []adminAPIAliasDeletionWaitDTO    `json:"waits,omitempty"`
	Accounts        []adminAPIAliasDeletionAccountDTO `json:"accounts"`
}

type adminAPIAliasDeletionAccountDTO struct {
	AccountID    int64  `json:"account_id"`
	AccountEmail string `json:"account_email"`
	Status       string `json:"status"`
	Requested    int    `json:"requested"`
	Deleted      int    `json:"deleted"`
	Failed       int    `json:"failed"`
	Pending      int    `json:"pending"`
	Cancelled    int    `json:"cancelled"`
	Used         int    `json:"used"`
	Limit        int    `json:"limit"`
	RetryAt      string `json:"retry_at,omitempty"`
	WaitReason   string `json:"wait_reason,omitempty"`
}

type adminAPIAliasDeletionWaitDTO struct {
	AccountID   int64  `json:"account_id"`
	AliasID     int64  `json:"alias_id"`
	Operation   string `json:"operation"`
	RetryAt     string `json:"retry_at"`
	Reason      string `json:"reason,omitempty"`
	Used        int    `json:"used"`
	Limit       int    `json:"limit"`
	Attempt     int    `json:"attempt,omitempty"`
	MaxAttempts int    `json:"max_attempts,omitempty"`
	HTTPStatus  int    `json:"http_status,omitempty"`
	ServiceCode string `json:"service_code,omitempty"`
}

// New queue records resume with reconciliation after restart; legacy jobs
// retain their interrupted outcome. Start once before serving HTTP.
func (s *Server) StartAliasDeletionJobs(ctx context.Context) error {
	runtime := &s.aliasDeletionJobs
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.ctx != nil {
		return errors.New("alias deletion jobs already started")
	}
	if err := s.store.RecoverAliasDeletionQueue(ctx); err != nil {
		return err
	}
	runtime.ctx = ctx
	runtime.active = make(map[string]struct{})
	runtime.blocked = make(map[string]struct{})
	runtime.wake = make(chan struct{}, 1)
	return nil
}

func (s *Server) RunAliasDeletionJobs() {
	runtime := &s.aliasDeletionJobs
	runtime.mu.Lock()
	ctx, wake := runtime.ctx, runtime.wake
	runtime.mu.Unlock()
	if ctx == nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		s.dispatchAliasDeletionQueue(ctx)
		select {
		case <-ctx.Done():
		case <-wake:
		case <-ticker.C:
		}
	}
	runtime.mu.Lock()
	runtime.stopping = true
	runtime.mu.Unlock()
	runtime.wg.Wait()
}

func (s *Server) wakeAliasDeletionQueue() {
	runtime := &s.aliasDeletionJobs
	runtime.mu.Lock()
	wake := runtime.wake
	runtime.mu.Unlock()
	select {
	case wake <- struct{}{}:
	default:
	}
}

// Waiting work occupies neither an account lock nor an execution slot.
// There is deliberately no cross-account concurrency semaphore.
func (s *Server) dispatchAliasDeletionQueue(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	// Let short SQL transactions finish during shutdown. Cancelling a scan
	// mid-query can outlive its caller inside database/sql and the SQLite driver.
	// The parent is checked before any claim or remote execution starts.
	scanContext, cancelScan := context.WithTimeout(context.WithoutCancel(ctx), aliasDeletionSaveTimeout)
	heads, err := s.store.ListAliasDeletionQueueHeads(scanContext, s.now().UTC())
	cancelScan()
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Error("读取 Apple 删除队列失败")
		}
		return
	}
	runtime := &s.aliasDeletionJobs
	for _, head := range heads {
		runtime.mu.Lock()
		_, active := runtime.active[head.AppleSubject]
		_, blocked := runtime.blocked[head.AppleSubject]
		if runtime.stopping || ctx.Err() != nil || runtime.rotationPending || active || blocked {
			runtime.mu.Unlock()
			continue
		}
		// A rotation writer must never park all accounts behind a blocking RLock.
		if !s.credentialRotationMu.TryRLock() {
			runtime.mu.Unlock()
			continue
		}
		claimContext, cancelClaim := context.WithTimeout(context.WithoutCancel(ctx), aliasDeletionSaveTimeout)
		work, claimErr := s.store.ClaimAliasDeletionWork(claimContext, head.ID, s.now().UTC())
		cancelClaim()
		if claimErr != nil {
			s.credentialRotationMu.RUnlock()
			runtime.mu.Unlock()
			if !errors.Is(claimErr, store.ErrNotFound) && ctx.Err() == nil {
				s.logger.Error("领取 Apple 删除队列项目失败", "account_id", head.AccountID)
			}
			continue
		}
		runtime.active[work.AppleSubject] = struct{}{}
		runtime.wg.Add(1)
		runtime.mu.Unlock()
		go func() {
			defer runtime.wg.Done()
			defer func() {
				s.credentialRotationMu.RUnlock()
				runtime.mu.Lock()
				delete(runtime.active, work.AppleSubject)
				runtime.mu.Unlock()
				s.wakeAliasDeletionQueue()
			}()
			s.runAliasDeletionWork(ctx, work)
		}()
	}
}

func (s *Server) beginAliasDeletionCredentialRotation() (func(), bool) {
	runtime := &s.aliasDeletionJobs
	runtime.mu.Lock()
	if len(runtime.active) != 0 || runtime.rotationPending {
		runtime.mu.Unlock()
		return nil, false
	}
	runtime.rotationPending = true
	runtime.mu.Unlock()
	return func() {
		runtime.mu.Lock()
		runtime.rotationPending = false
		runtime.mu.Unlock()
		s.wakeAliasDeletionQueue()
	}, true
}

func validAliasDeletionJobID(id string) bool {
	if len(id) < 16 || len(id) > 128 {
		return false
	}
	for _, char := range id {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_') {
			return false
		}
	}
	return true
}

func (s *Server) adminAPIStartAliasDeletionJob(c *gin.Context, admin domain.Session, input adminAPIDeleteAliasesRequest) {
	c.Header("Cache-Control", "no-store")
	if !validAliasDeletionJobID(input.OperationID) {
		writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "请提供 16–128 位字母、数字、连字符或下划线组成的 operation_id")
		return
	}
	runtime := &s.aliasDeletionJobs
	runtime.mu.Lock()
	if runtime.admission == nil {
		runtime.admission = make(chan struct{}, 1)
	}
	admission := runtime.admission
	runtime.mu.Unlock()
	select {
	case admission <- struct{}{}:
		defer func() { <-admission }()
	case <-c.Request.Context().Done():
		apiErr := adminAPIBatchAliasDeleteError(c.Request.Context().Err())
		writeAdminAPIError(c, apiErr.Status, apiErr.Code, apiErr.Message)
		return
	}
	// Retry the idempotency key before alias lookup, including after a completed
	// job has removed its local aliases.
	job, err := s.store.GetAliasDeletionJob(c.Request.Context(), input.OperationID, admin.AdminID)
	if err == nil {
		s.adminAPIReplyExistingAliasDeletionJob(c, job, input.AliasIDs)
		return
	}
	if !errors.Is(err, store.ErrNotFound) {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	admissionContext, cancelAdmission := context.WithTimeout(context.WithoutCancel(c.Request.Context()), aliasDeletionAdmissionTimeout)
	defer cancelAdmission()
	c.Request = c.Request.WithContext(admissionContext)
	aliases, ok := s.adminAPIPreflightAliasBatchDelete(c, admin, input.AliasIDs)
	if !ok {
		return
	}
	// Browser disconnect after authorization/preflight must not strand admission.
	targets := make([]domain.AliasDeletionWork, 0, len(input.AliasIDs))
	prepared := make(map[int64]domain.AliasDeletionWork)
	for _, id := range input.AliasIDs {
		alias := aliases[id]
		target, found := prepared[alias.AccountID]
		if !found {
			var prepareErr error
			target, prepareErr = s.prepareAliasDeletionWork(admissionContext, alias)
			if prepareErr == nil && target.AccountID != alias.AccountID {
				prepareErr = hmesync.ErrAccountChanged
			}
			if prepareErr != nil {
				s.adminAPIFinishBatchAliasDeleteFailure(c, admin, adminAPIBatchAliasDeleteError(prepareErr))
				return
			}
			prepared[alias.AccountID] = target
		}
		target.AliasID, target.Address = alias.ID, alias.Address
		targets = append(targets, target)
	}
	job = domain.AliasDeletionJob{
		ID: input.OperationID, AdminID: admin.AdminID, RequestID: requestID(c), Status: domain.AliasDeletionJobQueued,
		QueueVersion: 1, AuthorizingPasswordVersion: admin.PasswordVersion, Username: admin.Username, IP: c.ClientIP(),
		CreatedAt: s.now().UTC(), UpdatedAt: s.now().UTC(), Items: make([]domain.AliasDeletionJobItem, 0, len(input.AliasIDs)),
	}
	for _, target := range targets {
		job.Items = append(job.Items, domain.AliasDeletionJobItem{ID: target.AliasID, Address: target.Address, AccountID: target.AccountID})
	}
	runtime.mu.Lock()
	if runtime.ctx == nil || runtime.stopping || runtime.ctx.Err() != nil {
		runtime.mu.Unlock()
		writeAdminAPIError(c, http.StatusServiceUnavailable, "BATCH_DELETE_UNAVAILABLE", "后台删除服务尚未启动或正在关闭，请稍后重试")
		return
	}
	if runtime.rotationPending {
		runtime.mu.Unlock()
		writeAdminAPIError(c, http.StatusConflict, "BATCH_DELETE_IN_PROGRESS", "正在轮换凭证，请完成后重新登录再提交删除任务")
		return
	}
	job, err = s.store.EnqueueAliasDeletionJob(admissionContext, job, targets)
	runtime.mu.Unlock()
	if err != nil {
		if errors.Is(err, store.ErrAliasDeletionJobConflict) {
			existing, lookupErr := s.store.GetAliasDeletionJob(admissionContext, input.OperationID, admin.AdminID)
			if lookupErr == nil {
				s.adminAPIReplyExistingAliasDeletionJob(c, existing, input.AliasIDs)
			} else {
				writeAdminAPIError(c, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "任务编号冲突，请重新提交")
			}
		} else {
			s.writeAdminAPIInternalError(c, err)
		}
		return
	}
	s.wakeAliasDeletionQueue()
	writeAdminAPIData(c, http.StatusAccepted, s.adminAPIAliasDeletionJobSnapshot(job))
}

func (s *Server) prepareAliasDeletionWork(ctx context.Context, alias domain.Alias) (domain.AliasDeletionWork, error) {
	if queued, ok := s.hmeSync.(HMEQueuedDeletionService); ok {
		return queued.PrepareAliasDeletion(ctx, alias.ID)
	}
	// Existing embedders still execute one item at a time. Production supplies
	// the verified Apple identity through HMEQueuedDeletionService.
	account, err := s.store.GetAccount(ctx, alias.AccountID)
	if err != nil {
		return domain.AliasDeletionWork{}, err
	}
	return domain.AliasDeletionWork{AccountID: account.ID, AliasID: alias.ID, Address: alias.Address, AccountEmail: account.Email, AppleSubject: "account:" + strconv.FormatInt(account.ID, 10)}, nil
}

func (s *Server) adminAPIReplyExistingAliasDeletionJob(c *gin.Context, job domain.AliasDeletionJob, ids []int64) {
	matched := len(job.Items) == len(ids)
	if matched {
		for i, item := range job.Items {
			if item.ID != ids[i] {
				matched = false
				break
			}
		}
	}
	if !matched {
		writeAdminAPIError(c, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "该任务编号已经用于其他删除选择，请使用原任务查看结果")
		return
	}
	writeAdminAPIData(c, http.StatusAccepted, s.adminAPIAliasDeletionJobSnapshot(job))
}

func aliasDeletionJobActive(status string) bool {
	return status == domain.AliasDeletionJobQueued || status == domain.AliasDeletionJobRunning
}

func (s *Server) adminAPIGetAliasDeletionJob(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id := c.Param("jobID")
	if !validAliasDeletionJobID(id) {
		writeAdminAPIError(c, http.StatusNotFound, "NOT_FOUND", "删除任务不存在")
		return
	}
	job, err := s.store.GetAliasDeletionJob(c.Request.Context(), id, mustSession(c).AdminID)
	if err != nil {
		s.writeAdminAPIStoreReadError(c, err)
		return
	}
	writeAdminAPIData(c, http.StatusOK, s.adminAPIAliasDeletionJobSnapshot(job))
}

func (s *Server) adminAPIListAliasDeletionJobs(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	jobs, err := s.store.ListAliasDeletionJobs(c.Request.Context(), mustSession(c).AdminID, 20)
	if err != nil {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	result := make([]adminAPIAliasDeletionJobDTO, 0, len(jobs))
	for _, job := range jobs {
		result = append(result, s.adminAPIAliasDeletionJobSnapshot(job))
	}
	writeAdminAPIData(c, http.StatusOK, gin.H{"jobs": result})
}

func (s *Server) adminAPICancelAliasDeletionJob(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !validAliasDeletionJobID(c.Param("jobID")) {
		writeAdminAPIError(c, http.StatusNotFound, "NOT_FOUND", "删除任务不存在")
		return
	}
	job, err := s.store.CancelAliasDeletionJob(c.Request.Context(), c.Param("jobID"), mustSession(c).AdminID)
	if err != nil {
		s.writeAdminAPIStoreReadError(c, err)
		return
	}
	s.wakeAliasDeletionQueue()
	writeAdminAPIData(c, http.StatusOK, s.adminAPIAliasDeletionJobSnapshot(job))
}

func (s *Server) adminAPIGetLatestAliasDeletionJob(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	jobs, err := s.store.ListAliasDeletionJobs(c.Request.Context(), mustSession(c).AdminID, 1)
	if err != nil {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	if len(jobs) == 0 {
		writeAdminAPIData(c, http.StatusOK, nil)
		return
	}
	for _, job := range jobs {
		if aliasDeletionJobActive(job.Status) {
			writeAdminAPIData(c, http.StatusOK, s.adminAPIAliasDeletionJobSnapshot(job))
			return
		}
	}
	writeAdminAPIData(c, http.StatusOK, s.adminAPIAliasDeletionJobSnapshot(jobs[0]))
}

func adminAPIAliasDeletionJobFromRecord(job domain.AliasDeletionJob) adminAPIAliasDeletionJobDTO {
	dto := adminAPIAliasDeletionJobDTO{
		JobID: job.ID, Status: job.Status, RequestID: job.RequestID, CancelRequested: job.CancelRequested,
		CreatedAt: job.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: job.UpdatedAt.UTC().Format(time.RFC3339Nano),
		Accounts:                    make([]adminAPIAliasDeletionAccountDTO, 0),
		adminAPIAliasBatchDeleteDTO: adminAPIAliasBatchDeleteDTO{Requested: len(job.Items), Results: make([]adminAPIAliasBatchDeleteItemDTO, 0, len(job.Items))},
	}
	accounts := make(map[int64]*adminAPIAliasDeletionAccountDTO)
	waits := make(map[int64]adminAPIAliasDeletionWaitDTO)
	rank := map[string]int{"completed": 0, "cancelled": 1, "queued": 2, "waiting": 3, "paused": 4, "running": 5}
	for _, item := range job.Items {
		account := accounts[item.AccountID]
		if account == nil {
			account = &adminAPIAliasDeletionAccountDTO{AccountID: item.AccountID, AccountEmail: item.AccountEmail, Status: "completed", Limit: 200}
			accounts[item.AccountID] = account
		}
		account.Requested++
		if item.Used > account.Used {
			account.Used = item.Used
		}
		if item.Limit > 0 {
			account.Limit = item.Limit
		}
		state := item.State
		if state == domain.AliasDeletionWorkPending || state == "" {
			state = "queued"
		}
		if item.Done {
			dto.Processed++
			if state == domain.AliasDeletionWorkCancelled {
				dto.Cancelled++
				account.Cancelled++
				if rank["cancelled"] > rank[account.Status] {
					account.Status = "cancelled"
				}
				continue
			}
			result := adminAPIAliasBatchDeleteItemDTO{ID: item.ID, Address: item.Address, Deleted: item.Deleted, Code: item.Code, Message: item.Message, LocalRetained: item.LocalRetained}
			if item.Deleted {
				dto.Deleted++
				account.Deleted++
			} else {
				dto.Failed++
				account.Failed++
				if item.Code == "APPLE_BATCH_DEFERRED" {
					dto.Deferred++
				}
			}
			dto.Results = append(dto.Results, result)
			continue
		}
		if job.Status == domain.AliasDeletionJobInterrupted {
			dto.Failed++
			account.Failed++
			dto.Results = append(dto.Results, adminAPIAliasBatchDeleteItemDTO{ID: item.ID, Address: item.Address, Code: "BATCH_DELETE_INTERRUPTED", Message: "任务已中断，该邮箱的 Apple 删除结果尚未确认，请刷新 Apple 目录核对后再操作"})
			continue
		}
		dto.Pending++
		account.Pending++
		if rank[state] > rank[account.Status] {
			account.Status = state
		}
		if !item.RetryAt.IsZero() {
			retryAt := item.RetryAt.UTC().Format(time.RFC3339Nano)
			if account.RetryAt == "" || retryAt < account.RetryAt {
				account.RetryAt = retryAt
				account.WaitReason = item.WaitReason
			}
			if _, exists := waits[item.AccountID]; !exists {
				wait := adminAPIAliasDeletionWaitDTO{AccountID: item.AccountID, AliasID: item.ID, Operation: aliasDeletionOperationName(item.Operation), RetryAt: retryAt, Reason: item.WaitReason, Used: item.Used, Limit: account.Limit, HTTPStatus: item.HTTPStatus, ServiceCode: sanitizedAliasDeletionServiceCode(item.ServiceCode)}
				if wait.HTTPStatus < 100 || wait.HTTPStatus > 599 {
					wait.HTTPStatus = 0
				}
				waits[item.AccountID] = wait
			}
		}
	}
	for _, account := range accounts {
		if account.AccountID < 1 {
			continue
		}
		dto.Accounts = append(dto.Accounts, *account)
	}
	for _, wait := range waits {
		dto.Waits = append(dto.Waits, wait)
	}
	sort.Slice(dto.Accounts, func(i, j int) bool { return dto.Accounts[i].AccountID < dto.Accounts[j].AccountID })
	sort.Slice(dto.Waits, func(i, j int) bool { return dto.Waits[i].AccountID < dto.Waits[j].AccountID })
	return dto
}

func (s *Server) adminAPIAliasDeletionJobSnapshot(job domain.AliasDeletionJob) adminAPIAliasDeletionJobDTO {
	dto := adminAPIAliasDeletionJobFromRecord(job)
	subjects := make(map[int64]string)
	for _, item := range job.Items {
		if item.AppleSubject != "" {
			subjects[item.AccountID] = item.AppleSubject
		}
	}
	quotas := make(map[string]domain.AliasDeletionQuota)
	ctx, cancel := context.WithTimeout(context.Background(), aliasDeletionSaveTimeout)
	defer cancel()
	for index := range dto.Accounts {
		account := &dto.Accounts[index]
		subject := subjects[account.AccountID]
		if subject == "" {
			continue
		}
		quota, present := quotas[subject]
		if !present {
			var err error
			quota, err = s.store.GetAliasDeletionQuota(ctx, subject, s.now().UTC())
			if err != nil {
				continue
			}
			quotas[subject] = quota
		}
		account.Used, account.Limit = quota.Used, quota.Limit
	}
	return dto
}

func sanitizedAliasDeletionServiceCode(code string) string {
	if len(code) > 64 {
		return ""
	}
	for _, char := range code {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.') {
			return ""
		}
	}
	return code
}

func aliasDeletionOperationName(op string) string {
	switch op {
	case "validate", "validate Apple session":
		return "validate"
	case "list", "list Hide My Email aliases":
		return "list"
	case "deactivate", "deactivate Hide My Email alias":
		return "deactivate"
	case "delete", "delete Hide My Email alias":
		return "delete"
	default:
		return ""
	}
}

func (s *Server) runAliasDeletionWork(parent context.Context, work domain.AliasDeletionWork) {
	ctx, cancel := context.WithTimeout(parent, aliasDeletionItemTimeout)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			// Panic values may contain credentials; retain only reconciliation state.
			work.Status, work.Reconcile = domain.AliasDeletionWorkPaused, true
			work.WaitReason, work.NextRunAt = "execution_error", s.now().UTC().Add(time.Minute)
			s.logger.Error("Apple 删除队列执行异常，已暂停核对", "account_id", work.AccountID, "alias_id", work.AliasID)
		}
		work.UpdatedAt = s.now().UTC()
		saveContext, cancelSave := context.WithTimeout(context.Background(), aliasDeletionSaveTimeout)
		defer cancelSave()
		if err := s.store.SaveAliasDeletionWork(saveContext, work); err != nil {
			runtime := &s.aliasDeletionJobs
			runtime.mu.Lock()
			runtime.blocked[work.AppleSubject] = struct{}{}
			runtime.mu.Unlock()
			s.logger.Error("保存 Apple 删除进度失败，已停止该主号后续删除", "account_id", work.AccountID, "alias_id", work.AliasID)
		}
	}()
	if parent.Err() != nil {
		work.Status = domain.AliasDeletionWorkPending
		return
	}
	wanted, err := s.store.AliasDeletionWorkWanted(ctx, work.ID)
	if err != nil {
		work.Status, work.Reconcile = domain.AliasDeletionWorkPaused, true
		work.NextRunAt, work.WaitReason = s.now().UTC().Add(time.Minute), "storage_error"
		return
	}
	if !wanted && !work.Reconcile {
		work.Status = domain.AliasDeletionWorkCancelled
		return
	}
	if queued, ok := s.hmeSync.(HMEQueuedDeletionService); ok {
		err = queued.DeleteQueuedAlias(ctx, work)
	} else if batch, ok := s.hmeSync.(HMEBatchDeletionService); ok {
		var outcomes []hmesync.AliasDeletionOutcome
		outcomes, err = batch.DeleteAliases(ctx, []int64{work.AliasID})
		found := false
		for _, outcome := range outcomes {
			if outcome.AliasID == work.AliasID {
				err = outcome.Err
				found = true
				break
			}
		}
		if err == nil && !found {
			err = errors.New("batch deletion returned no result")
		}
	} else if s.hmeSync != nil {
		err = s.hmeSync.DeleteAlias(ctx, work.AliasID)
	} else {
		err = errors.New("Apple deletion service unavailable")
	}
	work.NextRunAt, work.WaitReason = time.Time{}, ""
	work.Code, work.Message, work.LocalRetained = "", "", false
	previousReconcile := work.Reconcile
	work.Reconcile = false
	if err == nil {
		work.Status, work.Deleted = domain.AliasDeletionWorkSucceeded, true
		return
	}
	if parent.Err() != nil {
		work.Status, work.Reconcile = domain.AliasDeletionWorkPending, true
		return
	}
	var wait *hmesync.AliasDeletionWaitError
	if errors.As(err, &wait) {
		work.Status, work.Reconcile = domain.AliasDeletionWorkWaiting, previousReconcile || wait.Reason != "quota"
		work.NextRunAt, work.WaitReason, work.Used, work.Limit = wait.RetryAt, wait.Reason, wait.Used, wait.Limit
		work.Operation, work.HTTPStatus, work.ServiceCode = aliasDeletionOperationName(wait.Operation), wait.HTTPStatus, sanitizedAliasDeletionServiceCode(wait.ServiceCode)
		if !work.NextRunAt.After(s.now()) {
			work.NextRunAt = s.now().UTC().Add(time.Second)
		}
		s.logger.Warn("Apple 删除等待额度恢复", "account_id", work.AccountID, "alias_id", work.AliasID, "reason", work.WaitReason, "retry_at", work.NextRunAt, "used", work.Used, "limit", work.Limit, "operation", work.Operation, "upstream_status", work.HTTPStatus, "upstream_code", work.ServiceCode)
		return
	}
	if errors.Is(err, context.Canceled) {
		wanted, wantedErr := s.store.AliasDeletionWorkWanted(ctx, work.ID)
		if wantedErr == nil && !wanted {
			work.Status = domain.AliasDeletionWorkCancelled
			return
		}
	}
	apiErr := adminAPIBatchAliasDeleteError(err)
	if apiErr.Code == hmesync.CodeLoginRequired || apiErr.Code == hmesync.CodeSessionExpired || apiErr.Code == hmesync.CodeAccountActionRequired {
		work.Status, work.Reconcile = domain.AliasDeletionWorkPaused, true
		var attempt *hmesync.AliasDeletionAttemptError
		if errors.As(err, &attempt) {
			work.Reconcile = previousReconcile || attempt.MutationAttempted
		}
		work.WaitReason, work.NextRunAt, work.Code, work.Message = "login_required", s.now().UTC().Add(time.Minute), apiErr.Code, apiErr.Message
		return
	}
	if ctx.Err() != nil {
		work.Status, work.Reconcile = domain.AliasDeletionWorkPaused, true
		work.WaitReason, work.NextRunAt = "request_timeout", s.now().UTC().Add(time.Minute)
		return
	}
	work.Status = domain.AliasDeletionWorkFailed
	if apiErr.Code != "NOT_FOUND" && apiErr.Code != "BATCH_DELETE_INTERRUPTED" {
		apiErr = adminAPIAppleAliasDeleteFailure(apiErr)
		work.LocalRetained = true
	}
	work.Code, work.Message = apiErr.Code, apiErr.Message
	var upstream *apple.Error
	if errors.As(err, &upstream) && upstream != nil {
		work.Operation, work.ServiceCode = aliasDeletionOperationName(upstream.Op), sanitizedAliasDeletionServiceCode(upstream.ServiceCode)
		if upstream.StatusCode >= 100 && upstream.StatusCode <= 599 {
			work.HTTPStatus = upstream.StatusCode
		}
	}
	s.logger.Warn("Apple alias deletion failed", "action", "delete", "account_id", work.AccountID, "alias_id", work.AliasID, "code", work.Code, "operation", work.Operation, "upstream_status", work.HTTPStatus, "upstream_code", work.ServiceCode)
}
