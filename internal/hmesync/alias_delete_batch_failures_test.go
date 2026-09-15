package hmesync

import (
	"context"
	"errors"
	"strings"
	"testing"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestDeleteAliasesBlockedAccountPreservesPartialCompletion(t *testing.T) {
	for _, scenario := range []string{"expired", "cleanup failed", "rate limited", "checkpoint failed", "identity mismatch", "local failed"} {
		t.Run(scenario, func(t *testing.T) {
			client := &fakeAppleClient{}
			service, repo, ids, full := newAliasDeletionBatchFixture(t, 3, client, newFakeAcquiringLocker())
			lists := 0
			failure := errors.New("fixture persistence error")
			client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil }
			client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
				lists++
				if lists > 1 {
					if scenario != "rate limited" || session.SessionToken != "second-returned" {
						t.Error("unexpected reconciliation or stale session")
					}
					session.SessionToken = "rate-reconciled"
				}
				return full, session, nil
			}
			client.deactivate = func(_ context.Context, session apple.Session, id string) (apple.Session, error) { return session, nil }
			client.deleteRemote = func(_ context.Context, session apple.Session, id string) (apple.Session, error) {
				if id == "remote-41" {
					session.SessionToken = "first-deleted"
					return session, nil
				}
				if id == "remote-43" {
					if scenario != "local failed" || session.SessionToken != "second-returned" {
						t.Error("blocked account started another mutation or reloaded stale session")
					}
					return session, nil
				}
				session.SessionToken = "second-returned"
				switch scenario {
				case "expired", "cleanup failed":
					if scenario == "cleanup failed" {
						repo.deleteSessionErr = failure
					}
					return session, apple.ErrInvalidSession
				case "rate limited":
					return session, &apple.Error{Kind: apple.ErrService, StatusCode: 429}
				case "checkpoint failed":
					repo.upsertSessionErr = failure
				case "identity mismatch":
					session.AppleID = "different@example.com"
				}
				return session, nil
			}
			repo.deleteAliasFn = func(_ context.Context, id int64) error {
				if scenario == "local failed" && id == ids[1] {
					return failure
				}
				return nil
			}
			reports := make(map[int64]int)
			ctx := WithAliasDeletionProgress(context.Background(), func(outcome AliasDeletionOutcome) { reports[outcome.AliasID]++ })
			outcomes, err := service.DeleteAliases(ctx, ids)
			if err != nil || len(outcomes) != 3 || outcomes[0].Err != nil || repo.hasAlias(ids[0]) || !repo.hasAlias(ids[1]) {
				t.Fatalf("partial results lost: err=%v items=%d", err, len(outcomes))
			}
			for _, id := range ids {
				if reports[id] != 1 {
					t.Errorf("item %d reports=%d", id, reports[id])
				}
			}
			want := failure
			switch scenario {
			case "expired", "cleanup failed":
				want = ErrSessionExpired
			case "rate limited":
				want = ErrRateLimited
			case "identity mismatch":
				want = ErrAccountMismatch
			}
			if !errors.Is(outcomes[1].Err, want) {
				t.Error("failing item lost its typed error")
			}
			if scenario == "expired" {
				if repo.sessionCount() != 0 || !errors.Is(outcomes[2].Err, ErrLoginRequired) || !errors.Is(outcomes[2].Err, store.ErrNotFound) {
					t.Error("expired session was reused or cleanup classification was lost")
				}
			} else if scenario == "local failed" {
				if outcomes[2].Err != nil || repo.hasAlias(ids[2]) {
					t.Error("local item failure stopped a healthy rolling session")
				}
			} else if !errors.Is(outcomes[2].Err, want) {
				t.Error("blocked account's remaining item was not finalized")
			}
			wantCalls := int32(2)
			if scenario == "local failed" {
				wantCalls = 3
			}
			if client.deleteCalls.Load() != wantCalls || client.deactivateCalls.Load() != wantCalls {
				t.Error("unexpected remote side effects after account failure")
			}
			if scenario == "rate limited" {
				if lists != 2 {
					t.Error("rate-limited mutation did not receive exactly one read-only reconciliation")
				}
				assertStoredAppleSessionToken(t, service, repo, 3, "rate-reconciled")
			}
			if scenario == "checkpoint failed" || scenario == "identity mismatch" || scenario == "cleanup failed" {
				assertStoredAppleSessionToken(t, service, repo, 3, "first-deleted")
			}
		})
	}
}

