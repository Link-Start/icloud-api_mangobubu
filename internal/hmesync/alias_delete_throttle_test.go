package hmesync

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

type aliasDeletionMockClock struct {
	mu     sync.Mutex
	value  time.Time
	delays []time.Duration
}

func (c *aliasDeletionMockClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

func (c *aliasDeletionMockClock) wait(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.delays = append(c.delays, delay)
	c.value = c.value.Add(delay)
	return nil
}

type aliasDeletionRecoveryCall struct {
	operation string
	remoteID  string
	at        time.Time
}

// The fixture implements real remote state transitions without network access.
// A hook can apply a mutation and still return 429 to model a lost/ambiguous result.
type aliasDeletionRecoveryFixture struct {
	t         *testing.T
	service   *Service
	repo      *fakeRepository
	client    *fakeAppleClient
	ids       []int64
	directory apple.ListResult
	clock     *aliasDeletionMockClock
	calls     []aliasDeletionRecoveryCall
	hook      func(operation, remoteID string) error
}

func newAliasDeletionRecoveryFixture(t *testing.T, count int) *aliasDeletionRecoveryFixture {
	t.Helper()
	client := &fakeAppleClient{}
	service, repo, ids, directory := newAliasDeletionBatchFixture(t, count, client, newFakeAcquiringLocker())
	f := &aliasDeletionRecoveryFixture{t: t, service: service, repo: repo, client: client, ids: ids, directory: directory}
	f.clock = &aliasDeletionMockClock{value: service.now()}
	WithClock(f.clock.now)(service)
	WithAliasDeletionWaiter(f.clock.wait)(service)
	client.validate = func(ctx context.Context, session apple.Session) (apple.Session, error) {
		return f.request(ctx, session, "validate", "")
	}
	client.list = func(ctx context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
		session, err := f.request(ctx, session, "list", "")
		result := f.directory
		result.Aliases = append([]apple.Alias(nil), result.Aliases...)
		return result, session, err
	}
	client.deactivate = func(ctx context.Context, session apple.Session, id string) (apple.Session, error) {
		session, err := f.request(ctx, session, "deactivate", id)
		if err == nil {
			for i := range f.directory.Aliases {
				if f.directory.Aliases[i].AnonymousID == id {
					f.directory.Aliases[i].IsActive = false
				}
			}
		}
		return session, err
	}
	client.deleteRemote = func(ctx context.Context, session apple.Session, id string) (apple.Session, error) {
		session, err := f.request(ctx, session, "delete", id)
		if err == nil {
			for i, alias := range f.directory.Aliases {
				if alias.AnonymousID == id {
					f.directory.Aliases = append(f.directory.Aliases[:i], f.directory.Aliases[i+1:]...)
					break
				}
			}
		}
		return session, err
	}
	return f
}

func (f *aliasDeletionRecoveryFixture) request(ctx context.Context, session apple.Session, operation, remoteID string) (apple.Session, error) {
	f.t.Helper()
	if ctx.Err() != nil || session.SessionToken != fmt.Sprintf("token-%d", len(f.calls)) {
		f.t.Errorf("%s used a cancelled context or stale session", operation)
	}
	assertStoredAppleSessionToken(f.t, f.service, f.repo, 3, session.SessionToken)
	f.calls = append(f.calls, aliasDeletionRecoveryCall{operation, remoteID, f.clock.now()})
	var err error
	if f.hook != nil {
		err = f.hook(operation, remoteID)
	}
	session.SessionToken = fmt.Sprintf("token-%d", len(f.calls))
	return session, err
}

func (f *aliasDeletionRecoveryFixture) assertPaced() {
	f.t.Helper()
	for i := 1; i < len(f.calls); i++ {
		if gap := f.calls[i].at.Sub(f.calls[i-1].at); gap < time.Second {
			f.t.Errorf("%s -> %s gap=%s, want at least 1s", f.calls[i-1].operation, f.calls[i].operation, gap)
		}
	}
	assertStoredAppleSessionToken(f.t, f.service, f.repo, 3, fmt.Sprintf("token-%d", len(f.calls)))
}

func aliasDeletionThrottle(delay time.Duration) error {
	return &apple.Error{Kind: apple.ErrService, StatusCode: 429, ServiceCode: "-41015", RetryAfter: delay}
}

func TestAliasDeletionRecoveryReadOnlyThrottle(t *testing.T) {
	for _, operation := range []string{"validate", "list"} {
		for _, serverDelay := range []time.Duration{0, 17 * time.Second, 7 * time.Minute} {
			t.Run(operation+"/"+serverDelay.String(), func(t *testing.T) {
				f := newAliasDeletionRecoveryFixture(t, 2)
				limited := false
				var failedAt time.Time
				f.hook = func(op, _ string) error {
					if op == operation && !limited {
						limited, failedAt = true, f.clock.now()
						return fmt.Errorf("wrapped: %w", errors.Join(errors.New("fixture"), aliasDeletionThrottle(serverDelay)))
					}
					return nil
				}
				var waits []AliasDeletionWait
				reports := 0
				ctx := WithAliasDeletionProgress(context.Background(), func(out AliasDeletionOutcome) { reports++ })
				ctx = WithAliasDeletionRecovery(ctx, func(state AliasDeletionWait) {
					waits = append(waits, state)
					if reports != 0 {
						t.Error("current or successor item finalized while still recovering")
					}
				})
				out, err := f.service.DeleteAliases(ctx, f.ids)
				if err != nil || len(out) != 2 || out[0].Err != nil || out[1].Err != nil || reports != 2 {
					t.Fatalf("read recovery: items=%v err=%v reports=%d", out, err, reports)
				}
				if len(waits) != 2 || !waits[0].Waiting || waits[1].Waiting {
					t.Fatalf("wait start/clear=%v", waits)
				}
				wantDelay := serverDelay
				if wantDelay <= 0 {
					wantDelay = time.Hour
				}
				want := AliasDeletionWait{AccountID: 3, AliasID: f.ids[0], Operation: operation,
					RetryAt: failedAt.Add(wantDelay), Attempt: 1, MaxAttempts: 3, Waiting: true, HTTPStatus: 429, ServiceCode: "-41015"}
				if waits[0] != want {
					t.Errorf("wait=%+v want=%+v", waits[0], want)
				}
				want.Waiting = false
				if waits[1] != want {
					t.Errorf("clear=%+v want=%+v", waits[1], want)
				}
				failedIndex := 0
				if operation == "list" {
					failedIndex = 1
				}
				if gap := f.calls[failedIndex+1].at.Sub(f.calls[failedIndex].at); gap != wantDelay {
					t.Errorf("retry gap=%s want=%s", gap, wantDelay)
				}
				wantCalls := 9
				if operation == "list" {
					wantCalls = 10
				}
				if len(f.calls) != wantCalls || f.repo.hasAlias(f.ids[0]) || f.repo.hasAlias(f.ids[1]) {
					t.Error("recovered batch did not finish with bounded calls")
				}
				f.assertPaced()
			})
		}
	}
}

func TestAliasDeletionRecoveryFreshDirectoryPreventsMutationReplay(t *testing.T) {
	for _, operation := range []string{"deactivate", "delete"} {
		for _, applied := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/applied=%v", operation, applied), func(t *testing.T) {
				f := newAliasDeletionRecoveryFixture(t, 2)
				limited := false
				f.hook = func(op, id string) error {
					if op == operation && !limited {
						limited = true
						if applied {
							if operation == "delete" {
								f.directory.Aliases = f.directory.Aliases[1:]
							} else {
								f.directory.Aliases[0].IsActive = false
							}
						}
						// Change the remote IDs of all remaining aliases while cooling.
						for i := range f.directory.Aliases {
							f.directory.Aliases[i].AnonymousID = "fresh-" + f.directory.Aliases[i].AnonymousID
						}
						return aliasDeletionThrottle(5 * time.Minute)
					}
					if limited && (op == "deactivate" || op == "delete") && !strings.HasPrefix(id, "fresh-") {
						t.Error("mutation used stale directory ID after cooldown")
					}
					return nil
				}
				var waits []AliasDeletionWait
				ctx := WithAliasDeletionRecovery(context.Background(), func(state AliasDeletionWait) { waits = append(waits, state) })
				out, err := f.service.DeleteAliases(ctx, f.ids)
				if err != nil || len(out) != 2 || out[0].Err != nil || out[1].Err != nil || len(waits) != 2 {
					t.Fatalf("mutation recovery out=%v err=%v waits=%v", out, err, waits)
				}
				failedIndex := 2
				if operation == "delete" {
					failedIndex = 3
				}
				if next := f.calls[failedIndex+1]; next.operation != "validate" || next.at.Sub(f.calls[failedIndex].at) != 5*time.Minute || f.calls[failedIndex+2].operation != "list" {
					t.Errorf("next call=%v: expected revalidation and fresh read after server cooldown", next)
				}
				wantDeactivate, wantDelete := int32(2), int32(2)
				if !applied {
					if operation == "deactivate" {
						wantDeactivate++
					} else {
						wantDelete++
					}
				}
				if f.client.deactivateCalls.Load() != wantDeactivate || f.client.deleteCalls.Load() != wantDelete {
					t.Errorf("mutation replay: deactivate=%d want=%d delete=%d want=%d", f.client.deactivateCalls.Load(), wantDeactivate, f.client.deleteCalls.Load(), wantDelete)
				}
				f.assertPaced()
			})
		}
	}
}

