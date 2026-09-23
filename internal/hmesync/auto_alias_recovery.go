package hmesync

import (
	"context"
	"errors"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

// This is a local visibility grace period, not an Apple consistency guarantee.
// Fresh candidates retain the short directory retries in CreateAutoAlias; only
// a later attempt may retire a candidate that still has no directory entry.
const autoCreateConfirmationGracePeriod = 5 * time.Minute

// discardMissingAutoAlias is only called after a successful, identity-checked
// complete directory read found no matching address. It never contacts Apple's
// mutation endpoints. A discarded attempt ends here so retries cannot reserve
// multiple addresses in one schedule slot.
func (s *Service) discardMissingAutoAlias(
	ctx context.Context,
	account domain.Account,
	pending domain.PendingAliasAPIKey,
	directory apple.ListResult,
	accountLockHeld bool,
) (bool, error) {
	createdAt := pending.CreatedAt
	if createdAt.IsZero() {
		createdAt = pending.Alias.CreatedAt
	}
	if createdAt.IsZero() || s.now().Sub(createdAt) < autoCreateConfirmationGracePeriod {
		return false, wrapError(CodeAliasConfirmationPending, ErrAliasConfirmationPending,
			errors.New("Apple directory omitted the pending candidate within its visibility grace period"))
	}
	// The caller has verified Apple ID and DSID. Changing the forwarding target
	// does not change that identity or the complete directory's absence proof.
	if _, _, err := filterAliases(directory, forwardingTarget(directory, account.Email)); err != nil {
		return false, err
	}
	repo, ok := s.repo.(AutoAliasDiscardRepository)
	if !ok {
		return false, wrapPersistenceError(errors.New("automatic alias candidate discard persistence is unavailable"))
	}
	discard := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := s.repo.GetAccount(ctx, account.ID)
		if err != nil {
			return err
		}
		if !current.Enabled {
			return wrapError(CodeAccountDisabled, ErrAccountDisabled, nil)
		}
		if !sameIdentity(identityOf(current), identityOf(account)) {
			return wrapError(CodeAccountChanged, ErrAccountChanged, nil)
		}
		return repo.DiscardPendingAutoAlias(ctx, account.ID, pending.Alias.ID)
	}
	var err error
	if accountLockHeld {
		err = discard()
	} else {
		err = s.locker.WithAccountLock(ctx, account.ID, discard)
	}
	if err != nil {
		return false, wrapPersistenceError(err)
	}
	return true, wrapError(CodeAliasCandidateDiscarded, ErrAliasCandidateDiscarded, nil)
}
