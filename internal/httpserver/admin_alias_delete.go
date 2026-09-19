package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"icloud-api/internal/domain"
	"icloud-api/internal/hmesync"
	"icloud-api/internal/store"
)

const adminAPIMaxAliasBatchDelete = domain.MaxEnabledAliasesPerAccount

type adminAPIDeleteAliasesRequest struct {
	AliasIDs    []int64 `json:"alias_ids"`
	OperationID string  `json:"operation_id,omitempty"`
}

type adminAPIAliasBatchDeleteItemDTO struct {
	ID            int64  `json:"id"`
	Address       string `json:"address"`
	Deleted       bool   `json:"deleted"`
	Code          string `json:"code,omitempty"`
	Message       string `json:"message,omitempty"`
	LocalRetained bool   `json:"local_retained,omitempty"`
	RetryAt       string `json:"retry_at,omitempty"`
	WaitReason    string `json:"wait_reason,omitempty"`
	Used          int    `json:"used,omitempty"`
	Limit         int    `json:"limit,omitempty"`
}

type adminAPIAliasBatchDeleteDTO struct {
	Requested int                               `json:"requested"`
	Deleted   int                               `json:"deleted"`
	Failed    int                               `json:"failed"`
	Results   []adminAPIAliasBatchDeleteItemDTO `json:"results"`
}

// adminAPIDeleteAliases performs an Apple-first batch deletion. Eligibility is
// checked for every selected alias before the first remote mutation, while
// individual remote failures are returned in the result so completed items
// remain visible to the administrator.
func (s *Server) adminAPIDeleteAliases(c *gin.Context) {
	var input adminAPIDeleteAliasesRequest
	if !decodeAdminAPIJSON(c, &input) {
		return
	}
	if len(input.AliasIDs) == 0 || len(input.AliasIDs) > adminAPIMaxAliasBatchDelete {
		writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "请提供要删除的隐私邮箱 ID")
		return
	}
	seen := make(map[int64]struct{}, len(input.AliasIDs))
	for _, aliasID := range input.AliasIDs {
		if aliasID < 1 {
			writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "隐私邮箱 ID 必须是正整数")
			return
		}
		if _, exists := seen[aliasID]; exists {
			writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "隐私邮箱 ID 不能重复")
			return
		}
		seen[aliasID] = struct{}{}
	}

	adminSession := mustSession(c)
	if s.hmeSync == nil {
		s.adminAPIFinishBatchAliasDeleteFailure(c, adminSession, adminAPIAppleServiceUnavailable())
		return
	}
	if mode := c.Query("async"); mode != "" {
		if mode != "1" {
			writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "async 必须为 1")
			return
		}
		s.adminAPIStartAliasDeletionJob(c, adminSession, input)
		return
	}

	aliases, ok := s.adminAPIPreflightAliasBatchDelete(c, adminSession, input.AliasIDs)
	if !ok {
		return
	}

	outcomes, runErr := s.adminAPIRunAliasBatchDelete(c.Request.Context(), input.AliasIDs)
	if runErr != nil && len(outcomes) == 0 {
		outcomes = make([]hmesync.AliasDeletionOutcome, 0, len(input.AliasIDs))
		for _, aliasID := range input.AliasIDs {
			outcomes = append(outcomes, hmesync.AliasDeletionOutcome{AliasID: aliasID, Err: runErr})
		}
	}

	byID := make(map[int64]hmesync.AliasDeletionOutcome, len(outcomes))
	for _, outcome := range outcomes {
		if _, exists := byID[outcome.AliasID]; !exists {
			byID[outcome.AliasID] = outcome
		}
	}

	result := adminAPIAliasBatchDeleteDTO{
		Requested: len(input.AliasIDs),
		Results:   make([]adminAPIAliasBatchDeleteItemDTO, 0, len(input.AliasIDs)),
	}
	for _, aliasID := range input.AliasIDs {
		alias := aliases[aliasID]
		outcome, exists := byID[aliasID]
		if !exists {
			missingErr := runErr
			if missingErr == nil {
				missingErr = errors.New("batch deletion did not return an outcome")
			}
			outcome = hmesync.AliasDeletionOutcome{
				AliasID: aliasID,
				Err:     missingErr,
			}
		}
		item := adminAPIAliasBatchDeleteItemDTO{ID: aliasID, Address: alias.Address}
		if outcome.Err == nil {
			item.Deleted = true
			result.Deleted++
			s.audit(c, &adminSession.AdminID, adminSession.Username, "delete", "alias", strconv.FormatInt(aliasID, 10), "success", "batch")
		} else {
			apiErr := adminAPIBatchAliasDeleteError(outcome.Err)
			if apiErr.Code != "BATCH_DELETE_INTERRUPTED" {
				apiErr = adminAPIAppleAliasDeleteFailure(apiErr)
				item.LocalRetained = true
			}
			item.Code = apiErr.Code
			item.Message = apiErr.Message
			if !apiErr.RetryAt.IsZero() {
				item.RetryAt = apiErr.RetryAt.UTC().Format(time.RFC3339Nano)
				item.WaitReason, item.Used, item.Limit = apiErr.WaitReason, apiErr.Used, apiErr.Limit
			}
			result.Failed++
			s.auditAppleAliasDeleteFailure(c, adminSession, aliasID, apiErr)
		}
		result.Results = append(result.Results, item)
	}

	writeAdminAPIData(c, http.StatusOK, result)
}