func TestAliasDeletionRecoveryExhaustionDefersUnattemptedItems(t *testing.T) {
	for _, operation := range []string{"validate", "list", "deactivate", "delete"} {
		t.Run(operation, func(t *testing.T) {
			f := newAliasDeletionRecoveryFixture(t, 416)
			f.hook = func(op, _ string) error {
				if op == operation {
					return aliasDeletionThrottle(0)
				}
				return nil
			}
			var waits []AliasDeletionWait
			reports := 0
			ctx := WithAliasDeletionProgress(context.Background(), func(out AliasDeletionOutcome) { reports++ })
			ctx = WithAliasDeletionRecovery(ctx, func(state AliasDeletionWait) {
				waits = append(waits, state)
				if reports != 0 {
					t.Error("recovering item emitted a terminal outcome")
				}
			})
			out, err := f.service.DeleteAliases(ctx, f.ids)
			if err != nil || len(out) != 416 || reports != 416 || len(waits) != 6 {
				t.Fatalf("exhaustion items=%d reports=%d waits=%v err=%v", len(out), reports, waits, err)
			}
			if Code(out[0].Err) != CodeRateLimited || !errors.Is(out[0].Err, ErrRateLimited) || errors.Is(out[0].Err, ErrBatchDeferred) {
				t.Errorf("trigger item=%v", out[0].Err)
			}
			for _, outcome := range out[1:] {
				if Code(outcome.Err) != "APPLE_BATCH_DEFERRED" || !errors.Is(outcome.Err, ErrBatchDeferred) || !errors.Is(outcome.Err, ErrRateLimited) {
					t.Fatalf("unattempted item %d=%v", outcome.AliasID, outcome.Err)
				}
				if !f.repo.hasAlias(outcome.AliasID) {
					t.Fatal("unattempted alias removed locally")
				}
			}
			limitedCalls := 0
			for _, call := range f.calls {
				if call.operation == operation {
					limitedCalls++
				}
			}
			if limitedCalls != 4 || len(f.calls) > 13 {
				t.Errorf("unbounded calls=%d limited=%d", len(f.calls), limitedCalls)
			}
			for i := range 3 {
				start, clear := waits[2*i], waits[2*i+1]
				if !start.Waiting || clear.Waiting || start.Attempt != i+1 || clear.Attempt != i+1 {
					t.Errorf("attempt %d start=%v clear=%v", i+1, start, clear)
				}
			}
			var cooldowns []time.Duration
			for _, delay := range f.clock.delays {
				if delay >= time.Minute {
					cooldowns = append(cooldowns, delay)
				}
			}
			if !reflect.DeepEqual(cooldowns, []time.Duration{time.Hour, time.Hour, time.Hour}) {
				t.Errorf("fallback delays=%v", cooldowns)
			}
			f.assertPaced()
		})
	}
}

