package hmesync

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestAliasDeletionRecoveryReleasesAccountLockDuringCooldown(t *testing.T) {
	for _, operation := range []string{"validate", "list", "deactivate", "delete", "reconcile"} {
		t.Run(operation, func(t *testing.T) {
			f := newAliasDeletionRecoveryFixture(t, 2)
			locker := f.service.locker.(*fakeAcquiringLocker)
			limited := false
			f.hook = func(op, _ string) error {
				if len(locker.token) != 0 {
					t.Error("Apple request escaped the shared account lock")
				}
				if !limited && (op == operation || operation == "reconcile" && op == "delete") {
					limited = true
					if operation == "reconcile" {
						return &apple.Error{Kind: apple.ErrService, StatusCode: 503, RetryAfter: 2 * time.Minute}
					}
					return aliasDeletionThrottle(2 * time.Minute)
				}
				return nil
			}
			waits := 0
			WithAliasDeletionWaiter(func(ctx context.Context, delay time.Duration) error {
				if delay >= time.Minute {
					waits++
					editCtx, cancelEdit := context.WithTimeout(ctx, 250*time.Millisecond)
					defer cancelEdit()
					release, err := locker.AcquireAccountLock(editCtx, 3)
					if err != nil {
						return fmt.Errorf("account setting blocked by cooldown: %w", err)
					}
					defer release()
					// Sync and creation must acquire the Apple operation lock while
					// deletion is cooling; the next deletion reloads their session.
					operationCtx, cancelOperation := context.WithTimeout(ctx, 10*time.Millisecond)
					defer cancelOperation()
					releaseOperation, err := f.service.acquireOperation(operationCtx, 3)
					if releaseOperation != nil {
						releaseOperation()
					}
					if err != nil {
						t.Errorf("another Apple operation was blocked during deletion cooldown: %v", err)
					}
				}
				return f.clock.wait(ctx, delay)
			})(f.service)
			out, err := f.service.DeleteAliases(WithAliasDeletionRecovery(context.Background(), nil), f.ids)
			if err != nil || len(out) != 2 || waits != 1 || out[1].Err != nil {
				t.Fatalf("cooldown recovery out=%v err=%v waits=%d", out, err, waits)
			}
			if operation == "reconcile" {
				if !errors.Is(out[0].Err, ErrUpstream) || !f.repo.hasAlias(f.ids[0]) {
					t.Errorf("non-throttle mutation was replayed: %v", out[0])
				}
			} else if out[0].Err != nil || f.repo.hasAlias(f.ids[0]) {
				t.Errorf("throttled item did not resume: %v", out[0])
			}
			if len(locker.token) != 1 {
				t.Error("batch leaked the shared account lock")
			}
			f.assertPaced()
		})
	}
}

func TestAliasDeletionRecoveryCancellationWhileReacquiringAccountLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newAliasDeletionRecoveryFixture(t, 2)
		locker := f.service.locker.(*fakeAcquiringLocker)
		f.hook = func(_, _ string) error { return aliasDeletionThrottle(time.Minute) }
		var releaseSetting func()
		WithAliasDeletionWaiter(func(ctx context.Context, delay time.Duration) error {
			var err error
			releaseSetting, err = locker.AcquireAccountLock(ctx, 3)
			if err != nil {
				return err
			}
			return f.clock.wait(ctx, delay)
		})(f.service)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var waits []AliasDeletionWait
		ctx = WithAliasDeletionRecovery(ctx, func(state AliasDeletionWait) { waits = append(waits, state) })
		done := make(chan []AliasDeletionOutcome, 1)
		go func() {
			out, err := f.service.DeleteAliases(ctx, f.ids)
			if err != nil {
				t.Errorf("cancelled batch error=%v", err)
			}
			done <- out
		}()
		synctest.Wait()
		if releaseSetting == nil {
			cancel()
			<-done
			t.Fatal("settings could not acquire the account lock during cooldown")
		}
		defer releaseSetting()
		cancel()
		out := <-done
		if len(out) != 2 || len(f.calls) != 1 || len(waits) != 2 || waits[1].Waiting {
			t.Fatalf("cancelled resume out=%v calls=%v waits=%v", out, f.calls, waits)
		}
		for _, outcome := range out {
			if !errors.Is(outcome.Err, context.Canceled) || !f.repo.hasAlias(outcome.AliasID) {
				t.Errorf("cancelled resume outcome=%v", outcome)
			}
		}
		if len(locker.token) != 0 {
			t.Error("cancelled batch released the setting operation's account lock")
		}
		operationCtx, cancelOperation := context.WithTimeout(context.Background(), time.Second)
		defer cancelOperation()
		releaseOperation, err := f.service.acquireOperation(operationCtx, 3)
		if err != nil {
			t.Fatalf("cancelled batch leaked the Apple operation lock: %v", err)
		}
		releaseOperation()
	})
}