func (s *Server) adminAPIPreflightAliasBatchDelete(
	c *gin.Context,
	adminSession domain.Session,
	aliasIDs []int64,
) (map[int64]domain.Alias, bool) {
	aliases, err := s.store.GetAliasesByIDs(c.Request.Context(), aliasIDs)
	if err != nil {
		s.adminAPIFinishBatchAliasDeleteFailure(c, adminSession, adminAPIBatchAliasDeleteError(err))
		return nil, false
	}
	accounts := make(map[int64]domain.Account)
	accountOrder := make([]int64, 0, len(aliasIDs))

	for _, aliasID := range aliasIDs {
		alias := aliases[aliasID]
		// Pending auto-created aliases are eligible for deletion. The deletion
		// service refreshes Apple's authoritative directory under the account
		// lock and removes a locally staged row when Apple omits the address.
		if alias.AccountID < 1 {
			s.adminAPIFinishBatchAliasDeleteFailure(c, adminSession, adminAPIAppleError{
				Status:  http.StatusInternalServerError,
				Code:    "INTERNAL_ERROR",
				Message: "请求处理失败，请稍后重试",
			})
			return nil, false
		}
		aliases[aliasID] = alias
		if _, exists := accounts[alias.AccountID]; exists {
			continue
		}
		account, err := s.store.GetAccount(c.Request.Context(), alias.AccountID)
		if err != nil {
			s.adminAPIFinishBatchAliasDeleteFailure(c, adminSession, adminAPIBatchAliasDeleteError(err))
			return nil, false
		}
		accounts[alias.AccountID] = account
		accountOrder = append(accountOrder, alias.AccountID)
	}

	for _, accountID := range accountOrder {
		account := accounts[accountID]
		if domain.NormalizeMailboxType(account.MailboxType) != domain.MailboxTypeICloud {
			s.adminAPIFinishBatchAliasDeleteFailure(c, adminSession, adminAPIAppleError{
				Status:  http.StatusConflict,
				Code:    "CUSTOM_MAILBOX_NO_APPLE",
				Message: "批量删除仅支持 Apple 已登录的 iCloud 主号",
			})
			return nil, false
		}
		if strings.EqualFold(strings.TrimSpace(account.LastSyncStatus), domain.SyncStatusError) ||
			strings.TrimSpace(account.LastSyncError) != "" {
			s.adminAPIFinishBatchAliasDeleteFailure(c, adminSession, adminAPIAppleError{
				Status:  http.StatusConflict,
				Code:    "BATCH_DELETE_NOT_ELIGIBLE",
				Message: "该 iCloud 主号存在同步错误，请先处理错误后再批量删除",
			})
			return nil, false
		}

		info, err := s.hmeSync.GetSession(c.Request.Context(), accountID)
		if err != nil {
			s.adminAPIFinishBatchAliasDeleteFailure(c, adminSession, classifyAdminAPIAppleError(err))
			return nil, false
		}
		switch info.Status {
		case hmesync.StatusAuthenticated:
			// Continue; the deletion service validates the live session again
			// immediately before contacting Apple.
		case hmesync.StatusExpired:
			s.adminAPIFinishBatchAliasDeleteFailure(c, adminSession, classifyAdminAPIAppleError(hmesync.ErrSessionExpired))
			return nil, false
		default:
			s.adminAPIFinishBatchAliasDeleteFailure(c, adminSession, classifyAdminAPIAppleError(hmesync.ErrLoginRequired))
			return nil, false
		}
	}
	return aliases, true
}