func TestAliasDeletionRecoveryBudgetSharedAcrossSuccessfulItemsAndReconciliation(t *testing.T) {
	f := newAliasDeletionRecoveryFixture(t, 5)
	listFailed := false
	f.hook = func(op, _ string) error {
		if op == "delete" {
			// Two aliases really disappear despite their throttle responses.
			if len(f.directory.Aliases) >= 4 {
				f.directory.Aliases = f.directory.Aliases[1:]
			}
			return aliasDeletionThrottle(0)
		}
		if op == "list" && len(f.calls) > 2 && !listFailed {
			listFailed = true
			return aliasDeletionThrottle(3 * time.Minute)
		}
		return nil
	}
	var starts []AliasDeletionWait
	ctx := WithAliasDeletionRecovery(context.Background(), func(state AliasDeletionWait) {
		if state.Waiting {
			starts = append(starts, state)
		}
	})
	out, err := f.service.DeleteAliases(ctx, f.ids)
	if err != nil || len(out) != 5 || out[0].Err != nil || out[1].Err != nil || Code(out[2].Err) != CodeRateLimited || Code(out[3].Err) != CodeBatchDeferred || Code(out[4].Err) != CodeBatchDeferred {
		t.Fatalf("cross-item shared budget out=%v err=%v", out, err)
	}
	if len(starts) != 3 || starts[0].Operation != "delete" || starts[1].Operation != "list" || starts[2].AliasID != f.ids[1] || starts[2].Attempt != 3 {
		t.Errorf("waits did not share attempts: %v", starts)
	}
	for i := 1; i < len(f.calls); i++ {
		if f.calls[i-1].operation == "list" && f.calls[i].operation == "list" && f.calls[i].at.Sub(f.calls[i-1].at) < 3*time.Minute {
			t.Error("recovery read bypassed Retry-After")
		}
	}
	f.assertPaced()
}