func TestAliasDeletionRecoveryRechecksSettingsChangedDuringUnlockedCooldown(t *testing.T) {
	for _, operation := range []string{"validate", "deactivate", "reconcile"} {
		for _, change := range []string{"identity", "ownership", "address", "pending", "removed"} {
			t.Run(operation+"/"+change, func(t *testing.T) {
				f := newAliasDeletionRecoveryFixture(t, 1)
				locker := f.service.locker.(*fakeAcquiringLocker)
				f.hook = func(op, _ string) error {
					if op == operation {
						return aliasDeletionThrottle(time.Minute)
					}
					if operation == "reconcile" && op == "delete" {
						return &apple.Error{Kind: apple.ErrService, StatusCode: 503, RetryAfter: time.Minute}
					}
					return nil
				}
				callsBeforeWait := 0
				WithAliasDeletionWaiter(func(ctx context.Context, delay time.Duration) error {
					if delay >= time.Minute {
						callsBeforeWait = len(f.calls)
						editCtx, cancelEdit := context.WithTimeout(ctx, 250*time.Millisecond)
						defer cancelEdit()
						if err := locker.WithAccountLock(editCtx, 3, func() error {
							f.repo.mu.Lock()
							defer f.repo.mu.Unlock()
							alias := f.repo.aliases[f.ids[0]]
							switch change {
							case "identity":
								f.repo.account.Email = "changed@example.com"
							case "ownership":
								alias.AccountID++
							case "address":
								alias.Address = "changed@icloud.com"
							case "pending":
								alias.Enabled, alias.LastSyncError = false, domain.AppleAliasConfirmationPending
							case "removed":
								delete(f.repo.aliases, alias.ID)
								return nil
							}
							f.repo.aliases[alias.ID] = alias
							return nil
						}); err != nil {
							return err
						}
					}
					return f.clock.wait(ctx, delay)
				})(f.service)
				out, err := f.service.DeleteAliases(WithAliasDeletionRecovery(context.Background(), nil), f.ids)
				want := ErrAccountChanged
				if change == "pending" {
					want = ErrAliasConfirmationPending
				} else if change == "removed" {
					want = store.ErrNotFound
				}
				if err != nil || len(out) != 1 || !errors.Is(out[0].Err, want) {
					t.Fatalf("changed settings out=%v err=%v want=%v", out, err, want)
				}
				if callsBeforeWait == 0 || len(f.calls) != callsBeforeWait || f.repo.aliasDeletes.Load() != 0 {
					t.Fatalf("stale settings caused another request or publication: calls=%v deletes=%d", f.calls, f.repo.aliasDeletes.Load())
				}
				if len(locker.token) != 1 {
					t.Error("changed settings leaked the shared account lock")
				}
			})
		}
	}
}

type aliasDeletionFailingResumeLocker struct {
	*fakeAcquiringLocker
	acquisitions int
	failure      error
}

func (l *aliasDeletionFailingResumeLocker) AcquireAccountLock(ctx context.Context, accountID int64) (func(), error) {
	l.acquisitions++
	if l.acquisitions > 1 {
		return nil, l.failure
	}
	return l.fakeAcquiringLocker.AcquireAccountLock(ctx, accountID)
}

func TestAliasDeletionRecoveryAccountLockFailureStopsRemainingItems(t *testing.T) {
	for _, operation := range []string{"validate", "deactivate", "delete", "reconcile"} {
		t.Run(operation, func(t *testing.T) {
			f := newAliasDeletionRecoveryFixture(t, 3)
			failure := errors.New("account lock unavailable")
			locker := &aliasDeletionFailingResumeLocker{fakeAcquiringLocker: newFakeAcquiringLocker(), failure: failure}
			f.service.locker = locker
			f.hook = func(op, _ string) error {
				if op == operation {
					return aliasDeletionThrottle(time.Minute)
				}
				if operation == "reconcile" && op == "delete" {
					return &apple.Error{Kind: apple.ErrService, StatusCode: 503, RetryAfter: time.Minute}
				}
				return nil
			}
			var waits []AliasDeletionWait
			ctx := WithAliasDeletionRecovery(context.Background(), func(state AliasDeletionWait) { waits = append(waits, state) })
			out, err := f.service.DeleteAliases(ctx, f.ids)
			if err != nil || len(out) != 3 || len(waits) != 2 || waits[1].Waiting || locker.acquisitions != 2 {
				t.Fatalf("account lock failure out=%v err=%v waits=%v acquisitions=%d", out, err, waits, locker.acquisitions)
			}
			for _, outcome := range out {
				if !errors.Is(outcome.Err, failure) || !f.repo.hasAlias(outcome.AliasID) {
					t.Errorf("failed resume outcome=%v", outcome)
				}
			}
			lastOperation := operation
			if operation == "reconcile" {
				lastOperation = "delete"
			}
			if f.calls[len(f.calls)-1].operation != lastOperation || f.repo.aliasDeletes.Load() != 0 || len(locker.token) != 1 {
				t.Fatalf("failed resume sent another request, published, or leaked lock: calls=%v deletes=%d", f.calls, f.repo.aliasDeletes.Load())
			}
			operationCtx, cancelOperation := context.WithTimeout(context.Background(), time.Second)
			defer cancelOperation()
			releaseOperation, err := f.service.acquireOperation(operationCtx, 3)
			if err != nil {
				t.Fatalf("failed resume leaked the Apple operation lock: %v", err)
			}
			releaseOperation()
		})
	}
}
