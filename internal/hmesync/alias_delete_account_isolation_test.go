package hmesync

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

// This fixture pauses actual cooldown timers, not recovery callbacks. It keeps
// independent remote directories and rolling tokens so scheduler changes cannot
// accidentally make a waiting account look complete or mix account sessions.
type aliasDeletionIsolationFixture struct {
	t         *testing.T
	service   *Service
	repo      *fakeRepository
	locker    *aliasDeletionKeyedLocker
	clock     *aliasDeletionMockClock
	operation string
	ids       []int64
	accounts  map[string]int64
	gates     map[time.Duration]chan struct{}
	waitError error

	initialRequests chan struct{}
	mu              sync.Mutex
	calls           map[int64]int
	limited         map[int64]bool
	directories     map[int64]map[string]apple.Alias
	reports         map[int64][]AliasDeletionOutcome
	waits           map[int64][]AliasDeletionWait
	active          int
	maxActive       int
}

func newAliasDeletionIsolationFixture(t *testing.T, operation string) *aliasDeletionIsolationFixture {
	t.Helper()
	now := time.Now().UTC()
	base := newFakeRepository(domain.Account{}, now)
	repo := &aliasDeletionAccountsRepository{fakeRepository: base, accounts: make(map[int64]domain.Account)}
	client := &fakeAppleClient{}
	f := &aliasDeletionIsolationFixture{
		t: t, repo: base, locker: &aliasDeletionKeyedLocker{}, clock: &aliasDeletionMockClock{value: now}, operation: operation,
		accounts: make(map[string]int64), initialRequests: make(chan struct{}), calls: make(map[int64]int),
		limited: make(map[int64]bool), directories: make(map[int64]map[string]apple.Alias),
		reports: make(map[int64][]AliasDeletionOutcome), waits: make(map[int64][]AliasDeletionWait),
		gates: map[time.Duration]chan struct{}{2 * time.Minute: make(chan struct{}), 3 * time.Minute: make(chan struct{})},
	}
	f.service = newTestService(t, repo, client, f.locker, f.clock.now)
	for accountID := int64(1); accountID <= 4; accountID++ {
		account := domain.Account{ID: accountID, Email: fmt.Sprintf("account%d@icloud.com", accountID), MailboxType: domain.MailboxTypeICloud}
		repo.accounts[accountID], f.accounts[account.Email] = account, accountID
		f.directories[accountID] = make(map[string]apple.Alias)
		for item := int64(1); item <= 2; item++ {
			aliasID := accountID*100 + item
			address, remoteID := fmt.Sprintf("alias%d@icloud.com", aliasID), fmt.Sprint(aliasID)
			f.ids = append(f.ids, aliasID)
			base.addAlias(domain.Alias{ID: aliasID, AccountID: accountID, Address: address, Enabled: true})
			f.directories[accountID][remoteID] = apple.Alias{HME: address, AnonymousID: remoteID, ForwardToEmail: account.Email, IsActive: true}
		}
		storeSession(t, f.service, base, accountID, apple.Session{AppleID: account.Email, Region: apple.RegionGlobal, SessionToken: f.token(accountID, 0)})
	}
	WithAliasDeletionWaiter(func(ctx context.Context, delay time.Duration) error {
		if delay >= time.Minute {
			gate, ok := f.gates[delay]
			if !ok {
				return fmt.Errorf("unexpected cooldown duration %s", delay)
			}
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
			// Only account 1 fails; account 2's independently paused timer must
			// remain alive and later recover normally.
			if delay == 2*time.Minute && f.waitError != nil {
				return f.waitError
			}
		}
		return f.clock.wait(ctx, delay)
	})(f.service)
	client.validate = func(ctx context.Context, session apple.Session) (apple.Session, error) {
		return f.request(ctx, session, "validate", "")
	}
	client.list = func(ctx context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
		returned, err := f.request(ctx, session, "list", "")
		f.mu.Lock()
		defer f.mu.Unlock()
		result := apple.ListResult{SelectedForwardTo: session.AppleID}
		for _, alias := range f.directories[f.accounts[session.AppleID]] {
			result.Aliases = append(result.Aliases, alias)
		}
		return result, returned, err
	}
	client.deactivate = func(ctx context.Context, session apple.Session, remoteID string) (apple.Session, error) {
		return f.request(ctx, session, "deactivate", remoteID)
	}
	client.deleteRemote = func(ctx context.Context, session apple.Session, remoteID string) (apple.Session, error) {
		return f.request(ctx, session, "delete", remoteID)
	}
	return f
}