func TestAliasDeletionRecoveryCancellationClearsWaitBeforeOutcomes(t *testing.T) {
	for _, operation := range []string{"validate", "list", "deactivate", "delete"} {
		t.Run(operation, func(t *testing.T) {
			f := newAliasDeletionRecoveryFixture(t, 3)
			f.hook = func(op, _ string) error {
				if op == operation {
					return aliasDeletionThrottle(10 * time.Minute)
				}
				return nil
			}
			base, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			entered := make(chan struct{})
			WithAliasDeletionWaiter(func(ctx context.Context, delay time.Duration) error {
				if delay < time.Minute {
					return f.clock.wait(ctx, delay)
				}
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			})(f.service)
			var reports atomic.Int32
			var waits []AliasDeletionWait
			waiting := false
			ctx := WithAliasDeletionRecovery(base, func(state AliasDeletionWait) {
				waits = append(waits, state)
				waiting = state.Waiting
			})
			ctx = WithAliasDeletionProgress(ctx, func(out AliasDeletionOutcome) {
				if waiting {
					t.Error("terminal outcome while account is waiting")
				}
				reports.Add(1)
			})
			done := make(chan []AliasDeletionOutcome, 1)
			go func() {
				out, err := f.service.DeleteAliases(ctx, f.ids)
				if err != nil {
					t.Errorf("cancelled batch error=%v", err)
				}
				done <- out
			}()
			select {
			case <-entered:
				if reports.Load() != 0 {
					t.Error("waiting item/successors already finalized")
				}
			case <-base.Done():
				t.Fatal("never entered cooldown")
			}
			cancel()
			select {
			case out := <-done:
				if len(out) != 3 || reports.Load() != 3 || len(waits) != 2 || !waits[0].Waiting || waits[1].Waiting {
					t.Fatalf("cancel completion out=%v waits=%v", out, waits)
				}
				for _, outcome := range out {
					if !errors.Is(outcome.Err, context.Canceled) || !f.repo.hasAlias(outcome.AliasID) {
						t.Errorf("cancelled item=%v", outcome)
					}
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation did not interrupt cooldown promptly")
			}
			if f.calls[len(f.calls)-1].operation != operation {
				t.Error("cancelled cooldown sent a reconciliation or mutation request")
			}
			f.assertPaced()
		})
	}
}

func TestAliasDeletionRecoveryProductionTimerHasNoBatchDeadline(t *testing.T) {
	for _, limited := range []bool{false, true} {
		t.Run(fmt.Sprint(limited), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newAliasDeletionRecoveryFixture(t, 2)
				WithClock(time.Now)(f.service)
				WithAliasDeletionWaiter(nil)(f.service)
				var callTimes []time.Time
				f.hook = func(op, _ string) error {
					callTimes = append(callTimes, time.Now())
					if limited {
						return aliasDeletionThrottle(3 * time.Hour)
					}
					return nil
				}
				var waits []AliasDeletionWait
				ctx := WithAliasDeletionRecovery(context.Background(), func(state AliasDeletionWait) { waits = append(waits, state) })
				start := time.Now()
				out, err := f.service.DeleteAliases(ctx, f.ids)
				if err != nil || len(out) != 2 {
					t.Fatalf("production timer out=%v err=%v", out, err)
				}
				if limited {
					if len(callTimes) != 4 || time.Since(start) != 9*time.Hour || len(waits) != 6 || waits[5].Waiting {
						t.Fatalf("long server cooldown calls=%v elapsed=%v waits=%v", callTimes, time.Since(start), waits)
					}
					if waits[0].RetryAt.Sub(start) != 3*time.Hour {
						t.Error("long server delay was shortened to a retry within the job deadline")
					}
					for _, outcome := range out {
						if !errors.Is(outcome.Err, ErrRateLimited) {
							t.Errorf("server throttle result=%v", outcome)
						}
					}
				} else {
					if len(callTimes) != 8 || out[0].Err != nil || out[1].Err != nil || len(waits) != 0 {
						t.Errorf("healthy paced batch out=%v calls=%v waits=%v", out, callTimes, waits)
					}
					for i := 1; i < len(callTimes); i++ {
						if gap := callTimes[i].Sub(callTimes[i-1]); gap != time.Second {
							t.Errorf("production pacing gap=%v", gap)
						}
					}
				}
			})
		})
	}
}