func TestDeleteAliasesCancellationStopsNewSideEffectsAndFinalizesEveryItem(t *testing.T) {
	for _, stage := range []string{"before batch", "validate", "list", "deactivate", "ambiguous deactivate", "delete", "ambiguous delete", "progress"} {
		t.Run(stage, func(t *testing.T) {
			client := &fakeAppleClient{}
			service, repo, ids, full := newAliasDeletionBatchFixture(t, 3, client, newFakeAcquiringLocker())
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			validates, lists, reports := 0, 0, 0
			client.validate = func(ctx context.Context, session apple.Session) (apple.Session, error) {
				validates++
				if ctx.Err() != nil {
					t.Error("started validation after cancellation")
				}
				if stage == "validate" {
					cancel()
				}
				session.SessionToken = "validated"
				return session, nil
			}
			client.list = func(ctx context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
				lists++
				if ctx.Err() != nil {
					t.Error("reconciliation did not get a bounded live context")
				}
				result := full
				if lists > 1 {
					result.Aliases = append([]apple.Alias(nil), full.Aliases...)
					if stage == "ambiguous delete" {
						result.Aliases = result.Aliases[1:]
					} else {
						result.Aliases[0].IsActive = false
					}
				}
				if stage == "list" {
					cancel()
				}
				session.SessionToken = "listed"
				return result, session, nil
			}
			client.deactivate = func(ctx context.Context, session apple.Session, _ string) (apple.Session, error) {
				if ctx.Err() != nil {
					t.Error("started deactivation after cancellation")
				}
				session.SessionToken = "deactivated"
				if stage == "deactivate" || stage == "ambiguous deactivate" {
					cancel()
				}
				if stage == "ambiguous deactivate" {
					return session, ambiguousAliasMutationError("deactivate")
				}
				return session, nil
			}
			client.deleteRemote = func(ctx context.Context, session apple.Session, _ string) (apple.Session, error) {
				if ctx.Err() != nil {
					t.Error("started permanent deletion after cancellation")
				}
				session.SessionToken = "deleted"
				if stage == "delete" || stage == "ambiguous delete" {
					cancel()
				}
				if stage == "ambiguous delete" {
					return session, ambiguousAliasMutationError("delete")
				}
				return session, nil
			}
			repo.deleteAliasFn = func(ctx context.Context, _ int64) error {
				if ctx.Err() != nil {
					t.Error("confirmed deletion used cancelled persistence context")
				}
				return nil
			}
			if stage == "before batch" {
				cancel()
			}
			ctx := WithAliasDeletionProgress(base, func(outcome AliasDeletionOutcome) {
				if reports >= len(ids) || outcome.AliasID != ids[reports] {
					t.Error("cancelled outcomes were reordered/duplicated")
				}
				if outcome.Err == nil && repo.hasAlias(outcome.AliasID) {
					t.Error("reported completion before durable cleanup")
				}
				reports++
				if stage == "progress" {
					cancel()
				}
			})
			outcomes, err := service.DeleteAliases(ctx, ids)
			if err != nil || len(outcomes) != 3 || reports != 3 {
				t.Fatalf("cancel results: err=%v items=%d reports=%d", err, len(outcomes), reports)
			}
			firstDone := stage == "delete" || stage == "ambiguous delete" || stage == "progress"
			if repo.hasAlias(ids[0]) == firstDone || (outcomes[0].Err == nil) != firstDone {
				t.Error("started deletion was not finalized correctly")
			}
			for _, outcome := range outcomes[1:] {
				if !errors.Is(outcome.Err, context.Canceled) || !repo.hasAlias(outcome.AliasID) {
					t.Error("new item ran after cancellation")
				}
			}
			wantValidate, wantList, wantDeactivate, wantDelete := 1, 1, int32(1), int32(0)
			wantToken := "deactivated"
			switch stage {
			case "before batch":
				wantValidate, wantList, wantDeactivate, wantToken = 0, 0, 0, "token-0"
			case "validate":
				wantList, wantDeactivate, wantToken = 0, 0, "validated"
			case "list":
				wantDeactivate, wantToken = 0, "listed"
			case "ambiguous deactivate":
				wantList, wantToken = 2, "listed"
			case "delete", "progress":
				wantDelete, wantToken = 1, "deleted"
			case "ambiguous delete":
				wantDelete, wantList, wantToken = 1, 2, "listed"
			}
			if validates != wantValidate || lists != wantList || client.deactivateCalls.Load() != wantDeactivate || client.deleteCalls.Load() != wantDelete {
				t.Errorf("requests after %s cancellation: validate=%d list=%d deactivate=%d delete=%d", stage, validates, lists, client.deactivateCalls.Load(), client.deleteCalls.Load())
			}
			assertStoredAppleSessionToken(t, service, repo, 3, wantToken)
		})
	}
}