func (f *aliasDeletionIsolationFixture) token(accountID int64, call int) string {
	return fmt.Sprintf("account-%d-token-%d", accountID, call)
}

func (f *aliasDeletionIsolationFixture) request(ctx context.Context, session apple.Session, operation, remoteID string) (apple.Session, error) {
	accountID := f.accounts[session.AppleID]
	f.locker.mu.Lock()
	if accountID == 0 || f.locker.held[accountID] != 1 {
		f.t.Error("Apple request escaped its account lock")
	}
	f.locker.mu.Unlock()
	f.mu.Lock()
	if session.SessionToken != f.token(accountID, f.calls[accountID]) {
		f.t.Error("Apple request mixed account sessions or reused a stale token")
	}
	f.calls[accountID]++
	call := f.calls[accountID]
	f.active++
	f.maxActive = max(f.maxActive, f.active)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()
	if operation == "validate" && call == 1 {
		select {
		case <-f.initialRequests:
		case <-ctx.Done():
			return session, ctx.Err()
		}
	}
	assertStoredAppleSessionToken(f.t, f.service, f.repo, accountID, session.SessionToken)
	f.mu.Lock()
	defer f.mu.Unlock()
	session.SessionToken = f.token(accountID, call)
	if accountID <= 2 && operation == f.operation && !f.limited[accountID] {
		f.limited[accountID] = true
		return session, aliasDeletionThrottle(time.Duration(accountID+1) * time.Minute)
	}
	switch operation {
	case "deactivate":
		alias, found := f.directories[accountID][remoteID]
		if !found {
			f.t.Error("deactivation used another account's remote alias ID")
		}
		alias.IsActive = false
		f.directories[accountID][remoteID] = alias
	case "delete":
		alias, found := f.directories[accountID][remoteID]
		if !found || alias.IsActive {
			f.t.Error("deletion used another account's remote ID or an active alias")
		}
		delete(f.directories[accountID], remoteID)
	}
	return session, nil
}

type aliasDeletionIsolationRun struct {
	cancel   context.CancelFunc
	done     chan struct{}
	outcomes []AliasDeletionOutcome
	err      error
}

func (f *aliasDeletionIsolationFixture) start(ids []int64) *aliasDeletionIsolationRun {
	ctx, cancel := context.WithCancel(context.Background())
	run := &aliasDeletionIsolationRun{cancel: cancel, done: make(chan struct{})}
	ctx = WithAliasDeletionRecovery(ctx, func(state AliasDeletionWait) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.waits[state.AccountID] = append(f.waits[state.AccountID], state)
	})
	ctx = WithAliasDeletionProgress(ctx, func(outcome AliasDeletionOutcome) {
		if outcome.Err == nil && f.repo.hasAlias(outcome.AliasID) {
			f.t.Error("successful progress was reported before durable deletion")
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.reports[outcome.AliasID] = append(f.reports[outcome.AliasID], outcome)
	})
	go func() {
		defer close(run.done)
		run.outcomes, run.err = f.service.DeleteAliases(ctx, ids)
	}()
	return run
}

func (r *aliasDeletionIsolationRun) stop() {
	r.cancel()
	<-r.done
}

func (f *aliasDeletionIsolationFixture) holdOperationLocks(accountIDs ...int64) func() {
	f.t.Helper()
	var releases []func()
	var once sync.Once
	releaseAll := func() {
		once.Do(func() {
			for _, release := range releases {
				release()
			}
		})
	}
	for _, accountID := range accountIDs {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		release, err := f.service.acquireOperation(ctx, accountID)
		cancel()
		if err != nil {
			releaseAll()
			f.t.Fatalf("could not hold account %d before scheduling: %v", accountID, err)
		}
		releases = append(releases, release)
	}
	return releaseAll
}