func TestAliasDeletionRecoveryDoesNotRetryNonThrottleErrors(t *testing.T) {
	for _, operation := range []string{"validate", "list", "deactivate", "delete"} {
		t.Run(operation, func(t *testing.T) {
			f := newAliasDeletionRecoveryFixture(t, 1)
			failed := 0
			f.hook = func(op, _ string) error {
				if op == operation {
					failed++
					return &apple.Error{Kind: apple.ErrService, StatusCode: 503, Retryable: true}
				}
				return nil
			}
			waits := 0
			ctx := WithAliasDeletionRecovery(context.Background(), func(state AliasDeletionWait) { waits++ })
			out, err := f.service.DeleteAliases(ctx, f.ids)
			if err != nil || len(out) != 1 || Code(out[0].Err) != CodeUpstreamError || failed != 1 || waits != 0 || !f.repo.hasAlias(f.ids[0]) {
				t.Fatalf("non-throttle was retried out=%v err=%v failures=%d waits=%d", out, err, failed, waits)
			}
			f.assertPaced()
		})
	}
}

func TestAliasDeletionRecoveryFatalFailuresNeverResume(t *testing.T) {
	for _, scenario := range []string{"checkpoint", "returned identity", "expired", "credentials", "terms"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAliasDeletionRecoveryFixture(t, 3)
			failure := errors.New("checkpoint fixture failure")
			f.hook = func(op, _ string) error {
				if op != "deactivate" {
					return nil
				}
				switch scenario {
				case "checkpoint":
					f.repo.upsertSessionErr = failure
				case "expired":
					return errors.Join(apple.ErrInvalidSession, aliasDeletionThrottle(time.Hour))
				case "credentials":
					return errors.Join(apple.ErrAuthentication, aliasDeletionThrottle(time.Hour))
				case "terms":
					return errors.Join(apple.ErrTermsRequired, aliasDeletionThrottle(time.Hour))
				}
				return aliasDeletionThrottle(time.Hour)
			}
			if scenario == "returned identity" {
				f.client.deactivate = func(ctx context.Context, session apple.Session, id string) (apple.Session, error) {
					session, err := f.request(ctx, session, "deactivate", id)
					session.AppleID = "different@example.com"
					return session, err
				}
			}
			waits := 0
			ctx := WithAliasDeletionRecovery(context.Background(), func(state AliasDeletionWait) { waits++ })
			out, err := f.service.DeleteAliases(ctx, f.ids)
			wantWaits := 0
			if scenario == "credentials" || scenario == "terms" {
				wantWaits = 2 // The existing confirmation read must honor its server hint.
			}
			if err != nil || len(out) != 3 || waits != wantWaits || f.client.deactivateCalls.Load() != 1 || f.client.deleteCalls.Load() != 0 {
				t.Fatalf("fatal failure resumed: out=%v err=%v waits=%d", out, err, waits)
			}
			want := failure
			switch scenario {
			case "returned identity":
				want = ErrAccountMismatch
			case "expired":
				want = ErrSessionExpired
			case "credentials":
				want = ErrCredentialsInvalid
			case "terms":
				want = ErrAccountActionRequired
			}
			for i, outcome := range out {
				if scenario == "expired" && i > 0 {
					want = ErrLoginRequired
				}
				if !errors.Is(outcome.Err, want) || errors.Is(outcome.Err, ErrBatchDeferred) || !f.repo.hasAlias(outcome.AliasID) {
					t.Errorf("fatal result=%v want=%v", outcome, want)
				}
			}
		})
	}
}

