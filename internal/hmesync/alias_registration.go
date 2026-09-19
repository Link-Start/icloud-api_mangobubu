package hmesync

import (
	"context"
	"errors"
	"net/mail"
	"strings"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

// RegisterExistingAlias registers only an active address already present in
// Apple's directory. This operation never reserves a new Apple address or
// imports unrelated directory entries.
func (s *Service) RegisterExistingAlias(ctx context.Context, alias domain.Alias) (domain.Alias, error) {
	if alias.AccountID < 1 {
		return domain.Alias{}, errors.New("account ID must be positive")
	}
	alias.Address = domain.NormalizeEmail(alias.Address)
	parsed, err := mail.ParseAddress(alias.Address)
	if err != nil || parsed.Name != "" || domain.NormalizeEmail(parsed.Address) != alias.Address {
		return domain.Alias{}, errors.New("alias address is invalid")
	}
	registrationRepo, ok := s.repo.(AliasRegistrationRepository)
	if !ok {
		return domain.Alias{}, errors.New("alias registration persistence is unavailable")
	}
	release, err := s.acquireOperation(ctx, alias.AccountID)
	if err != nil {
		return domain.Alias{}, err
	}
	defer release()

	account, err := s.repo.GetAccount(ctx, alias.AccountID)
	if err != nil {
		return domain.Alias{}, err
	}
	if domain.NormalizeMailboxType(account.MailboxType) != domain.MailboxTypeICloud ||
		(alias.AccountEmail != "" && !sameEmail(alias.AccountEmail, account.Email)) {
		return domain.Alias{}, wrapError(CodeAccountChanged, ErrAccountChanged, nil)
	}
	record, session, err := s.loadSession(ctx, alias.AccountID)
	if err != nil {
		return domain.Alias{}, err
	}
	if err := validateSessionDSID(session.DSID, session); err != nil {
		return domain.Alias{}, err
	}
	validated, err := s.client.Validate(ctx, session)
	if err != nil {
		return domain.Alias{}, mapAppleError(err, false)
	}
	validated, err = s.registrationSession(record, session, validated)
	if err != nil {
		return domain.Alias{}, err
	}
	list, updated, err := s.client.ListAliases(ctx, validated)
	if err != nil {
		return domain.Alias{}, mapAppleError(err, false)
	}
	updated, err = s.registrationSession(record, session, updated)
	if err != nil {
		return domain.Alias{}, err
	}
	// Validate the complete directory shape before trusting a single entry.
	if _, _, err := filterAliases(list, account.Email); err != nil {
		return domain.Alias{}, err
	}
	remote, exists := findAppleAlias(list.Aliases, alias.Address)
	if !exists {
		return domain.Alias{}, wrapError(CodeAliasNotFound, ErrAliasNotFound, nil)
	}
	if !sameEmail(remote.ForwardToEmail, account.Email) {
		return domain.Alias{}, wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
	}
	if !remote.IsActive {
		return domain.Alias{}, wrapError(CodeAliasInactive, ErrAliasInactive, nil)
	}

	var created domain.Alias
	err = s.locker.WithAccountLock(ctx, alias.AccountID, func() error {
		current, err := s.repo.GetAccount(ctx, alias.AccountID)
		if err != nil {
			return err
		}
		if !sameIdentity(identityOf(current), identityOf(account)) {
			return wrapError(CodeAccountChanged, ErrAccountChanged, nil)
		}
		currentSession, err := s.repo.GetAppleWebSession(ctx, alias.AccountID)
		if errors.Is(err, store.ErrNotFound) {
			return wrapError(CodeAccountChanged, ErrAccountChanged, err)
		}
		if err != nil {
			return err
		}
		if !currentSession.Authenticated || currentSession.Ciphertext != record.Ciphertext ||
			currentSession.AppleID != record.AppleID || currentSession.Region != record.Region {
			return wrapError(CodeAccountChanged, ErrAccountChanged, nil)
		}
		if _, err := s.saveSession(ctx, alias.AccountID, updated); err != nil {
			return err
		}
		created, err = registrationRepo.CreateAlias(ctx, alias)
		return err
	})
	return created, err
}

func (s *Service) registrationSession(record domain.AppleWebSession, original, returned apple.Session) (apple.Session, error) {
	if err := validateSessionDSID(original.DSID, returned); err != nil {
		return apple.Session{}, err
	}
	if strings.TrimSpace(returned.AppleID) != "" && !sameEmail(returned.AppleID, record.AppleID) {
		return apple.Session{}, wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
	}
	normalizeSession(&returned, record.AppleID, original.Region, s.now())
	region, err := normalizeRegion(returned.Region)
	if err != nil || region != original.Region {
		return apple.Session{}, wrapError(CodeAccountMismatch, ErrAccountMismatch, err)
	}
	returned.Region = region
	return returned, nil
}
