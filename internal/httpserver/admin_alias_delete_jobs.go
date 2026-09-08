package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"icloud-api/internal/domain"
	"icloud-api/internal/hmesync"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

const (
	aliasDeletionJobLimit    = 2
	aliasDeletionJobTimeout  = 2 * time.Hour
	aliasDeletionSaveTimeout = 5 * time.Second
)

type aliasDeletionJobRuntime struct {
	mu              sync.Mutex
	ctx             context.Context
	active          map[aliasDeletionJobKey]struct{}
	admission       chan struct{}
	stopping        bool
	rotationPending bool
	wg              sync.WaitGroup
}

type aliasDeletionJobKey struct {
	adminID int64
	id      string
}

type adminAPIAliasDeletionJobDTO struct {
	adminAPIAliasBatchDeleteDTO
	JobID     string `json:"job_id"`
	Status    string `json:"status"`
	Processed int    `json:"processed"`
	RequestID string `json:"request_id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// StartAliasDeletionJobs must run once before serving HTTP in a single-writer
// deployment. Interrupted jobs are observable but never automatically replayed:
// a crash can happen between an irreversible Apple call and its local checkpoint.
func (s *Server) StartAliasDeletionJobs(ctx context.Context) error {
	runtime := &s.aliasDeletionJobs
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.ctx != nil {
		return errors.New("alias deletion jobs already started")
	}
	if err := s.store.InterruptAliasDeletionJobs(ctx); err != nil {
		return err
	}
	runtime.ctx = ctx
	runtime.active = make(map[aliasDeletionJobKey]struct{})
	return nil
}

// RunAliasDeletionJobs owns all accepted goroutines until their bounded
// persistence has finished, so process shutdown does not close their database.
func (s *Server) RunAliasDeletionJobs() {
	runtime := &s.aliasDeletionJobs
	runtime.mu.Lock()
	ctx := runtime.ctx
	runtime.mu.Unlock()
	if ctx == nil {
		return
	}
	<-ctx.Done()
	runtime.mu.Lock()
	runtime.stopping = true
	runtime.mu.Unlock()
	runtime.wg.Wait()
}

// Rotation must not wait behind an hour-long background read lock and block
// every progress poll. Admission and rotation use the same mutex to close the
// check/start race before the existing credential lock is acquired.
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
	}, true
}

func validAliasDeletionJobID(id string) bool {
	if len(id) < 16 || len(id) > 128 {
		return false
	}
	for _, char := range id {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_') {
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
	// Serialize query -> preflight -> insertion for competing submissions.
	// A same-key retry must see its predecessor before any stale-alias or
	// active/capacity rejection. Progress reads and execution never take this gate.
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
	// Consult the idempotency key before looking up aliases: completed jobs
	// have already removed some or all of those local IDs.
	job, err := s.store.GetAliasDeletionJob(c.Request.Context(), input.OperationID, admin.AdminID)
	if err == nil {
		s.adminAPIReplyExistingAliasDeletionJob(c, job, input.AliasIDs)
		return
	}
	if !errors.Is(err, store.ErrNotFound) {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	latest, err := s.store.GetActiveAliasDeletionJob(c.Request.Context(), admin.AdminID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	if err == nil && aliasDeletionJobActive(latest.Status) {
		writeAdminAPIError(c, http.StatusConflict, "BATCH_DELETE_IN_PROGRESS", "已有后台删除任务，请先查看该任务的进度")
		return
	}
	aliases, ok := s.adminAPIPreflightAliasBatchDelete(c, admin, input.AliasIDs)
	if !ok {
		return
	}
	job = domain.AliasDeletionJob{
		ID: input.OperationID, AdminID: admin.AdminID, RequestID: requestID(c),
		Status: domain.AliasDeletionJobQueued, CreatedAt: s.now().UTC(), UpdatedAt: s.now().UTC(),
		Items: make([]domain.AliasDeletionJobItem, 0, len(input.AliasIDs)),
	}
	for _, id := range input.AliasIDs {
		job.Items = append(job.Items, domain.AliasDeletionJobItem{ID: id, Address: aliases[id].Address})
	}
	rawSession, err := c.Cookie(sessionCookie)
	if err != nil {
		writeAdminAPIError(c, http.StatusUnauthorized, "SESSION_EXPIRED", "登录会话已失效")
		return
	}
	sessionHash := secure.HashToken(rawSession)
	ip := c.ClientIP()
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
	if len(runtime.active) >= aliasDeletionJobLimit {
		runtime.mu.Unlock()
		writeAdminAPIError(c, http.StatusTooManyRequests, "BATCH_DELETE_BUSY", "后台删除任务已满，请稍后提交")
		return
	}
	// Admission is short and bounded, but cannot share the connection's
	// cancellation: a disconnect just after INSERT must not strand a queued
	// task without its in-process owner.
	admissionContext, cancelAdmission := context.WithTimeout(context.WithoutCancel(c.Request.Context()), aliasDeletionSaveTimeout)
	err = s.store.CreateAliasDeletionJob(admissionContext, job)
	cancelAdmission()
	if err != nil {
		runtime.mu.Unlock()
		if errors.Is(err, store.ErrAliasDeletionJobConflict) {
			existing, lookupErr := s.store.GetAliasDeletionJob(c.Request.Context(), input.OperationID, admin.AdminID)
			if lookupErr == nil {
				s.adminAPIReplyExistingAliasDeletionJob(c, existing, input.AliasIDs)
			} else {
				writeAdminAPIError(c, http.StatusConflict, "BATCH_DELETE_IN_PROGRESS", "已有后台删除任务或任务编号冲突，请先查看任务进度")
			}
			return
		}
		s.writeAdminAPIInternalError(c, err)
		return
	}
	key := aliasDeletionJobKey{adminID: job.AdminID, id: job.ID}
	runtime.active[key] = struct{}{}
	runtime.wg.Add(1)
	workerContext := runtime.ctx
	// Copy before starting the worker: the worker mutates its own item slice.
	dto := adminAPIAliasDeletionJobFromRecord(job)
	job.Items = append([]domain.AliasDeletionJobItem(nil), job.Items...)
	runtime.mu.Unlock()
	go func() {
		defer func() {
			runtime.mu.Lock()
			delete(runtime.active, key)
			runtime.mu.Unlock()
			runtime.wg.Done()
		}()
		s.runAliasDeletionJob(workerContext, job, admin, sessionHash, ip)
	}()
	writeAdminAPIData(c, http.StatusAccepted, dto)
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
	writeAdminAPIData(c, http.StatusAccepted, adminAPIAliasDeletionJobFromRecord(job))
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
	writeAdminAPIData(c, http.StatusOK, adminAPIAliasDeletionJobFromRecord(job))
}

func (s *Server) adminAPIGetLatestAliasDeletionJob(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	job, err := s.store.GetActiveAliasDeletionJob(c.Request.Context(), mustSession(c).AdminID)
	if errors.Is(err, store.ErrNotFound) {
		job, err = s.store.GetLatestAliasDeletionJob(c.Request.Context(), mustSession(c).AdminID)
	}
	if errors.Is(err, store.ErrNotFound) {
		writeAdminAPIData(c, http.StatusOK, nil)
		return
	}
	if err != nil {
		s.writeAdminAPIInternalError(c, err)
		return
	}
	writeAdminAPIData(c, http.StatusOK, adminAPIAliasDeletionJobFromRecord(job))
}

func adminAPIAliasDeletionJobFromRecord(job domain.AliasDeletionJob) adminAPIAliasDeletionJobDTO {
	dto := adminAPIAliasDeletionJobDTO{
		JobID: job.ID, Status: job.Status, RequestID: job.RequestID,
		CreatedAt: job.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: job.UpdatedAt.UTC().Format(time.RFC3339Nano),
		adminAPIAliasBatchDeleteDTO: adminAPIAliasBatchDeleteDTO{
			Requested: len(job.Items), Results: make([]adminAPIAliasBatchDeleteItemDTO, 0, len(job.Items)),
		},
	}
	for _, item := range job.Items {
		if !item.Done && job.Status != domain.AliasDeletionJobInterrupted {
			continue
		}
		result := adminAPIAliasBatchDeleteItemDTO{
			ID: item.ID, Address: item.Address, Deleted: item.Deleted, Code: item.Code,
			Message: item.Message, LocalRetained: item.LocalRetained,
		}
		if item.Done {
			dto.Processed++
		} else {
			result.Code = "BATCH_DELETE_INTERRUPTED"
			result.Message = "任务已中断，该邮箱的 Apple 删除结果尚未确认，请刷新 Apple 目录核对后再操作"
			result.LocalRetained = false
		}
		if result.Deleted {
			dto.Deleted++
		} else {
			dto.Failed++
		}
		dto.Results = append(dto.Results, result)
	}
	return dto
}

func (s *Server) saveAliasDeletionJob(job domain.AliasDeletionJob, audit *domain.AuditLog) error {
	ctx, cancel := context.WithTimeout(context.Background(), aliasDeletionSaveTimeout)
	defer cancel()
	return s.store.SaveAliasDeletionJob(ctx, job, audit)
}

func (s *Server) runAliasDeletionJob(parent context.Context, job domain.AliasDeletionJob, admin domain.Session, sessionHash []byte, ip string) {
	ctx, cancel := context.WithTimeout(parent, aliasDeletionJobTimeout)
	defer cancel()
	var progressMu sync.Mutex
	var persistenceErr error
	defer func() {
		if recovered := recover(); recovered != nil {
			// Never log a panic value: Apple/session adapters can embed secrets.
			s.logger.Error("后台删除任务异常中断", "job_id", job.ID, "request_id", job.RequestID)
		}
		progressMu.Lock()
		defer progressMu.Unlock()
		if aliasDeletionJobActive(job.Status) {
			job.Status = domain.AliasDeletionJobInterrupted
			job.UpdatedAt = s.now().UTC()
			if err := s.saveAliasDeletionJob(job, nil); err != nil {
				s.logger.Error("保存后台删除任务终态失败", "job_id", job.ID, "request_id", job.RequestID)
			}
		}
	}()

	// The task was authorized by the accepted request. Recheck after acquiring
	// the same rotation guard; never retain a gin.Context or raw session cookie.
	for !s.credentialRotationMu.TryRLock() {
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	defer s.credentialRotationMu.RUnlock()
	if ctx.Err() != nil {
		return
	}
	current, err := s.store.GetSessionByHash(ctx, sessionHash)
	if err != nil || current.AdminID != admin.AdminID || current.PasswordVersion != admin.PasswordVersion {
		return
	}
	job.Status = domain.AliasDeletionJobRunning
	job.UpdatedAt = s.now().UTC()
	if err := s.saveAliasDeletionJob(job, nil); err != nil {
		return
	}
	ids := make([]int64, len(job.Items))
	indices := make(map[int64]int, len(job.Items))
	for i, item := range job.Items {
		ids[i] = item.ID
		indices[item.ID] = i
	}
	report := func(outcome hmesync.AliasDeletionOutcome) {
		progressMu.Lock()
		defer progressMu.Unlock()
		index, exists := indices[outcome.AliasID]
		if !exists || job.Items[index].Done || persistenceErr != nil {
			return
		}
		item := job.Items[index]
		item.Done = true
		audit := domain.AuditLog{
			AdminID: &admin.AdminID, Username: admin.Username, Action: "delete", ResourceType: "alias",
			ResourceID: strconv.FormatInt(item.ID, 10), Result: "success", Detail: "batch",
			IP: ip, RequestID: job.RequestID, CreatedAt: s.now().UTC(),
		}
		if outcome.Err == nil {
			item.Deleted = true
		} else {
			apiErr := adminAPIBatchAliasDeleteError(outcome.Err)
			if ctx.Err() != nil && errors.Is(outcome.Err, ctx.Err()) {
				apiErr = adminAPIAppleError{
					Status: http.StatusConflict, Code: "BATCH_DELETE_INTERRUPTED",
					Message: "删除任务已停止或达到运行时限，请刷新 Apple 目录核对结果后再操作",
				}
			}
			if apiErr.Code != "BATCH_DELETE_INTERRUPTED" && apiErr.Code != "NOT_FOUND" {
				apiErr = adminAPIAppleAliasDeleteFailure(apiErr)
				item.LocalRetained = true
			}
			item.Code, item.Message = apiErr.Code, apiErr.Message
			audit.Result, audit.Detail = "failed", apiErr.Code
		}
		previous := job.Items[index]
		job.Items[index] = item
		job.UpdatedAt = s.now().UTC()
		if err := s.saveAliasDeletionJob(job, &audit); err != nil {
			// Do not publish an uncommitted result; cancellation stops further
			// mutations, while in-flight calls still get bounded reconciliation.
			job.Items[index] = previous
			persistenceErr = err
			cancel()
			s.logger.Error("保存后台删除任务进度失败，已停止后续删除", "job_id", job.ID, "alias_id", item.ID, "request_id", job.RequestID)
			return
		}
		if outcome.Err != nil {
			s.logger.Warn("Apple alias deletion failed", "action", "delete", "alias_id", item.ID,
				"code", item.Code, "job_id", job.ID, "request_id", job.RequestID)
		}
	}
	ctx = hmesync.WithAliasDeletionProgress(ctx, report)
	var outcomes []hmesync.AliasDeletionOutcome
	var runErr error
	if batch, ok := s.hmeSync.(HMEBatchDeletionService); ok {
		outcomes, runErr = batch.DeleteAliases(ctx, ids)
	} else {
		for _, id := range ids {
			if ctx.Err() != nil {
				break
			}
			err := s.hmeSync.DeleteAlias(ctx, id)
			report(hmesync.AliasDeletionOutcome{AliasID: id, Err: err})
		}
	}
	// Compatibility implementations may not emit progress callbacks. Folding
	// their final results is idempotent and preserves every reported success.
	for _, outcome := range outcomes {
		report(outcome)
	}
	progressMu.Lock()
	defer progressMu.Unlock()
	complete := persistenceErr == nil && runErr == nil && ctx.Err() == nil
	for _, item := range job.Items {
		complete = complete && item.Done
	}
	status := domain.AliasDeletionJobInterrupted
	if complete {
		status = domain.AliasDeletionJobCompleted
	}
	job.Status = status
	job.UpdatedAt = s.now().UTC()
	if err := s.saveAliasDeletionJob(job, nil); err != nil {
		job.Status = domain.AliasDeletionJobRunning // The deferred bounded finalizer retries.
		s.logger.Error("保存后台删除任务结果失败", "job_id", job.ID, "request_id", job.RequestID)
	}
}