func TestAliasDeletionRecoveryChecksIdentityAndPendingAfterCooldown(t *testing.T) {
	for _, scenario := range []string{"account identity", "alias ownership", "alias address", "pending", "remote ownership"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAliasDeletionRecoveryFixture(t, 1)
			f.hook = func(op, _ string) error {
				if op == "deactivate" {
					return aliasDeletionThrottle(0)
				}
				return nil
			}
			ctx := WithAliasDeletionRecovery(context.Background(), func(state AliasDeletionWait) {
				if !state.Waiting {
					return
				}
				f.repo.mu.Lock()
				defer f.repo.mu.Unlock()
				alias := f.repo.aliases[f.ids[0]]
				switch scenario {
				case "account identity":
					f.repo.account.Email = "different@example.com"
				case "alias ownership":
					alias.AccountID++
				case "alias address":
					alias.Address = "changed@icloud.com"
				case "pending":
					alias.Enabled, alias.LastSyncError = false, " "+domain.AppleAliasConfirmationPending+" "
				case "remote ownership":
					f.directory.Aliases[0].ForwardToEmail = "foreign@example.com"
				}
				f.repo.aliases[alias.ID] = alias
			})
			out, err := f.service.DeleteAliases(ctx, f.ids)
			want := ErrAccountChanged
			if scenario == "pending" {
				want = ErrAliasConfirmationPending
			} else if scenario == "remote ownership" {
				want = ErrAccountMismatch
			}
			if err != nil || len(out) != 1 || !errors.Is(out[0].Err, want) || !f.repo.hasAlias(f.ids[0]) || f.client.deactivateCalls.Load() != 1 || f.client.deleteCalls.Load() != 0 {
				t.Errorf("stale checks after cooldown out=%v err=%v want=%v", out, err, want)
			}
		})
	}
}

func TestAliasDeletionRecoveryReconciliationRetainsCancellationIndependentConfirmation(t *testing.T) {
	for _, throttleRead := range []bool{false, true} {
		t.Run(fmt.Sprint(throttleRead), func(t *testing.T) {
			f := newAliasDeletionRecoveryFixture(t, 2)
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.hook = func(op, _ string) error {
				if op == "delete" {
					f.directory.Aliases = f.directory.Aliases[1:]
					cancel()
					return ambiguousAliasMutationError("delete")
				}
				if op == "list" && len(f.calls) > 2 && throttleRead {
					return aliasDeletionThrottle(3 * time.Minute)
				}
				return nil
			}
			waits := 0
			ctx := WithAliasDeletionRecovery(base, func(state AliasDeletionWait) { waits++ })
			out, err := f.service.DeleteAliases(ctx, f.ids)
			if err != nil || len(out) != 2 || len(f.calls) != 5 || waits != 0 || !errors.Is(out[1].Err, context.Canceled) {
				t.Fatalf("cancelled reconciliation out=%v err=%v calls=%v", out, err, f.calls)
			}
			if throttleRead {
				if !errors.Is(out[0].Err, context.Canceled) || !f.repo.hasAlias(f.ids[0]) {
					t.Error("cancelled read retry escaped original job context")
				}
			} else if out[0].Err != nil || f.repo.hasAlias(f.ids[0]) {
				t.Error("lost confirmed local cleanup after cancelled ambiguous mutation")
			}
			f.assertPaced()
		})
	}
}