func TestDeleteAliasesProgressValidationAndFallback(t *testing.T) {
	client := &fakeAppleClient{}
	service, repo, ids, _ := newAliasDeletionBatchFixture(t, 1, client, &fakeLocker{})
	repo.addAlias(domain.Alias{ID: ids[0], AccountID: 3, Address: "pending@icloud.com", LastSyncError: domain.AppleAliasConfirmationPending})
	client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil }
	client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
		return aliasDeletionDirectory(), session, nil
	}
	inputs := []int64{ids[0], 0, ids[0], 999, -1}
	var reports []AliasDeletionOutcome
	ctx := WithAliasDeletionProgress(context.Background(), func(outcome AliasDeletionOutcome) { reports = append(reports, outcome) })
	outcomes, err := service.DeleteAliases(ctx, inputs)
	if err != nil || len(outcomes) != len(inputs) || len(reports) != len(inputs) {
		t.Fatal("invalid/duplicate/missing items did not each report")
	}
	for i, id := range inputs {
		if outcomes[i].AliasID != id || (i != 0 && outcomes[i].Err == nil) {
			t.Errorf("invalid result at index %d", i)
		}
		matches := 0
		for _, report := range reports {
			if report.AliasID == id && report.Err == outcomes[i].Err {
				matches++
			}
		}
		if matches != 1 {
			t.Errorf("index %d reported %d times", i, matches)
		}
	}
	if outcomes[0].Err != nil || repo.hasAlias(ids[0]) || repo.aliasDeletes.Load() != 1 {
		t.Errorf("pending alias was not treated as idempotent deletion: outcome=%#v exists=%v deletes=%d", outcomes[0], repo.hasAlias(ids[0]), repo.aliasDeletes.Load())
	}
	if !errors.Is(outcomes[3].Err, store.ErrNotFound) {
		t.Error("missing alias classification lost")
	}
	ReportAliasDeletionProgress(ctx, AliasDeletionOutcome{AliasID: 123})
	ReportAliasDeletionProgress(WithAliasDeletionProgress(ctx, nil), AliasDeletionOutcome{AliasID: 124})
	ReportAliasDeletionProgress(context.Background(), AliasDeletionOutcome{AliasID: 125})
	if len(reports) != len(inputs)+1 || reports[len(inputs)].AliasID != 123 {
		t.Error("fallback reporter/nil context reporter contract broken")
	}
	if out, err := service.DeleteAliases(ctx, nil); err == nil || out != nil || len(reports) != len(inputs)+1 {
		t.Error("empty batch should reject without reporting a nonexistent input")
	}
}

