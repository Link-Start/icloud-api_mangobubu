package hmesync

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

type deletionSafetyRepository struct {
	*fakeRepository
	AliasDeletionQuotaRepository
	wanted bool
}

func (r *deletionSafetyRepository) AliasDeletionWorkWanted(context.Context, string) (bool, error) {
	return r.wanted, nil
}

func (r *deletionSafetyRepository) GetAliasByAddress(_ context.Context, address string) (domain.Alias, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, alias := range r.aliases {
		if sameEmail(alias.Address, address) {
			return alias, nil
		}
	}
	return domain.Alias{}, store.ErrNotFound
}

func newDeletionSafetyFixture(t *testing.T, count int) (*deletionInterleavingFixture, *deletionSafetyRepository, domain.AliasDeletionWork) {
	t.Helper()
	f := newDeletionInterleavingFixture(t, count)
	db, err := store.Open(filepath.Join(t.TempDir(), "quota.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := &deletionSafetyRepository{fakeRepository: f.repo, AliasDeletionQuotaRepository: db, wanted: true}
	f.service.repo = repo
	work, err := f.service.PrepareAliasDeletion(context.Background(), f.ids[0])
	if err != nil {
		t.Fatal(err)
	}
	work.ID = "queue-work"
	return f, repo, work
}

func TestQueuedDeletionConfirmsMissingLocalRowsWithFreshDirectory(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(fmt.Sprint(present), func(t *testing.T) {
			f, repo, work := newDeletionSafetyFixture(t, 1)
			if err := repo.DeleteAlias(context.Background(), work.AliasID); err != nil {
				t.Fatal(err)
			}
			if !present {
				f.directory = make(map[string]apple.Alias)
			}
			work.Reconcile = true
			if err := f.service.DeleteQueuedAlias(context.Background(), work); err != nil {
				t.Fatal(err)
			}
			want := int32(0)
			if present {
				want = 1
			}
			if f.client.deleteCalls.Load() != want || f.requests < 2 {
				t.Fatal("missing local row skipped authoritative confirmation")
			}
		})
	}
}

func TestQueuedDeletionReportsWhetherPausedAttemptReachedMutation(t *testing.T) {
	for _, stage := range []string{"missing session", "validate", "deactivate", "delete"} {
		t.Run(stage, func(t *testing.T) {
			f, repo, work := newDeletionSafetyFixture(t, 1)
			wantErr := ErrSessionExpired
			switch stage {
			case "missing session":
				if err := repo.DeleteAppleWebSession(context.Background(), work.AccountID); err != nil {
					t.Fatal(err)
				}
				wantErr = ErrLoginRequired
			case "validate":
				f.client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) {
					return session, apple.ErrInvalidSession
				}
			case "deactivate":
				f.client.deactivate = func(_ context.Context, session apple.Session, _ string) (apple.Session, error) {
					return session, apple.ErrInvalidSession
				}
			case "delete":
				f.client.deleteRemote = func(_ context.Context, session apple.Session, _ string) (apple.Session, error) {
					return session, apple.ErrInvalidSession
				}
			}
			err := f.service.DeleteQueuedAlias(context.Background(), work)
			var attempt *AliasDeletionAttemptError
			wantMutation := stage == "deactivate" || stage == "delete"
			if !errors.Is(err, wantErr) || !errors.As(err, &attempt) || attempt.MutationAttempted != wantMutation {
				t.Fatalf("paused %s attempt lost mutation state: err=%v attempt=%+v", stage, err, attempt)
			}
			if !repo.hasAlias(work.AliasID) {
				t.Fatal("unconfirmed paused attempt deleted its local row")
			}
			if !wantMutation && (f.client.deactivateCalls.Load() != 0 || f.client.deleteCalls.Load() != 0) {
				t.Fatal("read-only paused attempt sent an Apple mutation")
			}
		})
	}
}

func TestQueuedDeletionRejectsChangedSnapshotAndTransferredAddress(t *testing.T) {
	for _, changed := range []string{"account", "subject", "address", "transferred"} {
		t.Run(changed, func(t *testing.T) {
			f, repo, work := newDeletionSafetyFixture(t, 1)
			switch changed {
			case "account":
				repo.account.Email = "new-owner@icloud.com"
			case "subject":
				storeSession(t, f.service, repo.fakeRepository, 3, apple.Session{AppleID: work.AppleID, Region: apple.RegionGlobal, DSID: "new-subject", SessionToken: "token-0"})
			case "address":
				alias := repo.aliases[work.AliasID]
				alias.Address = "new-address@icloud.com"
				repo.aliases[alias.ID] = alias
			case "transferred":
				delete(repo.aliases, work.AliasID)
				repo.aliases[999] = domain.Alias{ID: 999, AccountID: 99, Address: work.Address}
			}
			if err := f.service.DeleteQueuedAlias(context.Background(), work); !errors.Is(err, ErrAccountChanged) {
				t.Fatalf("changed snapshot error=%v", err)
			}
			if f.requests != 0 || f.client.deleteCalls.Load() != 0 {
				t.Fatal("changed snapshot reached Apple")
			}
		})
	}
}