func TestAliasDeletionRecoveryRemainsOptInAndSingleDeletionUnchanged(t *testing.T) {
	for _, single := range []bool{false, true} {
		t.Run(fmt.Sprint(single), func(t *testing.T) {
			f := newAliasDeletionRecoveryFixture(t, 1)
			f.hook = func(op, _ string) error { return aliasDeletionThrottle(time.Minute) }
			waits := 0
			WithAliasDeletionWaiter(func(ctx context.Context, delay time.Duration) error {
				t.Error("legacy/single deletion used recovery waiter")
				return nil
			})(f.service)
			var err error
			if single {
				ctx := WithAliasDeletionRecovery(context.Background(), func(state AliasDeletionWait) { waits++ })
				err = f.service.DeleteAlias(ctx, f.ids[0])
			} else {
				out, batchErr := f.service.DeleteAliases(context.Background(), f.ids)
				if batchErr != nil || len(out) != 1 {
					t.Fatalf("legacy batch err=%v items=%d", batchErr, len(out))
				}
				err = out[0].Err
			}
			if !errors.Is(err, ErrRateLimited) || len(f.calls) != 1 || waits != 0 || !f.repo.hasAlias(f.ids[0]) {
				t.Errorf("opt-in escaped batch boundary: err=%v calls=%d waits=%d", err, len(f.calls), waits)
			}
		})
	}
}

func TestAliasDeletionRecoveryNonThrottleReconciliationSharesServerDelay(t *testing.T) {
	for _, readLimited := range []bool{false, true} {
		t.Run(fmt.Sprint(readLimited), func(t *testing.T) {
			f := newAliasDeletionRecoveryFixture(t, 1)
			reads := 0
			f.hook = func(op, _ string) error {
				if op == "delete" {
					f.directory.Aliases = nil // The mutation was applied despite 503.
					return &apple.Error{Kind: apple.ErrService, StatusCode: 503, RetryAfter: 90 * time.Second}
				}
				if op == "list" {
					reads++
					if reads == 2 && readLimited {
						return aliasDeletionThrottle(5 * time.Minute)
					}
				}
				return nil
			}
			var starts []AliasDeletionWait
			ctx := WithAliasDeletionRecovery(context.Background(), func(state AliasDeletionWait) {
				if state.Waiting {
					starts = append(starts, state)
				}
			})
			out, err := f.service.DeleteAliases(ctx, f.ids)
			if err != nil || len(out) != 1 || out[0].Err != nil || f.client.deleteCalls.Load() != 1 || f.repo.hasAlias(f.ids[0]) {
				t.Fatalf("reconciliation out=%v err=%v", out, err)
			}
			if f.calls[4].operation != "validate" || f.calls[5].operation != "list" || f.calls[4].at.Sub(f.calls[3].at) != 90*time.Second {
				t.Errorf("reconciliation skipped Retry-After: %v", f.calls)
			}
			if len(starts) < 1 || starts[0].Attempt != 0 || starts[0].Operation != "list" || starts[0].HTTPStatus != 503 {
				t.Errorf("read-only server wait=%v", starts)
			}
			if readLimited {
				if len(starts) != 2 || starts[1].Attempt != 1 || len(f.calls) != 8 || f.calls[6].at.Sub(f.calls[5].at) != 5*time.Minute {
					t.Errorf("reconcile retry did not share cooldown: waits=%v calls=%v", starts, f.calls)
				}
			}
			f.assertPaced()
		})
	}
}

