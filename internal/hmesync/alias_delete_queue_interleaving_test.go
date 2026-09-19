package hmesync

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

// This directory changes with every mutation and rotates its session on every
// request. The next deletion must reload changes made by an intervening sync or
// creation, rather than just release a lock while retaining stale remote state.
type deletionInterleavingFixture struct {
	t       *testing.T
	service *Service
	repo    *fakeRepository
	client  *fakeAppleClient
	locker  *fakeAcquiringLocker
	ids     []int64

	mu           sync.Mutex
	directory    map[string]apple.Alias
	requests     int
	beforeDelete func(context.Context, string) error
}

func newDeletionInterleavingFixture(t *testing.T, count int) *deletionInterleavingFixture {
	t.Helper()
	f := &deletionInterleavingFixture{
		t: t, client: &fakeAppleClient{}, locker: newFakeAcquiringLocker(),
		directory: make(map[string]apple.Alias),
	}
	var listed apple.ListResult
	f.service, f.repo, f.ids, listed = newAliasDeletionBatchFixture(t, count, f.client, f.locker)
	for _, alias := range listed.Aliases {
		f.directory[alias.AnonymousID] = alias
	}
	f.client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.advance(session), nil
	}
	f.client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		result := aliasDeletionDirectory()
		for _, alias := range f.directory {
			result.Aliases = append(result.Aliases, alias)
		}
		return result, f.advance(session), nil
	}
	f.client.deactivate = func(_ context.Context, session apple.Session, id string) (apple.Session, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		alias, ok := f.directory[id]
		if !ok || !alias.IsActive {
			t.Error("deletion used a stale directory when deactivating an alias")
		}
		alias.IsActive = false
		f.directory[id] = alias
		return f.advance(session), nil
	}
	f.client.deleteRemote = func(ctx context.Context, session apple.Session, id string) (apple.Session, error) {
		if f.beforeDelete != nil {
			if err := f.beforeDelete(ctx, id); err != nil {
				return session, err
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		alias, ok := f.directory[id]
		if !ok || alias.IsActive {
			t.Error("permanent deletion used a stale remote ID or active alias")
		}
		delete(f.directory, id)
		return f.advance(session), nil
	}
	f.client.create = func(_ context.Context, session apple.Session, label, note string) (apple.Alias, apple.Session, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		created := apple.Alias{
			AnonymousID: "created-between-deletions", HME: "created-between-deletions@icloud.com",
			ForwardToEmail: "primary@icloud.com", IsActive: true, Label: label, Note: note,
		}
		f.directory[created.AnonymousID] = created
		// Apple may rotate another alias's remote ID when returning a new
		// directory. Resumed deletion must use its new authoritative record.
		if len(f.ids) > 1 {
			oldID := fmt.Sprintf("remote-%d", f.ids[1])
			if alias, ok := f.directory[oldID]; ok {
				delete(f.directory, oldID)
				alias.AnonymousID = "refreshed-after-creation"
				f.directory[alias.AnonymousID] = alias
			}
		}
		return created, f.advance(session), nil
	}
	return f
}

// Called only while the fixture mutex is held; every request must consume the
// latest returned token, including requests made after releasing account locks.
func (f *deletionInterleavingFixture) advance(session apple.Session) apple.Session {
	if session.SessionToken != fmt.Sprintf("token-%d", f.requests) {
		f.t.Errorf("request %d reused a stale session after another account operation", f.requests+1)
	}
	f.requests++
	session.SessionToken = fmt.Sprintf("token-%d", f.requests)
	return session
}

type deletionInterleavingBatchResult struct {
	outcomes []AliasDeletionOutcome
	err      error
}

func TestAliasDeletionQueueBatchYieldsToWaitingSyncAndCreate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDeletionInterleavingFixture(t, 3)
		ctx, cancel := context.WithCancel(context.Background())
		var workers sync.WaitGroup
		defer func() { cancel(); workers.Wait() }()
		firstEntered, releaseFirst := make(chan struct{}), make(chan struct{})
		firstReported, releaseBatch := make(chan struct{}), make(chan struct{})
		f.beforeDelete = func(ctx context.Context, id string) error {
			if id != fmt.Sprintf("remote-%d", f.ids[0]) {
				return nil
			}
			close(firstEntered)
			select {
			case <-releaseFirst:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		batchCtx := WithAliasDeletionProgress(ctx, func(outcome AliasDeletionOutcome) {
			if outcome.AliasID == f.ids[0] {
				close(firstReported)
				select {
				case <-releaseBatch:
				case <-ctx.Done():
				}
			}
		})
		batchDone := make(chan deletionInterleavingBatchResult, 1)
		workers.Go(func() {
			outcomes, err := f.service.DeleteAliases(batchCtx, f.ids)
			batchDone <- deletionInterleavingBatchResult{outcomes, err}
		})
		synctest.Wait()
		select {
		case <-firstEntered:
		default:
			t.Fatal("first deletion never reached its remote mutation")
		}

		syncDone, createDone, accountDone := make(chan error, 1), make(chan error, 1), make(chan error, 1)
		workers.Go(func() { _, err := f.service.SyncAliases(ctx, 3); syncDone <- err })
		workers.Go(func() {
			created, err := f.service.CreateAutoAlias(ctx, 3)
			if err == nil && (!created.Enabled || created.Address != "created-between-deletions@icloud.com") {
				err = fmt.Errorf("creation did not confirm the new alias: %+v", created)
			}
			createDone <- err
		})
		workers.Go(func() { accountDone <- f.locker.WithAccountLock(ctx, 3, func() error { return nil }) })
		synctest.Wait()
		for name, done := range map[string]<-chan error{"sync": syncDone, "create": createDone, "account publication": accountDone} {
			select {
			case err := <-done:
				t.Fatalf("%s escaped the active deletion's lock: %v", name, err)
			default:
			}
		}

		close(releaseFirst)
		synctest.Wait()
		select {
		case <-firstReported:
		default:
			t.Fatal("first deletion did not report its durable result")
		}
		for name, done := range map[string]<-chan error{"sync": syncDone, "create": createDone, "account publication": accountDone} {
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("%s failed between deletion items: %v", name, err)
				}
			default:
				t.Fatalf("%s remained blocked until the entire batch completed", name)
			}
		}
		if f.repo.aliasDeletes.Load() != 1 || !f.repo.hasAlias(f.ids[1]) || !f.repo.hasAlias(f.ids[2]) {
			t.Fatal("interleaving happened only after all batch items had completed")
		}

		close(releaseBatch)
		synctest.Wait()
		select {
		case result := <-batchDone:
			if result.err != nil || len(result.outcomes) != len(f.ids) {
				t.Fatalf("batch did not finish after interleaving: %+v", result)
			}
			for i, outcome := range result.outcomes {
				if outcome.Err != nil || outcome.AliasID != f.ids[i] || f.repo.hasAlias(outcome.AliasID) {
					t.Errorf("batch lost order or a durable result: %+v", outcome)
				}
			}
		default:
			t.Fatal("batch stalled after sync and creation completed")
		}
		if f.repo.imports.Load() != 1 || f.repo.confirms.Load() != 1 || len(f.locker.token) != 1 {
			t.Fatal("sync or creation was not published, or an account lock leaked")
		}
		f.mu.Lock()
		finalToken := fmt.Sprintf("token-%d", f.requests)
		_, createdRetained := f.directory["created-between-deletions"]
		f.mu.Unlock()
		if !createdRetained {
			t.Error("continuing the queued deletion removed the newly created alias")
		}
		assertStoredAppleSessionToken(t, f.service, f.repo, 3, finalToken)
	})
}