func TestDeleteAliasesPanicsFinalizeWithoutLeakingOrRepeatingCallbacks(t *testing.T) {
	for _, stage := range []string{"lookup", "deactivate", "delete", "local", "progress"} {
		t.Run(stage, func(t *testing.T) {
			client := &fakeAppleClient{}
			locker := newFakeAcquiringLocker()
			service, repo, ids, full := newAliasDeletionBatchFixture(t, 3, client, locker)
			panicValue := "fixture-sensitive-session-value"
			client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil }
			client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
				return full, session, nil
			}
			client.deactivate = func(_ context.Context, session apple.Session, id string) (apple.Session, error) {
				if stage == "deactivate" && id == "remote-42" {
					panic(panicValue)
				}
				return session, nil
			}
			client.deleteRemote = func(_ context.Context, session apple.Session, id string) (apple.Session, error) {
				if stage == "delete" && id == "remote-42" {
					panic(panicValue)
				}
				return session, nil
			}
			repo.getAliasFn = func(_ context.Context, id int64) error {
				if stage == "lookup" && id == ids[1] {
					panic(panicValue)
				}
				return nil
			}
			repo.deleteAliasFn = func(_ context.Context, id int64) error {
				if stage == "local" && id == ids[1] {
					panic(panicValue)
				}
				return nil
			}
			reports := make(map[int64]int)
			ctx := WithAliasDeletionProgress(context.Background(), func(outcome AliasDeletionOutcome) {
				reports[outcome.AliasID]++
				if stage == "progress" && outcome.AliasID == ids[1] {
					panic(panicValue)
				}
			})
			outcomes, err := service.DeleteAliases(ctx, ids)
			if !errors.Is(err, ErrUpstream) || Code(err) != CodeUpstreamError || strings.Contains(err.Error(), panicValue) || len(outcomes) != 3 {
				t.Fatal("panic did not produce a redacted typed batch failure")
			}
			for i, outcome := range outcomes {
				if outcome.AliasID != ids[i] || reports[ids[i]] != 1 {
					t.Error("panic lost/repeated an input outcome")
				}
				if outcome.Err != nil && strings.Contains(outcome.Err.Error(), panicValue) {
					t.Error("panic value leaked into item outcome")
				}
			}
			if stage != "lookup" && (outcomes[0].Err != nil || repo.hasAlias(ids[0])) {
				t.Error("panic overwrote earlier completed deletion")
			}
			if stage == "progress" && (outcomes[1].Err != nil || repo.hasAlias(ids[1])) {
				t.Error("callback panic overwrote completed deletion")
			}
			if outcomes[2].Err == nil || !repo.hasAlias(ids[2]) {
				t.Error("new deletion started after panic")
			}
			if len(locker.token) != 1 || len(service.operationLock) != 0 {
				t.Error("worker panic leaked account/operation lock")
			}
		})
	}
}

func TestDeleteAliasesThousandItemsAfterProgressFailureReportExactlyOnce(t *testing.T) {
	for _, panicCallback := range []bool{false, true} {
		name := "cancel"
		if panicCallback {
			name = "panic"
		}
		t.Run(name, func(t *testing.T) {
			client := &fakeAppleClient{}
			service, repo, ids, full := newAliasDeletionBatchFixture(t, 1000, client, &fakeLocker{})
			client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil }
			client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
				return full, session, nil
			}
			client.deactivate = func(_ context.Context, session apple.Session, _ string) (apple.Session, error) { return session, nil }
			client.deleteRemote = func(_ context.Context, session apple.Session, _ string) (apple.Session, error) { return session, nil }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reports := make(map[int64]int)
			ctx = WithAliasDeletionProgress(ctx, func(outcome AliasDeletionOutcome) {
				reports[outcome.AliasID]++
				if panicCallback {
					panic("fixture-sensitive-callback-value")
				}
				cancel() // Simulate a failed HTTP job progress persistence write.
			})
			out, err := service.DeleteAliases(ctx, ids)
			if len(out) != len(ids) || (err != nil) != panicCallback {
				t.Fatal("progress failure lost batch results")
			}
			if panicCallback && (Code(err) != CodeUpstreamError || strings.Contains(err.Error(), "fixture-sensitive") || len(err.Error()) > 100) {
				t.Fatal("callback panics were leaked or accumulated")
			}
			for i, outcome := range out {
				if outcome.AliasID != ids[i] || reports[ids[i]] != 1 {
					t.Fatalf("input %d was lost or reported more than once", i)
				}
				if i == 0 {
					if outcome.Err != nil || repo.hasAlias(ids[i]) {
						t.Error("first successful item was overwritten")
					}
				} else if !errors.Is(outcome.Err, context.Canceled) || !repo.hasAlias(ids[i]) {
					t.Fatalf("item %d started after progress failure", i)
				}
			}
			if client.deactivateCalls.Load() != 1 || client.deleteCalls.Load() != 1 {
				t.Error("progress failure did not stop new remote side effects")
			}
		})
	}
}