func TestAliasDeletionRecoveryWaitCallbackPanicAndWaiterFailureStopWork(t *testing.T) {
	for _, stage := range []string{"waiter error", "waiter panic", "start panic", "clear panic"} {
		t.Run(stage, func(t *testing.T) {
			f := newAliasDeletionRecoveryFixture(t, 3)
			failure := errors.New("fixture wait failed")
			f.hook = func(op, _ string) error { return aliasDeletionThrottle(time.Minute) }
			WithAliasDeletionWaiter(func(ctx context.Context, delay time.Duration) error {
				switch stage {
				case "waiter error":
					return failure
				case "waiter panic":
					panic("fixture-sensitive-waiter-value")
				}
				return f.clock.wait(ctx, delay)
			})(f.service)
			var waits []AliasDeletionWait
			reports := 0
			ctx := WithAliasDeletionProgress(context.Background(), func(out AliasDeletionOutcome) { reports++ })
			ctx = WithAliasDeletionRecovery(ctx, func(state AliasDeletionWait) {
				waits = append(waits, state)
				if (state.Waiting && stage == "start panic") || (!state.Waiting && stage == "clear panic") {
					panic("fixture-sensitive-callback-value")
				}
			})
			out, err := f.service.DeleteAliases(ctx, f.ids)
			if len(out) != 3 || len(f.calls) != 1 || reports != 3 || len(waits) != 2 || !waits[0].Waiting || waits[1].Waiting {
				t.Fatalf("wait failure leaked work: out=%v err=%v waits=%v calls=%v reports=%d", out, err, waits, f.calls, reports)
			}
			if stage == "waiter error" {
				if err != nil || !errors.Is(out[0].Err, failure) {
					t.Error("waiter failure classification lost")
				}
			} else if Code(err) != CodeUpstreamError || strings.Contains(err.Error(), "fixture-sensitive") {
				t.Error("wait panic was not redacted/typed")
			}
			for _, outcome := range out {
				if outcome.Err == nil || !f.repo.hasAlias(outcome.AliasID) {
					t.Error("wait failure finalized an unattempted alias as successful")
				}
			}
		})
	}
}

func TestAliasDeletionRecoveryConcurrentAccountsHaveIndependentWaits(t *testing.T) {
	now := time.Now().UTC()
	base := newFakeRepository(domain.Account{}, now)
	repo := &aliasDeletionAccountsRepository{fakeRepository: base, accounts: make(map[int64]domain.Account)}
	client := &fakeAppleClient{}
	clock := &aliasDeletionMockClock{value: now}
	service := newTestService(t, repo, client, &aliasDeletionKeyedLocker{}, clock.now)
	WithAliasDeletionWaiter(clock.wait)(service)
	ids := []int64{101, 201}
	for id := int64(1); id <= 2; id++ {
		account := domain.Account{ID: id, Email: fmt.Sprintf("account%d@icloud.com", id), MailboxType: domain.MailboxTypeICloud}
		repo.accounts[id] = account
		base.addAlias(domain.Alias{ID: id*100 + 1, AccountID: id, Address: fmt.Sprintf("alias%d@icloud.com", id), Enabled: true})
		storeSession(t, service, base, id, apple.Session{AppleID: account.Email, Region: apple.RegionGlobal, SessionToken: "initial"})
	}
	client.validate = func(ctx context.Context, session apple.Session) (apple.Session, error) {
		if session.SessionToken == "initial" {
			session.SessionToken = "limited"
			return session, aliasDeletionThrottle(0)
		}
		return session, nil
	}
	client.list = func(ctx context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
		// Both aliases already disappeared remotely: successful recovery is read-only.
		return apple.ListResult{SelectedForwardTo: session.AppleID}, session, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	entered := make(chan int64, 2)
	var mu sync.Mutex
	waits := make(map[int64][]AliasDeletionWait)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	ctx = WithAliasDeletionRecovery(ctx, func(state AliasDeletionWait) {
		mu.Lock()
		waits[state.AccountID] = append(waits[state.AccountID], state)
		mu.Unlock()
		if state.Waiting {
			entered <- state.AccountID
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
	})
	done := make(chan []AliasDeletionOutcome, 1)
	go func() {
		out, err := service.DeleteAliases(ctx, ids)
		if err != nil {
			t.Errorf("concurrent batch error=%v", err)
		}
		done <- out
	}()
	seen := make(map[int64]bool)
	for range 2 {
		select {
		case id := <-entered:
			seen[id] = true
		case <-ctx.Done():
			t.Fatal("account wait callbacks were serialized across accounts")
		}
	}
	once.Do(func() { close(release) })
	out := <-done
	if len(seen) != 2 || len(out) != 2 || out[0].Err != nil || out[1].Err != nil {
		t.Fatalf("concurrent account results=%v entered=%v", out, seen)
	}
	for accountID, states := range waits {
		if len(states) != 2 || !states[0].Waiting || states[1].Waiting || states[0].Attempt != 1 || states[0].AliasID != accountID*100+1 {
			t.Errorf("cross-account wait state=%v", states)
		}
	}
}