func TestAliasDeletionQueueMoreThanFourAccountsRunTogetherAndSameAccountSerializes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const accountCount = 6
		now := time.Now().UTC()
		base := newFakeRepository(domain.Account{}, now)
		repo := &aliasDeletionAccountsRepository{fakeRepository: base, accounts: make(map[int64]domain.Account)}
		locker, client := &aliasDeletionKeyedLocker{}, &fakeAppleClient{}
		service := newTestService(t, repo, client, locker, func() time.Time { return now })
		var ids []int64
		accountIDs := make(map[string]int64)
		for accountID := int64(1); accountID <= accountCount; accountID++ {
			account := domain.Account{ID: accountID, Email: fmt.Sprintf("account-%d@icloud.com", accountID), Enabled: true, MailboxType: domain.MailboxTypeICloud}
			repo.accounts[accountID], accountIDs[account.Email] = account, accountID
			for item := int64(1); item <= 3; item++ {
				id := accountID*100 + item
				base.addAlias(domain.Alias{ID: id, AccountID: accountID, Address: fmt.Sprintf("alias-%d@icloud.com", id), Enabled: true})
				if item <= 2 {
					ids = append(ids, id)
				}
			}
			storeSession(t, service, base, accountID, apple.Session{AppleID: account.Email, Region: apple.RegionGlobal, DSID: fmt.Sprintf("dsid-%d", accountID)})
		}
		ctx, cancel := context.WithCancel(context.Background())
		var workers sync.WaitGroup
		defer func() { cancel(); workers.Wait() }()
		entered, release := make(chan int64, 3*accountCount), make(chan struct{})
		var mu sync.Mutex
		calls := make(map[int64]int)
		client.validate = func(ctx context.Context, session apple.Session) (apple.Session, error) {
			accountID := accountIDs[session.AppleID]
			locker.mu.Lock()
			if locker.held[accountID] != 1 {
				t.Error("request was not protected by its shared account lock")
			}
			locker.mu.Unlock()
			mu.Lock()
			calls[accountID]++
			first := calls[accountID] == 1
			mu.Unlock()
			if first {
				entered <- accountID
				select {
				case <-release:
				case <-ctx.Done():
					return session, ctx.Err()
				}
			}
			return session, nil
		}
		client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			accountID := accountIDs[session.AppleID]
			result := apple.ListResult{SelectedForwardTo: session.AppleID, ForwardToEmails: []string{session.AppleID}}
			for item := int64(1); item <= 3; item++ {
				id := accountID*100 + item
				result.Aliases = append(result.Aliases, apple.Alias{AnonymousID: fmt.Sprint(id), HME: fmt.Sprintf("alias-%d@icloud.com", id), ForwardToEmail: session.AppleID, IsActive: true})
			}
			return result, session, nil
		}
		client.deactivate = func(_ context.Context, session apple.Session, _ string) (apple.Session, error) { return session, nil }
		client.deleteRemote = func(_ context.Context, session apple.Session, _ string) (apple.Session, error) { return session, nil }
		batchDone := make(chan deletionInterleavingBatchResult, 1)
		workers.Go(func() {
			outcomes, err := service.DeleteAliases(ctx, ids)
			batchDone <- deletionInterleavingBatchResult{outcomes, err}
		})
		synctest.Wait()
		if len(entered) != accountCount {
			t.Fatalf("only %d of %d independent accounts reached Apple together", len(entered), accountCount)
		}
		seen := make(map[int64]bool)
		for range accountCount {
			seen[<-entered] = true
		}
		if len(seen) != accountCount {
			t.Fatal("the concurrent requests did not belong to distinct accounts")
		}

		secondCtx, cancelSecond := context.WithCancel(ctx)
		defer cancelSecond()
		secondDone := make(chan error, 1)
		workers.Go(func() { secondDone <- service.DeleteAlias(secondCtx, 103) })
		synctest.Wait()
		mu.Lock()
		firstAccountCalls := calls[1]
		mu.Unlock()
		if firstAccountCalls != 1 || client.deactivateCalls.Load() != 0 || client.deleteCalls.Load() != 0 {
			t.Fatal("a second deletion on one account bypassed its active operation")
		}
		cancelSecond()
		synctest.Wait()
		select {
		case err := <-secondDone:
			if !errors.Is(err, context.Canceled) || !base.hasAlias(103) {
				t.Fatalf("cancelled same-account waiter had a side effect: %v", err)
			}
		default:
			t.Fatal("cancelled same-account deletion remained blocked")
		}

		close(release)
		synctest.Wait()
		select {
		case result := <-batchDone:
			if result.err != nil || len(result.outcomes) != len(ids) {
				t.Fatalf("concurrent batch result: %+v", result)
			}
			for i, outcome := range result.outcomes {
				if outcome.Err != nil || outcome.AliasID != ids[i] || base.hasAlias(ids[i]) {
					t.Errorf("concurrent batch outcome: %+v", outcome)
				}
			}
		default:
			t.Fatal("concurrent account work did not finish")
		}
		locker.mu.Lock()
		if len(locker.held) != 0 || locker.max < accountCount {
			t.Errorf("account locks leaked or concurrency capped: held=%v maximum=%d", locker.held, locker.max)
		}
		locker.mu.Unlock()
		service.operationMu.Lock()
		if len(service.operationLock) != 0 {
			t.Error("completed or cancelled work leaked its operation lock")
		}
		service.operationMu.Unlock()
	})
}

