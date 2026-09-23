package httpserver

import (
	"context"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"icloud-api/internal/domain"
	"icloud-api/internal/hmesync"
)

// Optional to preserve compatibility with existing HMESyncService adapters.
type HMEForwardingService interface {
	GetForwardingSettings(context.Context, int64) (hmesync.ForwardingSettings, error)
	UpdateForwardingSettings(context.Context, int64, string) (hmesync.ForwardingSettings, error)
}

type adminAPIForwardingSettingsDTO struct {
	SelectedForwardTo string   `json:"selected_forward_to"`
	ForwardToEmails   []string `json:"forward_to_emails"`
}

func (s *Server) adminAPIGetForwardingSettings(c *gin.Context) {
	accountID, ok := adminAPIParseID(c)
	if !ok || !s.adminAPIAppleAccountExists(c, accountID) {
		return
	}
	service, ok := s.hmeSync.(HMEForwardingService)
	if !ok {
		apiErr := adminAPIAppleServiceUnavailable()
		writeAdminAPIError(c, apiErr.Status, apiErr.Code, apiErr.Message)
		return
	}
	settings, err := service.GetForwardingSettings(c.Request.Context(), accountID)
	if err != nil {
		apiErr := classifyAdminAPIAppleError(err)
		writeAdminAPIError(c, apiErr.Status, apiErr.Code, apiErr.Message)
		return
	}
	writeAdminAPIData(c, http.StatusOK, adminAPIForwardingSettings(settings))
}

func (s *Server) adminAPIUpdateForwardingSettings(c *gin.Context) {
	accountID, ok := adminAPIParseID(c)
	if !ok || !s.adminAPIAppleAccountExists(c, accountID) {
		return
	}
	var input struct {
		ForwardToEmail string `json:"forward_to_email"`
	}
	if !decodeAdminAPIJSON(c, &input) {
		return
	}
	input.ForwardToEmail = domain.NormalizeEmail(input.ForwardToEmail)
	if len(input.ForwardToEmail) > 320 || validateEmail(input.ForwardToEmail) != nil {
		writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "请选择有效的转发邮箱")
		return
	}
	adminSession := mustSession(c)
	service, ok := s.hmeSync.(HMEForwardingService)
	if !ok {
		s.adminAPIFinishAppleFailure(c, adminSession, accountID, "update_hme_forwarding", adminAPIAppleServiceUnavailable())
		return
	}
	settings, err := service.UpdateForwardingSettings(c.Request.Context(), accountID, input.ForwardToEmail)
	if err != nil {
		s.adminAPIFinishAppleFailure(c, adminSession, accountID, "update_hme_forwarding", classifyAdminAPIAppleError(err))
		return
	}
	s.audit(c, &adminSession.AdminID, adminSession.Username, "update_hme_forwarding", "account", strconv.FormatInt(accountID, 10), "success", "")
	writeAdminAPIData(c, http.StatusOK, adminAPIForwardingSettings(settings))
}

func adminAPIForwardingSettings(settings hmesync.ForwardingSettings) adminAPIForwardingSettingsDTO {
	return adminAPIForwardingSettingsDTO{
		SelectedForwardTo: settings.SelectedForwardTo,
		ForwardToEmails:   append([]string{}, settings.ForwardToEmails...),
	}
}