func (f *aliasDeletionIsolationFixture) releaseInitialRequests() {
	f.t.Helper()
	synctest.Wait()
	f.mu.Lock()
	active := f.active
	f.mu.Unlock()
	if active != 2 {
		f.t.Errorf("initial concurrent requests=%d, want two", active)
	}
	close(f.initialRequests)
	synctest.Wait()
}

func (f *aliasDeletionIsolationFixture) assertWaiting(accountID int64) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	wantCalls := 1
	if f.operation == "delete" {
		wantCalls = 4 // validate, list, deactivate, throttled delete
	}
	states := f.waits[accountID]
	if f.calls[accountID] != wantCalls || len(states) != 1 || !states[0].Waiting || states[0].AccountID != accountID || states[0].AliasID != accountID*100+1 {
		f.t.Errorf("account %d not independently paused: calls=%d waits=%v", accountID, f.calls[accountID], states)
	}
	for item := int64(1); item <= 2; item++ {
		aliasID := accountID*100 + item
		if len(f.reports[aliasID]) != 0 || !f.repo.hasAlias(aliasID) {
			f.t.Errorf("waiting alias %d received a premature result or was removed", aliasID)
		}
	}
}

func (f *aliasDeletionIsolationFixture) assertReported(ids []int64, wantErr error) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, aliasID := range ids {
		reports := f.reports[aliasID]
		if len(reports) != 1 || !errors.Is(reports[0].Err, wantErr) {
			f.t.Errorf("alias %d progress=%v, want exactly one report with error %v", aliasID, reports, wantErr)
		}
		if f.repo.hasAlias(aliasID) != (wantErr != nil) {
			f.t.Errorf("alias %d durable state disagrees with outcome", aliasID)
		}
	}
}

func (f *aliasDeletionIsolationFixture) assertFinished(run *aliasDeletionIsolationRun, ids []int64) {
	f.t.Helper()
	synctest.Wait()
	select {
	case <-run.done:
	default:
		f.t.Fatal("batch did not finish after cooldown was released or cancelled")
	}
	if run.err != nil || len(run.outcomes) != len(ids) {
		f.t.Fatalf("batch result error=%v outcomes=%v", run.err, run.outcomes)
	}
	for index, aliasID := range ids {
		if run.outcomes[index].AliasID != aliasID {
			f.t.Error("concurrent completion changed input result order")
		}
	}
	if f.maxActive != 2 {
		f.t.Errorf("maximum actual request concurrency=%d, want two", f.maxActive)
	}
}

func (f *aliasDeletionIsolationFixture) assertLocksReleased() {
	f.t.Helper()
	for accountID := int64(1); accountID <= 4; accountID++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		release, err := f.service.acquireOperation(ctx, accountID)
		cancel()
		if err != nil {
			f.t.Fatalf("account %d leaked its Apple operation lock: %v", accountID, err)
		}
		release()
		assertStoredAppleSessionToken(f.t, f.service, f.repo, accountID, f.token(accountID, f.calls[accountID]))
	}
	f.locker.mu.Lock()
	defer f.locker.mu.Unlock()
	if len(f.locker.held) != 0 {
		f.t.Errorf("account lock leaked: held=%v", f.locker.held)
	}
	for accountID, states := range f.waits {
		if len(states) != 2 || !states[0].Waiting || states[1].Waiting || states[0].AccountID != accountID || states[1].AccountID != accountID {
			f.t.Errorf("account %d wait start/clear was not paired: %v", accountID, states)
		}
	}
}