func TestQueuedDeletionCancellationBetweenMutationsReleasesUnusedQuota(t *testing.T) {
	f, repo, work := newDeletionSafetyFixture(t, 1)
	deactivate := f.client.deactivate
	f.client.deactivate = func(ctx context.Context, session apple.Session, id string) (apple.Session, error) {
		session, err := deactivate(ctx, session, id)
		repo.wanted = false
		return session, err
	}
	if err := f.service.DeleteQueuedAlias(context.Background(), work); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
	quota, err := repo.GetAliasDeletionQuota(context.Background(), work.AppleSubject, f.service.now())
	if err != nil || quota.Used != 0 || f.client.deleteCalls.Load() != 0 || !repo.hasAlias(work.AliasID) {
		t.Fatalf("cancel started permanent delete or leaked quota: %+v %v", quota, err)
	}
	// Once an already-started request has disappeared remotely, cancelled work
	// still confirms the result instead of leaving its local row behind forever.
	f.directory = make(map[string]apple.Alias)
	work.Reconcile = true
	if err := f.service.DeleteQueuedAlias(context.Background(), work); err != nil || repo.hasAlias(work.AliasID) {
		t.Fatalf("cancelled reconciliation error=%v", err)
	}
}

func TestEveryDeletionEntrySharesRollingQuota(t *testing.T) {
	f, repo, work := newDeletionSafetyFixture(t, 3)
	ctx := context.Background()
	for index := range 199 {
		id := fmt.Sprintf("already-deleted-%d", index)
		if _, err := repo.ReserveAliasDeletionQuota(ctx, work.AppleSubject, id, f.service.now()); err != nil {
			t.Fatal(err)
		}
		if err := repo.CommitAliasDeletionQuota(ctx, work.AppleSubject, id, f.service.now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.service.DeleteAlias(ctx, work.AliasID); err != nil {
		t.Fatal(err)
	}
	out, err := f.service.DeleteAliases(ctx, f.ids[1:2])
	var wait *AliasDeletionWaitError
	if err != nil || len(out) != 1 || !errors.As(out[0].Err, &wait) || wait.Used != 200 {
		t.Fatalf("batch bypassed single deletion quota: %v %v", out, err)
	}
	next, err := f.service.PrepareAliasDeletion(ctx, f.ids[2])
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.DeleteQueuedAlias(ctx, next); !errors.As(err, &wait) || wait.Used != 200 {
		t.Fatalf("queue bypassed shared quota: %v", err)
	}
	if f.client.deleteCalls.Load() != 1 || f.client.deactivateCalls.Load() != 1 {
		t.Fatal("full quota allowed another mutation")
	}
}

func TestQueuedDeletionPersistsUpstreamCooldownBeforeReturning(t *testing.T) {
	f, repo, work := newDeletionSafetyFixture(t, 2)
	f.client.deactivate = func(_ context.Context, session apple.Session, _ string) (apple.Session, error) {
		return f.advance(session), aliasDeletionThrottle(0)
	}
	err := f.service.DeleteQueuedAlias(context.Background(), work)
	var wait *AliasDeletionWaitError
	if !errors.As(err, &wait) || wait.Reason != "upstream_rate_limit" || wait.RetryAt.Sub(f.service.now()) < time.Hour {
		t.Fatalf("upstream was not deferred: %v", err)
	}
	requests := f.requests
	if err := f.service.DeleteAlias(context.Background(), f.ids[1]); !errors.As(err, &wait) {
		t.Fatalf("single deletion bypassed cooldown: %v", err)
	}
	if f.requests != requests {
		t.Fatal("shared cooldown performed more Apple requests")
	}
	quota, err := repo.GetAliasDeletionQuota(context.Background(), work.AppleSubject, f.service.now())
	if err != nil || quota.Used != 0 || quota.RetryAt.IsZero() {
		t.Fatalf("unused reservation or cooldown incorrect: %+v %v", quota, err)
	}
}

func TestQueuedDeletionUnknownPermanentResultRemainsCounted(t *testing.T) {
	f, repo, work := newDeletionSafetyFixture(t, 1)
	f.client.deleteRemote = func(_ context.Context, session apple.Session, _ string) (apple.Session, error) {
		return f.advance(session), ambiguousAliasMutationError("delete")
	}
	if err := f.service.DeleteQueuedAlias(context.Background(), work); !errors.Is(err, ErrUpstream) {
		t.Fatalf("ambiguous error=%v", err)
	}
	quota, err := repo.GetAliasDeletionQuota(context.Background(), work.AppleSubject, f.service.now())
	if err != nil || quota.Used != 1 || !repo.hasAlias(work.AliasID) {
		t.Fatalf("unknown result released quota: %+v %v", quota, err)
	}
	// A later attempt reconciles afresh and needs its own allowance if the
	// permanent request is sent again, even when Work.ID and Attempts repeat.
	work.Reconcile = true
	if err := f.service.DeleteQueuedAlias(context.Background(), work); !errors.Is(err, ErrUpstream) {
		t.Fatalf("repeat ambiguous error=%v", err)
	}
	quota, err = repo.GetAliasDeletionQuota(context.Background(), work.AppleSubject, f.service.now())
	if err != nil || quota.Used != 2 {
		t.Fatalf("new permanent attempt reused old allowance: %+v %v", quota, err)
	}
}

func TestQueuedDeletionRejectsReturnedAppleSubjectChange(t *testing.T) {
	f, repo, work := newDeletionSafetyFixture(t, 1)
	f.client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) {
		session.DSID = "different-account"
		return session, nil
	}
	if err := f.service.DeleteQueuedAlias(context.Background(), work); !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("returned subject error=%v", err)
	}
	if f.client.deactivateCalls.Load() != 0 || f.client.deleteCalls.Load() != 0 || !repo.hasAlias(work.AliasID) {
		t.Fatal("changed Apple subject reached mutation")
	}
	_, session, err := f.service.loadSession(context.Background(), work.AccountID)
	if err != nil || session.DSID == "different-account" {
		t.Fatal("mismatched subject replaced trusted session")
	}
}

