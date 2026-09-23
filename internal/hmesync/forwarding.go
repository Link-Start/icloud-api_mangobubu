package hmesync

import (
	"context"
	"errors"
	"net/mail"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

// ForwardingSettings describes the account-wide setting, without importing or
// removing any aliases from the local directory.
type ForwardingSettings struct {
	SelectedForwardTo string
	ForwardToEmails   []string
}

func (s *Service) GetForwardingSettings(ctx context.Context, accountID int64) (ForwardingSettings, error) {
	return s.forwardingSettings(ctx, accountID, "")
}

func (s *Service) UpdateForwardingSettings(ctx context.Context, accountID int64, email string) (ForwardingSettings, error) {
	email = domain.NormalizeEmail(email)
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Name != "" || parsed.Address != email || len(email) > 320 {
		return ForwardingSettings{}, wrapError(CodeForwardingTargetInvalid, ErrForwardingTargetInvalid, nil)
	}
	return s.forwardingSettings(ctx, accountID, email)
}

func (s *Service) forwardingSettings(ctx context.Context, accountID int64, target string) (ForwardingSettings, error) {
	if accountID < 1 {
		return ForwardingSettings{}, errors.New("account ID must be positive")
	}
	release, err := s.acquireOperation(ctx, accountID)
	if err != nil {
		return ForwardingSettings{}, err
	}
	defer release()

	account, err := s.repo.GetAccount(ctx, accountID)
	if err != nil {
		return ForwardingSettings{}, err
	}
	if domain.NormalizeMailboxType(account.MailboxType) != domain.MailboxTypeICloud {
		return ForwardingSettings{}, wrapError(CodeAccountChanged, ErrAccountChanged, store.ErrICloudMailboxRequired)
	}
	record, original, err := s.loadSession(ctx, accountID)
	if err != nil {
		return ForwardingSettings{}, err
	}
	if err := validateSessionDSID(original.DSID, original); err != nil {
		return ForwardingSettings{}, err
	}
	validated, err := s.client.Validate(ctx, original)
	if err != nil {
		return ForwardingSettings{}, s.forwardingError(ctx, record, err)
	}
	validated, err = s.registrationSession(record, original, validated)
	if err != nil {
		return ForwardingSettings{}, err
	}
	list, session, err := s.client.ListAliases(ctx, validated)
	if err != nil {
		return ForwardingSettings{}, s.forwardingError(ctx, record, err)
	}
	session, err = s.registrationSession(record, original, session)
	if err != nil {
		return ForwardingSettings{}, err
	}
	if _, _, err := filterAliases(list, account.Email); err != nil {
		return ForwardingSettings{}, err
	}
	settings := forwardingSettingsFromList(list)
	if target != "" && !containsForwardingCandidate(settings.ForwardToEmails, target) {
		return ForwardingSettings{}, wrapError(CodeForwardingTargetInvalid, ErrForwardingTargetInvalid, nil)
	}

	// Serialize the remote mutation with account edits as well as other Apple
	// operations. Recheck the account and session before changing Apple state.
	err = s.locker.WithAccountLock(ctx, accountID, func() error {
		current, err := s.repo.GetAccount(ctx, accountID)
		if err != nil {
			return err
		}
		if !sameIdentity(identityOf(current), identityOf(account)) {
			return wrapError(CodeAccountChanged, ErrAccountChanged, nil)
		}
		currentSession, err := s.repo.GetAppleWebSession(ctx, accountID)
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

		// Preserve rotated cookies even when a mutation or its confirmation
		// fails, including when the browser disconnects after submitting it.
		checkpoint := func(returned apple.Session, requestErr error) error {
			if !hasAppleSessionState(returned) {
				returned = session
			}
			checked, identityErr := s.registrationSession(record, original, returned)
			if identityErr != nil {
				return identityErr
			}
			session = checked
			persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			mapped := mapAppleError(requestErr, false)
			if errors.Is(mapped, ErrSessionExpired) {
				deleteErr := s.repo.DeleteAppleWebSession(persistCtx, accountID)
				if errors.Is(deleteErr, store.ErrNotFound) {
					deleteErr = nil
				}
				return errors.Join(mapped, deleteErr)
			}
			_, saveErr := s.saveSession(persistCtx, accountID, session)
			return errors.Join(mapped, saveErr)
		}
		if err := checkpoint(session, nil); err != nil {
			return err
		}
		if target == "" || sameEmail(settings.SelectedForwardTo, target) {
			return nil
		}
		updater, ok := s.client.(ForwardingTargetUpdater)
		if !ok {
			return wrapError(CodeUpstreamError, ErrUpstream, errors.New("forwarding update is unavailable"))
		}
		updated, updateErr := updater.UpdateForwardTo(ctx, session, target)
		if err := checkpoint(updated, updateErr); err != nil {
			return err
		}
		confirmed, updated, verifyErr := s.client.ListAliases(ctx, session)
		if err := checkpoint(updated, verifyErr); err != nil {
			return err
		}
		if !sameEmail(confirmed.SelectedForwardTo, target) {
			return wrapError(CodeForwardingNotConfirmed, ErrForwardingNotConfirmed, nil)
		}
		settings = forwardingSettingsFromList(confirmed)
		return nil
	})
	if err != nil {
		return ForwardingSettings{}, err
	}
	return settings, nil
}

func (s *Service) forwardingError(ctx context.Context, expected domain.AppleWebSession, err error) error {
	mapped := mapAppleError(err, false)
	if errors.Is(mapped, ErrSessionExpired) {
		// Account edits can replace/remove a session during the initial network
		// reads. An old response must not invalidate a replacement session.
		_ = s.locker.WithAccountLock(ctx, expected.AccountID, func() error {
			current, readErr := s.repo.GetAppleWebSession(ctx, expected.AccountID)
			if readErr != nil || current.Ciphertext != expected.Ciphertext {
				return readErr
			}
			return s.repo.DeleteAppleWebSession(ctx, expected.AccountID)
		})
	}
	return mapped
}

func forwardingSettingsFromList(list apple.ListResult) ForwardingSettings {
	settings := ForwardingSettings{
		SelectedForwardTo: domain.NormalizeEmail(list.SelectedForwardTo),
		ForwardToEmails:   make([]string, 0, len(list.ForwardToEmails)),
	}
	for _, candidate := range list.ForwardToEmails {
		candidate = domain.NormalizeEmail(candidate)
		if candidate != "" && !containsForwardingCandidate(settings.ForwardToEmails, candidate) {
			settings.ForwardToEmails = append(settings.ForwardToEmails, candidate)
		}
	}
	return settings
}