func TestAliasDeletionRecoveryCoolingAccountsDoNotOccupyBatchCapacity(t *testing.T) {
	for _, operation := range []string{"validate", "delete"} {
		t.Run(operation, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newAliasDeletionIsolationFixture(t, operation)
				releaseHealthy := f.holdOperationLocks(3, 4)
				run := f.start(f.ids)
				defer func() {
					releaseHealthy()
					run.stop()
				}()
				f.releaseInitialRequests()
				f.assertWaiting(1)
				f.assertWaiting(2)
				// Hold healthy groups outside the scheduler until both cold
				// accounts have actually reached their paused timers. Otherwise
				// healthy groups could run first and hide a leaked execution slot.
				releaseHealthy()
				synctest.Wait()
				f.assertReported([]int64{301, 302, 401, 402}, nil)
				close(f.gates[2*time.Minute])
				synctest.Wait()
				f.assertReported([]int64{101, 102}, nil)
				f.assertWaiting(2)
				close(f.gates[3*time.Minute])
				f.assertFinished(run, f.ids)
				f.assertReported(f.ids, nil)
				f.assertLocksReleased()
			})
		})
	}
}

func TestAliasDeletionRecoveryWaiterFailureOnlyStopsItsAccount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newAliasDeletionIsolationFixture(t, "delete")
		f.waitError = errors.New("fixture account cooldown failed")
		releaseHealthy := f.holdOperationLocks(3, 4)
		run := f.start(f.ids)
		defer func() {
			releaseHealthy()
			run.stop()
		}()
		f.releaseInitialRequests()
		f.assertWaiting(1)
		f.assertWaiting(2)
		releaseHealthy()
		synctest.Wait()
		f.assertReported([]int64{301, 302, 401, 402}, nil)
		close(f.gates[2*time.Minute])
		synctest.Wait()
		f.assertReported([]int64{101, 102}, f.waitError)
		f.assertReported([]int64{301, 302, 401, 402}, nil)
		f.assertWaiting(2)
		close(f.gates[3*time.Minute])
		f.assertFinished(run, f.ids)
		f.assertReported([]int64{201, 202}, nil)
		if f.calls[1] != 4 {
			t.Error("failed cooldown sent more requests for its account")
		}
		f.assertLocksReleased()
	})
}

func TestAliasDeletionRecoveryCancellingCoolingAccountsPreservesHealthyResults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newAliasDeletionIsolationFixture(t, "delete")
		releaseHealthy := f.holdOperationLocks(3, 4)
		run := f.start(f.ids)
		defer func() {
			releaseHealthy()
			run.stop()
		}()
		f.releaseInitialRequests()
		f.assertWaiting(1)
		f.assertWaiting(2)
		releaseHealthy()
		synctest.Wait()
		f.assertReported([]int64{301, 302, 401, 402}, nil)
		run.cancel()
		f.assertFinished(run, f.ids)
		f.assertReported([]int64{101, 102, 201, 202}, context.Canceled)
		f.assertReported([]int64{301, 302, 401, 402}, nil)
		if f.calls[1] != 4 || f.calls[2] != 4 {
			t.Error("cancelled cooldown started another Apple request")
		}
		f.assertLocksReleased()
	})
}

func TestAliasDeletionRecoveryCooldownReleasesOperationLocksForOtherWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newAliasDeletionIsolationFixture(t, "validate")
		firstIDs := []int64{101, 201}
		first := f.start(firstIDs)
		defer first.stop()
		f.releaseInitialRequests()
		f.assertWaiting(1)
		f.assertWaiting(2)
		// Synchronization and creation use these same operation locks. Neither
		// suspended deletion may retain the lock throughout the cooldown.
		for _, accountID := range []int64{1, 2} {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			release, err := f.service.acquireOperation(ctx, accountID)
			cancel()
			if err != nil {
				t.Fatalf("cooldown blocked account %d: %v", accountID, err)
			}
			release()
		}
		secondIDs := []int64{301, 302, 401, 402}
		second := f.start(secondIDs)
		defer second.stop()
		synctest.Wait()
		f.assertReported([]int64{301, 302, 401, 402}, nil)
		f.assertWaiting(1)
		f.assertWaiting(2)
		second.cancel()
		f.assertFinished(second, secondIDs)
		f.assertReported(secondIDs, nil)
		close(f.gates[2*time.Minute])
		close(f.gates[3*time.Minute])
		f.assertFinished(first, firstIDs)
		f.assertReported(firstIDs, nil)
		f.assertReported([]int64{301, 302, 401, 402}, nil)
		f.assertLocksReleased()
	})
}