// Only the quota surface is promoted from the real store. Local mailbox and
// session operations continue using the existing deterministic service fixture.
type deletionInterleavingQuotaRepository struct {
	*fakeRepository
	AliasDeletionQuotaRepository
}

func TestAliasDeletionQueueFullQuotaReturnsAndLeavesSyncAndCreateAvailable(t *testing.T) {
	f := newDeletionInterleavingFixture(t, 1)
	quotaStore, err := store.Open(filepath.Join(t.TempDir(), "deletion-quota.db"))
	if err != nil {
		t.Fatalf("open quota store: %v", err)
	}
	t.Cleanup(func() { _ = quotaStore.Close() })
	f.service.repo = &deletionInterleavingQuotaRepository{f.repo, quotaStore}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	work, err := f.service.PrepareAliasDeletion(ctx, f.ids[0])
	if err != nil {
		t.Fatalf("prepare queued deletion: %v", err)
	}
	work.ID = "quota-full-work"
	now := f.service.now()
	for index := range 200 {
		reservation := fmt.Sprintf("already-deleted-%d", index)
		quota, err := quotaStore.ReserveAliasDeletionQuota(ctx, work.AppleSubject, reservation, now)
		if err != nil || !quota.RetryAt.IsZero() {
			t.Fatalf("seed used quota %d: quota=%+v error=%v", index, quota, err)
		}
		if err := quotaStore.CommitAliasDeletionQuota(ctx, work.AppleSubject, reservation, now); err != nil {
			t.Fatalf("commit used quota %d: %v", index, err)
		}
	}
	deletionErr := f.service.DeleteQueuedAlias(ctx, work)
	var waiting *AliasDeletionWaitError
	if !errors.As(deletionErr, &waiting) || !errors.Is(deletionErr, ErrRateLimited) {
		t.Fatalf("full quota should return scheduling state immediately: %v", deletionErr)
	}
	if waiting.Used != 200 || waiting.Limit != 200 || !waiting.RetryAt.After(now) {
		t.Fatalf("wrong quota wait: %+v", waiting)
	}
	if ctx.Err() != nil || f.client.deactivateCalls.Load() != 0 || f.client.deleteCalls.Load() != 0 || !f.repo.hasAlias(f.ids[0]) {
		t.Fatal("full quota waited in execution or deactivated an alias before acquiring capacity")
	}
	if err := f.locker.WithAccountLock(ctx, 3, func() error { return nil }); err != nil {
		t.Fatalf("quota wait retained the IMAP/publication lock: %v", err)
	}
	if _, err := f.service.SyncAliases(ctx, 3); err != nil {
		t.Fatalf("quota wait blocked directory synchronization: %v", err)
	}
	created, err := f.service.CreateAutoAlias(ctx, 3)
	if err != nil || !created.Enabled || created.Address != "created-between-deletions@icloud.com" {
		t.Fatalf("quota wait blocked automatic creation: alias=%+v error=%v", created, err)
	}
	quota, err := quotaStore.GetAliasDeletionQuota(ctx, work.AppleSubject, now)
	if err != nil || quota.Used != 200 || !quota.RetryAt.After(now) {
		t.Fatalf("sync/creation disturbed the deletion quota: quota=%+v error=%v", quota, err)
	}
}