func (s *Server) adminAPIRunAliasBatchDelete(
	ctx context.Context,
	aliasIDs []int64,
) ([]hmesync.AliasDeletionOutcome, error) {
	if batch, ok := s.hmeSync.(HMEBatchDeletionService); ok {
		return batch.DeleteAliases(ctx, aliasIDs)
	}
	outcomes := make([]hmesync.AliasDeletionOutcome, 0, len(aliasIDs))
	for _, aliasID := range aliasIDs {
		if err := ctx.Err(); err != nil {
			outcomes = append(outcomes, hmesync.AliasDeletionOutcome{AliasID: aliasID, Err: err})
			continue
		}
		outcomes = append(outcomes, hmesync.AliasDeletionOutcome{
			AliasID: aliasID,
			Err:     s.hmeSync.DeleteAlias(ctx, aliasID),
		})
	}
	return outcomes, nil
}

func adminAPIBatchAliasDeleteError(err error) adminAPIAppleError {
	var wait *hmesync.AliasDeletionWaitError
	if errors.As(err, &wait) {
		return classifyAdminAPIAppleError(err)
	}
	if hmesync.Code(err) == hmesync.CodeBatchDeferred || errors.Is(err, hmesync.ErrBatchDeferred) {
		return adminAPIAppleError{
			Status: http.StatusConflict, Code: hmesync.CodeBatchDeferred,
			Message: "该主号持续受到 Apple 限流，此邮箱尚未执行删除，请稍后重新选择",
		}
	}
	// A known throttle response can also wrap a timeout while reading its
	// body. Preserve the explicit recovery classification; an expired job's
	// own context is handled separately by its runner as an interruption.
	if hmesync.Code(err) == hmesync.CodeRateLimited {
		return classifyAdminAPIAppleError(hmesync.ErrRateLimited)
	}
	if errors.Is(err, context.Canceled) && hmesync.Code(err) == "" {
		return adminAPIAppleError{
			Status: http.StatusConflict, Code: "BATCH_DELETE_INTERRUPTED",
			Message: "删除任务已中断，请刷新 Apple 目录核对结果后重试",
		}
	}
	// A pending marker introduced after batch admission is still protected by
	// the deletion service. Preserve that concurrency result for the API while
	// allowing aliases that were already pending at admission to reconcile.
	if errors.Is(err, store.ErrAliasConfirmationPending) || errors.Is(err, hmesync.ErrAliasConfirmationPending) {
		return adminAPIAppleError{
			Status:  http.StatusConflict,
			Code:    hmesync.CodeAliasConfirmationPending,
			Message: "该隐私邮箱正在等待 Apple 目录确认，暂时不能批量删除",
		}
	}
	// A missing Apple session also wraps store.ErrNotFound. Preserve the
	// Apple classification before specializing a genuine missing-record error.
	apiErr := classifyAdminAPIAppleError(err)
	if apiErr.Code == "NOT_FOUND" {
		apiErr.Message = "隐私邮箱不存在"
	}
	return apiErr
}

func (s *Server) adminAPIFinishBatchAliasDeleteFailure(
	c *gin.Context,
	adminSession domain.Session,
	apiErr adminAPIAppleError,
) {
	apiErr = adminAPIAppleAliasDeleteFailure(apiErr)
	s.logger.Warn("批量删除 Apple 隐私邮箱失败",
		"action", "delete",
		"code", apiErr.Code,
		"request_id", requestID(c),
	)
	s.audit(c, &adminSession.AdminID, adminSession.Username, "delete", "alias", "batch", "failed", apiErr.Code)
	writeAdminAPIAppleError(c, apiErr)
}
