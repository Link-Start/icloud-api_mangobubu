package httpserver

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"icloud-api/internal/apple"
	"icloud-api/internal/hmesync"
)

type HMEAccountEmailService interface {
	GetAccountEmails(context.Context, int64) (apple.AccountEmailProfile, error)
	StartEmailAccountAuth(context.Context, int64, int64, string) (hmesync.EmailActionResult, error)
	VerifyEmailAccountAuth(context.Context, int64, int64, string, string) (hmesync.EmailActionResult, error)
	BeginAccountEmail(context.Context, int64, int64, string) (hmesync.EmailActionResult, error)
	VerifyAccountEmail(context.Context, int64, int64, string, string) (hmesync.EmailActionResult, error)
	DeleteAccountEmail(context.Context, int64, string) error
}

func (s *Server) adminAPIAccountEmail(action string) gin.HandlerFunc {
	return func(c *gin.Context) {
		accountID, ok := adminAPIParseID(c)
		if !ok || !s.adminAPIAppleAccountExists(c, accountID) {
			return
		}
		service, ok := s.hmeSync.(HMEAccountEmailService)
		if !ok {
			apiErr := adminAPIAppleServiceUnavailable()
			writeAdminAPIError(c, apiErr.Status, apiErr.Code, apiErr.Message)
			return
		}
		admin := mustSession(c)
		ctx := c.Request.Context()
		var result any
		var err error
		status := http.StatusOK
		switch action {
		case "list_apple_emails":
			result, err = service.GetAccountEmails(ctx, accountID)
		case "apple_email_auth":
			var input struct {
				Password string `json:"password"`
			}
			if !decodeAdminAPIJSON(c, &input) {
				return
			}
			if input.Password == "" || len(input.Password) > 1024 {
				writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "请填写 Apple 账户密码")
				return
			}
			var auth hmesync.EmailActionResult
			auth, err = service.StartEmailAccountAuth(ctx, admin.AdminID, accountID, input.Password)
			input.Password = ""
			result = auth
			if auth.Status == hmesync.StatusVerificationRequired {
				status = http.StatusAccepted
			}
		case "apple_email_auth_verify", "add_apple_email_verify":
			var input struct {
				ChallengeID string `json:"challenge_id"`
				Code        string `json:"code"`
			}
			if !decodeAdminAPIJSON(c, &input) {
				return
			}
			input.ChallengeID, input.Code = strings.TrimSpace(input.ChallengeID), strings.TrimSpace(input.Code)
			if input.ChallengeID == "" || len(input.ChallengeID) > 256 || !adminAPISixDigitCode(input.Code) {
				writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "请输入有效的 6 位验证码")
				return
			}
			if action == "apple_email_auth_verify" {
				result, err = service.VerifyEmailAccountAuth(ctx, admin.AdminID, accountID, input.ChallengeID, input.Code)
			} else {
				result, err = service.VerifyAccountEmail(ctx, admin.AdminID, accountID, input.ChallengeID, input.Code)
			}
			input.Code = ""
		case "add_apple_email", "delete_apple_email":
			var input struct {
				Address string `json:"address"`
			}
			if !decodeAdminAPIJSON(c, &input) {
				return
			}
			input.Address = strings.TrimSpace(input.Address)
			if len(input.Address) > 320 || validateEmail(input.Address) != nil {
				writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "请填写有效的电子邮箱地址")
				return
			}
			if action == "add_apple_email" {
				result, err = service.BeginAccountEmail(ctx, admin.AdminID, accountID, input.Address)
				status = http.StatusAccepted
			} else {
				err = service.DeleteAccountEmail(ctx, accountID, input.Address)
				result = hmesync.EmailActionResult{Status: "complete", Address: input.Address}
			}
		}
		if err != nil {
			if action == "list_apple_emails" {
				apiErr := classifyAdminAPIAppleError(err)
				writeAdminAPIError(c, apiErr.Status, apiErr.Code, apiErr.Message)
			} else {
				s.adminAPIFinishAppleFailure(c, admin, accountID, action, classifyAdminAPIAppleError(err))
			}
			return
		}
		if action != "list_apple_emails" {
			s.audit(c, &admin.AdminID, admin.Username, action, "account", strconv.FormatInt(accountID, 10), "success", "")
		}
		writeAdminAPIData(c, status, result)
	}
}
