package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"icloud-api/internal/domain"
	"icloud-api/internal/hmesync"
)

// HMEAliasRegistrationService is optional for older integrations. iCloud
// registration fails closed when their HME service does not implement it.
type HMEAliasRegistrationService interface {
	RegisterExistingAlias(context.Context, domain.Alias) (domain.Alias, error)
}

var errManualAliasRegistrationUnavailable = errors.New("Apple alias registration is unavailable")

func (s *Server) registerManualAlias(ctx context.Context, account domain.Account, alias domain.Alias) (domain.Alias, error) {
	if domain.NormalizeMailboxType(account.MailboxType) == domain.MailboxTypeICloud {
		registration, ok := s.hmeSync.(HMEAliasRegistrationService)
		if !ok {
			return domain.Alias{}, errManualAliasRegistrationUnavailable
		}
		alias.AccountEmail = account.Email
		return registration.RegisterExistingAlias(ctx, alias)
	}
	var created domain.Alias
	err := s.withAccountLock(ctx, account.ID, func() error {
		current, err := s.store.GetAccount(ctx, account.ID)
		if err != nil {
			return err
		}
		if domain.NormalizeMailboxType(current.MailboxType) != domain.MailboxTypeCustom ||
			domain.NormalizeEmail(current.Email) != domain.NormalizeEmail(account.Email) ||
			strings.TrimSpace(current.IMAPUsername) != strings.TrimSpace(account.IMAPUsername) {
			return hmesync.ErrAccountChanged
		}
		if conflict, err := s.externalAliasIdentityConflict(ctx, alias.Address, current); err != nil {
			return err
		} else if conflict {
			return errExternalAliasIdentityConflict
		}
		created, err = s.store.CreateAlias(ctx, alias)
		return err
	})
	return created, err
}

func manualAliasAppleError(err error) (adminAPIAppleError, bool) {
	if errors.Is(err, errManualAliasRegistrationUnavailable) {
		return adminAPIAppleError{
			Status: http.StatusServiceUnavailable, Code: "APPLE_REGISTRATION_UNAVAILABLE",
			Message: "Apple 地址验证服务暂不可用，请稍后重试",
		}, true
	}
	classified := classifyAdminAPIAppleError(err)
	return classified, classified.Code != "INTERNAL_ERROR" && classified.Code != "NOT_FOUND"
}