func TestQueuedDeletionCheckpointFailureStillPersistsCooldown(t *testing.T) {
	f, repo, work := newDeletionSafetyFixture(t, 2)
	failure := errors.New("checkpoint failure")
	f.client.deactivate = func(_ context.Context, session apple.Session, _ string) (apple.Session, error) {
		repo.upsertSessionErr = failure
		return f.advance(session), aliasDeletionThrottle(0)
	}
	if err := f.service.DeleteQueuedAlias(context.Background(), work); !errors.Is(err, failure) {
		t.Fatalf("checkpoint error=%v", err)
	}
	quota, err := repo.GetAliasDeletionQuota(context.Background(), work.AppleSubject, f.service.now())
	if err != nil || quota.Used != 0 || quota.RetryAt.Sub(f.service.now()) != time.Hour {
		t.Fatalf("checkpoint failure erased shared cooldown: %+v %v", quota, err)
	}
	var wait *AliasDeletionWaitError
	requests := f.requests
	if err := f.service.DeleteAlias(context.Background(), f.ids[1]); !errors.As(err, &wait) || f.requests != requests {
		t.Fatalf("failed checkpoint allowed another deletion to bypass cooldown: %v", err)
	}
}

func TestDeleteAliasesThousandItemsContinueAcrossHourlyQuotaWindows(t *testing.T) {
	f, repo, work := newDeletionSafetyFixture(t, 1000)
	clock := &aliasDeletionMockClock{value: f.service.now()}
	WithClock(clock.now)(f.service)
	WithAliasDeletionWaiter(clock.wait)(f.service)
	started := clock.now()
	waits := 0
	ctx := WithAliasDeletionRecovery(context.Background(), func(state AliasDeletionWait) {
		if state.Waiting {
			waits++
			if state.Attempt != 0 {
				t.Error("normal hourly quota consumed upstream recovery budget")
			}
		}
	})
	out, err := f.service.DeleteAliases(ctx, f.ids)
	if err != nil || len(out) != 1000 {
		t.Fatalf("1000-item batch: %d %v", len(out), err)
	}
	for _, item := range out {
		if item.Err != nil || repo.hasAlias(item.AliasID) {
			t.Fatalf("unfinished queued item: %+v", item)
		}
	}
	quota, err := repo.GetAliasDeletionQuota(context.Background(), work.AppleSubject, clock.now())
	if err != nil || quota.Used > 200 || f.client.deleteCalls.Load() != 1000 || waits < 4 || clock.now().Sub(started) < 4*time.Hour {
		t.Fatalf("quota progression: used=%d deleted=%d waits=%d duration=%v err=%v", quota.Used, f.client.deleteCalls.Load(), waits, clock.now().Sub(started), err)
	}
}
