package hmesync

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

type aliasDeletionAccountsRepository struct {
	*fakeRepository
	accounts map[int64]domain.Account
}

func (r *aliasDeletionAccountsRepository) GetAccount(_ context.Context, id int64) (domain.Account, error) {
	if account, ok := r.accounts[id]; ok {
		return account, nil
	}
	return domain.Account{}, store.ErrNotFound
}

type aliasDeletionKeyedLocker struct {
	mu    sync.Mutex
	locks map[int64]*fakeAcquiringLocker
	held  map[int64]int
	max   int
}

func (l *aliasDeletionKeyedLocker) AcquireAccountLock(ctx context.Context, id int64) (func(), error) {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[int64]*fakeAcquiringLocker)
		l.held = make(map[int64]int)
	}
	lock := l.locks[id]
	if lock == nil {
		lock = newFakeAcquiringLocker()
		l.locks[id] = lock
	}
	l.mu.Unlock()
	release, err := lock.AcquireAccountLock(ctx, id)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.held[id]++
	l.max = max(l.max, len(l.held))
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		delete(l.held, id)
		l.mu.Unlock()
		release()
	}, nil
}

func (l *aliasDeletionKeyedLocker) WithAccountLock(ctx context.Context, id int64, operation func() error) error {
	release, err := l.AcquireAccountLock(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	return operation()
}

func TestDeleteAliasesTwoAccountsAtATimeOrderedResultsAndConcurrentProgress(t *testing.T) {
	now := time.Now().UTC()
	base := newFakeRepository(domain.Account{}, now)
	repo := &aliasDeletionAccountsRepository{fakeRepository: base, accounts: make(map[int64]domain.Account)}
	locker := &aliasDeletionKeyedLocker{}
	client := &fakeAppleClient{}
	service := newTestService(t, repo, client, locker, func() time.Time { return now })
	for id := int64(1); id <= 4; id++ {
		account := domain.Account{ID: id, Email: fmt.Sprintf("account%d@icloud.com", id), MailboxType: domain.MailboxTypeICloud}
		repo.accounts[id] = account
		for i := int64(1); i <= 2; i++ {
			base.addAlias(domain.Alias{ID: id*100 + i, AccountID: id, Address: fmt.Sprintf("alias%d-%d@icloud.com", id, i), Enabled: true})
		}
		storeSession(t, service, base, id, apple.Session{AppleID: account.Email, Region: apple.RegionGlobal, SessionToken: "initial"})
	}
	accountID := func(session apple.Session) int64 {
		for id, account := range repo.accounts {
			if session.AppleID == account.Email {
				return id
			}
		}
		t.Error("session crossed account boundary")
		return 0
	}
	assertLock := func(id int64) {
		locker.mu.Lock()
		defer locker.mu.Unlock()
		if locker.held[id] != 1 || len(locker.held) > 2 {
			t.Error("account lock escaped or account concurrency exceeded two")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	entered := make(chan int64, 4)
	progressEntered := make(chan int64, 4)
	releaseValidate := make(chan struct{})
	releaseProgress := make(chan struct{})
	var validateOnce, progressOnce sync.Once
	defer validateOnce.Do(func() { close(releaseValidate) })
	defer progressOnce.Do(func() { close(releaseProgress) })
	var validates, lists atomic.Int32
	client.validate = func(ctx context.Context, session apple.Session) (apple.Session, error) {
		id := accountID(session)
		assertLock(id)
		validates.Add(1)
		entered <- id
		select {
		case <-releaseValidate:
		case <-ctx.Done():
			return session, ctx.Err()
		}
		return session, nil
	}
	client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
		id := accountID(session)
		assertLock(id)
		lists.Add(1)
		result := apple.ListResult{SelectedForwardTo: session.AppleID}
		for i := 1; i <= 2; i++ {
			result.Aliases = append(result.Aliases, apple.Alias{HME: fmt.Sprintf("alias%d-%d@icloud.com", id, i), AnonymousID: fmt.Sprint(i), ForwardToEmail: session.AppleID, IsActive: true})
		}
		return result, session, nil
	}
	client.deactivate = func(_ context.Context, session apple.Session, remote string) (apple.Session, error) {
		assertLock(accountID(session))
		want := "initial"
		if remote == "2" {
			want = "deleted-1"
		}
		if session.SessionToken != want {
			t.Error("same-account requests interleaved or session rolled back")
		}
		session.SessionToken = "inactive-" + remote
		return session, nil
	}
	client.deleteRemote = func(_ context.Context, session apple.Session, remote string) (apple.Session, error) {
		assertLock(accountID(session))
		if session.SessionToken != "inactive-"+remote {
			t.Error("permanent deletion preceded deactivation")
		}
		session.SessionToken = "deleted-" + remote
		return session, nil
	}
	var reportsMu sync.Mutex
	reports := make(map[int64]int)
	ctx = WithAliasDeletionProgress(ctx, func(outcome AliasDeletionOutcome) {
		assertLock(outcome.AliasID / 100)
		if outcome.Err != nil || base.hasAlias(outcome.AliasID) {
			t.Error("progress not completed durably")
		}
		reportsMu.Lock()
		reports[outcome.AliasID]++
		reportsMu.Unlock()
		if outcome.AliasID%100 == 1 {
			progressEntered <- outcome.AliasID / 100
			select {
			case <-releaseProgress:
			case <-ctx.Done():
			}
		}
	})
	ids := []int64{101, 201, 102, 301, 202, 401, 302, 402}
	type result struct {
		outcomes []AliasDeletionOutcome
		err      error
	}
	done := make(chan result, 1)
	go func() { out, err := service.DeleteAliases(ctx, ids); done <- result{out, err} }()
	firstTwo := make(map[int64]bool)
	for range 2 {
		select {
		case id := <-entered:
			firstTwo[id] = true
		case <-ctx.Done():
			t.Fatal("two accounts did not validate concurrently")
		}
	}
	if len(firstTwo) != 2 {
		t.Fatal("same account used two workers")
	}
	select {
	case <-entered:
		t.Error("third account bypassed the worker limit")
	case <-time.After(30 * time.Millisecond):
	}
	validateOnce.Do(func() { close(releaseValidate) })
	progressAccounts := make(map[int64]bool)
	for range 2 {
		select {
		case id := <-progressEntered:
			progressAccounts[id] = true
		case <-ctx.Done():
			t.Fatal("callbacks were buffered until batch completion or serialized globally")
		}
	}
	if len(progressAccounts) != 2 {
		t.Error("same-account callbacks overlapped")
	}
	progressOnce.Do(func() { close(releaseProgress) })
	select {
	case result := <-done:
		if result.err != nil || len(result.outcomes) != len(ids) {
			t.Fatalf("batch result: err=%v items=%d", result.err, len(result.outcomes))
		}
		for i, outcome := range result.outcomes {
			if outcome.AliasID != ids[i] || outcome.Err != nil || reports[ids[i]] != 1 {
				t.Errorf("result order/progress mismatch at %d", i)
			}
		}
	case <-ctx.Done():
		t.Fatal("concurrent batch did not finish")
	}
	if validates.Load() != 4 || lists.Load() != 4 || client.deleteCalls.Load() != 8 || client.deactivateCalls.Load() != 8 || locker.max != 2 {
		t.Error("account concurrency or per-account request counts incorrect")
	}
	for id := int64(1); id <= 4; id++ {
		assertStoredAppleSessionToken(t, service, base, id, "deleted-2")
	}
}

func TestDeleteAliasesHoldsOperationLockAcrossProgressAndCancelsWaiters(t *testing.T) {
	client := &fakeAppleClient{}
	service, repo, ids, full := newAliasDeletionBatchFixture(t, 3, client, &fakeLocker{})
	var validates atomic.Int32
	client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) {
		validates.Add(1)
		return session, nil
	}
	client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
		return full, session, nil
	}
	client.deactivate = func(_ context.Context, session apple.Session, _ string) (apple.Session, error) { return session, nil }
	client.deleteRemote = func(_ context.Context, session apple.Session, _ string) (apple.Session, error) { return session, nil }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	inProgress := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	done := make(chan []AliasDeletionOutcome, 1)
	progressCtx := WithAliasDeletionProgress(ctx, func(outcome AliasDeletionOutcome) {
		if outcome.AliasID == ids[0] {
			close(inProgress)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
	})
	go func() { out, _ := service.DeleteAliases(progressCtx, ids[:2]); done <- out }()
	select {
	case <-inProgress:
	case <-ctx.Done():
		t.Fatal("first item never reported")
	}
	waiterCtx, cancelWaiter := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancelWaiter()
	waiterOut, err := service.DeleteAliases(waiterCtx, ids[2:])
	if err != nil || len(waiterOut) != 1 || !errors.Is(waiterOut[0].Err, context.DeadlineExceeded) {
		t.Error("same-account batch bypassed operation lock")
	}
	singleCtx, cancelSingle := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancelSingle()
	if err := service.DeleteAlias(singleCtx, ids[2]); !errors.Is(err, context.DeadlineExceeded) {
		t.Error("single deletion bypassed batch operation lock")
	}
	if validates.Load() != 1 || client.deleteCalls.Load() != 1 || !repo.hasAlias(ids[2]) {
		t.Error("waiting cancelled operations performed remote side effects")
	}
	once.Do(func() { close(release) })
	select {
	case out := <-done:
		if len(out) != 2 || out[1].Err != nil {
			t.Error("original batch did not finish")
		}
	case <-ctx.Done():
		t.Fatal("original batch failed to release lock")
	}
}

func TestDeleteAliasesRechecksOwnershipAndPendingUnderAccountLock(t *testing.T) {
	for _, state := range []string{"moved", "pending", "identity changed"} {
		t.Run(state, func(t *testing.T) {
			client := &fakeAppleClient{}
			service, repo, ids, full := newAliasDeletionBatchFixture(t, 2, client, &fakeLocker{})
			client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil }
			client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
				return full, session, nil
			}
			client.deactivate = func(_ context.Context, session apple.Session, _ string) (apple.Session, error) { return session, nil }
			client.deleteRemote = func(_ context.Context, session apple.Session, _ string) (apple.Session, error) { return session, nil }
			ctx := WithAliasDeletionProgress(context.Background(), func(outcome AliasDeletionOutcome) {
				if outcome.AliasID != ids[0] {
					return
				}
				// Deliberately bypass the fixture's locker to prove that scheduling
				// snapshots are not trusted at the next item's publication boundary.
				repo.mu.Lock()
				defer repo.mu.Unlock()
				alias := repo.aliases[ids[1]]
				switch state {
				case "moved":
					alias.AccountID = 999
				case "pending":
					alias.Enabled = false
					alias.LastSyncError = domain.AppleAliasConfirmationPending
				case "identity changed":
					repo.account.Email = "changed@icloud.com"
				}
				repo.aliases[ids[1]] = alias
			})
			out, err := service.DeleteAliases(ctx, ids)
			if err != nil || len(out) != 2 || out[0].Err != nil {
				t.Fatal("first outcome lost")
			}
			want := ErrAccountChanged
			if state == "pending" {
				want = ErrAliasConfirmationPending
			}
			if !errors.Is(out[1].Err, want) || !repo.hasAlias(ids[1]) || client.deleteCalls.Load() != 1 || client.deactivateCalls.Load() != 1 {
				t.Error("stale local ownership/pending/identity snapshot caused deletion")
			}
		})
	}
}
