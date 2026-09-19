package hmesync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

const aliasDeletionItemTimeout = 5 * time.Minute

// AliasDeletionAttemptError distinguishes an attempt that only read account or
// directory state from one that may have changed Apple. A queue can cancel a
// login-paused item immediately when it has no earlier result to reconcile.
// Unwrap preserves typed login, wait and cancellation errors for existing callers.
type AliasDeletionAttemptError struct {
	Err               error
	MutationAttempted bool
}

func (e *AliasDeletionAttemptError) Error() string {
	if e == nil || e.Err == nil {
		return "Apple deletion attempt failed"
	}
	return e.Err.Error()
}

func (e *AliasDeletionAttemptError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// AliasDeletionWaitError means that the item is still pending. The queue saves
// RetryAt and releases its worker; it must not record this as a failed deletion.
type AliasDeletionWaitError struct {
	Reason      string
	RetryAt     time.Time
	Used        int
	Limit       int
	Operation   string
	HTTPStatus  int
	ServiceCode string
	cause       error
}

func (e *AliasDeletionWaitError) Error() string { return CodeRateLimited }
func (e *AliasDeletionWaitError) Unwrap() error {
	return wrapError(CodeRateLimited, ErrRateLimited, e.cause)
}

func aliasDeletionSubject(session apple.Session) (string, error) {
	dsid := strings.TrimSpace(session.DSID)
	region, err := normalizeRegion(session.Region)
	if err != nil || dsid == "" {
		return "", wrapError(CodeSessionExpired, ErrSessionExpired, errors.New("Apple session has no verified subject"))
	}
	return string(region) + ":" + dsid, nil
}

// PrepareAliasDeletion captures identity only; it makes no Apple mutations or
// remote requests. Credentials remain in the encrypted session repository.
func (s *Service) PrepareAliasDeletion(ctx context.Context, aliasID int64) (domain.AliasDeletionWork, error) {
	if aliasID < 1 {
		return domain.AliasDeletionWork{}, errors.New("alias ID must be positive")
	}
	repo, ok := s.repo.(AliasDeletionRepository)
	if !ok {
		return domain.AliasDeletionWork{}, errors.New("alias deletion persistence is unavailable")
	}
	alias, err := repo.GetAlias(ctx, aliasID)
	if err != nil {
		return domain.AliasDeletionWork{}, err
	}
	account, err := s.repo.GetAccount(ctx, alias.AccountID)
	if err != nil {
		return domain.AliasDeletionWork{}, err
	}
	if domain.NormalizeMailboxType(account.MailboxType) != domain.MailboxTypeICloud {
		return domain.AliasDeletionWork{}, wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
	}
	record, session, err := s.loadSession(ctx, alias.AccountID)
	if err != nil {
		return domain.AliasDeletionWork{}, err
	}
	subject, err := aliasDeletionSubject(session)
	if err != nil {
		return domain.AliasDeletionWork{}, err
	}
	return domain.AliasDeletionWork{
		AccountID: alias.AccountID, AliasID: alias.ID, Address: domain.NormalizeEmail(alias.Address),
		AccountEmail: domain.NormalizeEmail(account.Email), AppleID: domain.NormalizeEmail(record.AppleID), AppleSubject: subject,
	}, nil
}

// DeleteQueuedAlias executes one bounded attempt. A restart, a cooldown, or a
// missing local row always leads to a fresh authenticated directory read before
// another mutation. This method never waits for an hourly allowance to return.
func (s *Service) DeleteQueuedAlias(ctx context.Context, work domain.AliasDeletionWork) (resultErr error) {
	var batch *aliasDeletionBatch
	defer func() {
		if resultErr != nil {
			resultErr = &AliasDeletionAttemptError{Err: resultErr, MutationAttempted: batch != nil && batch.mutationAttempted}
		}
	}()
	if work.AccountID < 1 || work.AliasID < 1 || strings.TrimSpace(work.Address) == "" ||
		strings.TrimSpace(work.AccountEmail) == "" || strings.TrimSpace(work.AppleID) == "" || strings.TrimSpace(work.AppleSubject) == "" {
		return errors.New("alias deletion identity snapshot is incomplete")
	}
	repo, ok := s.repo.(AliasDeletionRepository)
	if !ok {
		return errors.New("alias deletion persistence is unavailable")
	}
	client, ok := s.client.(AliasDeletionClient)
	if !ok {
		return errors.New("Apple alias deletion client is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, aliasDeletionItemTimeout)
	defer cancel()
	batch = &aliasDeletionBatch{s: s, repo: repo, client: client, accountID: work.AccountID, work: &work,
		initialPending: map[int64]bool{work.AliasID: true}}
	return s.withAliasDeletionAccount(ctx, work.AccountID, nil, func(_ func(context.Context, time.Duration) error) error {
		return batch.delete(ctx, work.AliasID)
	})
}

func (b *aliasDeletionBatch) checkWork(ctx context.Context, mutation bool) error {
	if b.work == nil {
		return nil
	}
	w := b.work
	account, err := b.s.repo.GetAccount(ctx, w.AccountID)
	if err != nil {
		return err
	}
	if !sameEmail(account.Email, w.AccountEmail) || domain.NormalizeMailboxType(account.MailboxType) != domain.MailboxTypeICloud {
		return wrapError(CodeAccountChanged, ErrAccountChanged, nil)
	}
	alias, err := b.repo.GetAlias(ctx, w.AliasID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err == nil && (alias.AccountID != w.AccountID || !sameEmail(alias.Address, w.Address)) {
		return wrapError(CodeAccountChanged, ErrAccountChanged, nil)
	}
	if lookup, ok := b.s.repo.(interface {
		GetAliasByAddress(context.Context, string) (domain.Alias, error)
	}); ok {
		byAddress, err := lookup.GetAliasByAddress(ctx, w.Address)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err == nil && byAddress.AccountID != w.AccountID {
			return wrapError(CodeAccountChanged, ErrAccountChanged, nil)
		}
	}
	if mutation && w.ID != "" {
		if watcher, ok := b.s.repo.(interface {
			AliasDeletionWorkWanted(context.Context, string) (bool, error)
		}); ok {
			wanted, err := watcher.AliasDeletionWorkWanted(ctx, w.ID)
			if err != nil {
				return err
			}
			if !wanted {
				return context.Canceled
			}
		}
	}
	return nil
}

func (b *aliasDeletionBatch) quotaWait(ctx context.Context) error {
	repo, ok := b.s.repo.(AliasDeletionQuotaRepository)
	if !ok {
		return nil
	}
	quota, err := repo.GetAliasDeletionQuota(ctx, b.subject, b.s.now())
	if err != nil {
		return err
	}
	if !quota.RetryAt.IsZero() {
		return &AliasDeletionWaitError{Reason: "quota", RetryAt: quota.RetryAt, Used: quota.Used, Limit: quota.Limit, Operation: b.operation}
	}
	return nil
}

type aliasDeletionLease struct {
	b         *aliasDeletionBatch
	repo      AliasDeletionQuotaRepository
	id        string
	committed bool
}

func (b *aliasDeletionBatch) reserveQuota(ctx context.Context) (*aliasDeletionLease, error) {
	lease := &aliasDeletionLease{b: b}
	var ok bool
	lease.repo, ok = b.s.repo.(AliasDeletionQuotaRepository)
	if !ok {
		return lease, nil
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	lease.id = hex.EncodeToString(random[:])
	quota, err := lease.repo.ReserveAliasDeletionQuota(ctx, b.subject, lease.id, b.s.now())
	if err != nil {
		return nil, err
	}
	if !quota.RetryAt.IsZero() {
		return nil, &AliasDeletionWaitError{Reason: "quota", RetryAt: quota.RetryAt, Used: quota.Used, Limit: quota.Limit, Operation: "deactivate"}
	}
	return lease, nil
}

func (l *aliasDeletionLease) release(ctx context.Context) error {
	if l.repo == nil || l.committed || l.id == "" {
		return nil
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), aliasDeletePersistTimeout)
	defer cancel()
	err := l.repo.ReleaseAliasDeletionQuota(persistCtx, l.b.subject, l.id)
	if err == nil {
		l.id = ""
	}
	return err
}

func (l *aliasDeletionLease) commit(ctx context.Context) error {
	if l.repo == nil {
		return nil
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), aliasDeletePersistTimeout)
	defer cancel()
	if err := l.repo.CommitAliasDeletionQuota(persistCtx, l.b.subject, l.id, l.b.s.now()); err != nil {
		return err
	}
	l.committed = true
	return nil
}

func (l *aliasDeletionLease) markSent(ctx context.Context) error {
	if l.repo == nil {
		return nil
	}
	if err := l.repo.MarkAliasDeletionQuotaSent(ctx, l.b.subject, l.id, l.b.s.now()); err != nil {
		return err
	}
	l.committed = true
	return nil
}

func (b *aliasDeletionBatch) deferQuota(ctx context.Context, cause error) (*AliasDeletionWaitError, error) {
	delay := apple.RetryDelay(cause)
	if delay <= 0 {
		delay = time.Hour
	}
	result := &AliasDeletionWaitError{Reason: "upstream_rate_limit", RetryAt: b.s.now().Add(delay), Limit: 200, Operation: b.operation, cause: cause}
	var upstream *apple.Error
	if errors.As(cause, &upstream) && upstream != nil {
		result.HTTPStatus, result.ServiceCode = upstream.StatusCode, upstream.ServiceCode
	}
	if repo, ok := b.s.repo.(AliasDeletionQuotaRepository); ok {
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), aliasDeletePersistTimeout)
		defer cancel()
		if err := repo.DeferAliasDeletionQuota(persistCtx, b.subject, result.RetryAt); err != nil {
			return nil, err
		}
		quota, err := repo.GetAliasDeletionQuota(persistCtx, b.subject, b.s.now())
		if err != nil {
			return nil, err
		}
		result.Used, result.Limit = quota.Used, quota.Limit
		if quota.RetryAt.After(result.RetryAt) {
			result.RetryAt = quota.RetryAt
		}
	}
	return result, nil
}
