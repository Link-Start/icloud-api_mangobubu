package hmesync

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

func newAliasDeletionBatchFixture(t *testing.T, count int, client *fakeAppleClient, locker AccountLocker) (*Service, *fakeRepository, []int64, apple.ListResult) {
	t.Helper()
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", MailboxType: domain.MailboxTypeICloud, Enabled: true}, now)
	directory := aliasDeletionDirectory()
	ids := make([]int64, count)
	for i := range ids {
		id := int64(41 + i)
		ids[i] = id
		address := fmt.Sprintf("alias%d@icloud.com", id)
		repo.addAlias(domain.Alias{ID: id, AccountID: 3, Address: address, Enabled: true})
		directory.Aliases = append(directory.Aliases, apple.Alias{
			AnonymousID: fmt.Sprintf("remote-%d", id), HME: address,
			ForwardToEmail: "primary@icloud.com", IsActive: true,
		})
	}
	service := newTestService(t, repo, client, locker, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{
		AppleID: "owner@example.com", Region: apple.RegionGlobal, SessionToken: "token-0",
	})
	return service, repo, ids, directory
}

func TestDeleteAliasesReusesValidationDirectoryAndRollingSession(t *testing.T) {
	for _, count := range []int{1, 5, 1000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			client := &fakeAppleClient{}
			locker := newFakeAcquiringLocker()
			service, repo, ids, directory := newAliasDeletionBatchFixture(t, count, client, locker)
			calls, validates, lists, localDeletes, reports := 0, 0, 0, 0, 0
			advance := func(ctx context.Context, session apple.Session) apple.Session {
				t.Helper()
				if ctx.Err() != nil || len(locker.token) != 0 {
					t.Error("request escaped the account lock or used a cancelled context")
				}
				if session.SessionToken != fmt.Sprintf("token-%d", calls) {
					t.Errorf("request %d did not receive the latest session", calls+1)
				}
				if calls > 0 && (len(session.Cookies) != 1 || session.Cookies[0].Value != session.SessionToken) {
					t.Error("rotated cookies were lost")
				}
				assertStoredAppleSessionToken(t, service, repo, 3, session.SessionToken)
				calls++
				session.SessionToken = fmt.Sprintf("token-%d", calls)
				session.Cookies = []apple.PersistentCookie{{Name: "test-cookie", Value: session.SessionToken}}
				return session
			}
			client.validate = func(ctx context.Context, session apple.Session) (apple.Session, error) {
				validates++
				return advance(ctx, session), nil
			}
			client.list = func(ctx context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
				lists++
				return directory, advance(ctx, session), nil
			}
			client.deactivate = func(ctx context.Context, session apple.Session, id string) (apple.Session, error) {
				if id != fmt.Sprintf("remote-%d", ids[localDeletes]) || reports != localDeletes {
					t.Error("same-account items or progress reports were reordered")
				}
				return advance(ctx, session), nil
			}
			client.deleteRemote = func(ctx context.Context, session apple.Session, id string) (apple.Session, error) {
				if id != fmt.Sprintf("remote-%d", ids[localDeletes]) || calls != 3+2*localDeletes {
					t.Error("permanent deletion did not follow deactivation")
				}
				return advance(ctx, session), nil
			}
			repo.deleteAliasFn = func(ctx context.Context, id int64) error {
				if ctx.Err() != nil || id != ids[localDeletes] || calls != 4+2*localDeletes || len(locker.token) != 0 {
					t.Error("local deletion ran before confirmed remote deletion or outside account lock")
				}
				localDeletes++
				return nil
			}
			ctx := WithAliasDeletionProgress(context.Background(), func(outcome AliasDeletionOutcome) {
				if reports >= count || outcome.AliasID != ids[reports] || outcome.Err != nil || repo.hasAlias(outcome.AliasID) || localDeletes != reports+1 {
					t.Error("progress was not emitted immediately after durable local completion")
				}
				reports++
			})
			outcomes, err := service.DeleteAliases(ctx, ids)
			if err != nil || len(outcomes) != count || reports != count || localDeletes != count {
				t.Fatalf("batch completion: err=%v items=%d reports=%d local=%d", err, len(outcomes), reports, localDeletes)
			}
			if calls != 2*count+2 || validates != 1 || lists != 1 || client.deactivateCalls.Load() != int32(count) || client.deleteCalls.Load() != int32(count) {
				t.Fatalf("network count: requests=%d want=%d validate=%d list=%d", calls, 2*count+2, validates, lists)
			}
			assertStoredAppleSessionToken(t, service, repo, 3, fmt.Sprintf("token-%d", calls))
		})
	}
}

func TestDeleteAliasesFreshRechecksCachedAbsence(t *testing.T) {
	for _, scenario := range []string{"absent", "appeared", "foreign", "read failed"} {
		t.Run(scenario, func(t *testing.T) {
			client := &fakeAppleClient{}
			service, repo, ids, full := newAliasDeletionBatchFixture(t, 2, client, &fakeLocker{})
			lists := 0
			client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil }
			client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
				lists++
				result := aliasDeletionDirectory()
				if lists == 1 {
					result.Aliases = full.Aliases[:1]
				} else {
					if session.SessionToken != "deleted-first" {
						t.Error("fresh recheck did not use the preceding deletion's session")
					}
					session.SessionToken = "fresh-recheck"
					switch scenario {
					case "appeared", "foreign":
						remote := full.Aliases[1]
						if scenario == "foreign" {
							remote.ForwardToEmail = "other@example.com"
						}
						result.Aliases = []apple.Alias{remote}
					case "read failed":
						return result, session, ambiguousAliasMutationError("list")
					}
				}
				return result, session, nil
			}
			client.deactivate = func(_ context.Context, session apple.Session, id string) (apple.Session, error) {
				if id == "remote-42" && (scenario != "appeared" || lists != 2 || session.SessionToken != "fresh-recheck") {
					t.Error("cached absence reached a mutation before fresh ownership check")
				}
				return session, nil
			}
			client.deleteRemote = func(_ context.Context, session apple.Session, id string) (apple.Session, error) {
				if id == "remote-41" {
					session.SessionToken = "deleted-first"
				}
				return session, nil
			}
			repo.deleteAliasFn = func(_ context.Context, id int64) error {
				if id == ids[1] && lists != 2 {
					t.Error("cached absence was used as local deletion evidence")
				}
				return nil
			}
			outcomes, err := service.DeleteAliases(context.Background(), ids)
			if err != nil || len(outcomes) != 2 || outcomes[0].Err != nil || lists != 2 {
				t.Fatalf("fresh recheck: err=%v lists=%d items=%d", err, lists, len(outcomes))
			}
			wantLocal := scenario == "foreign" || scenario == "read failed"
			if repo.hasAlias(ids[1]) != wantLocal || (outcomes[1].Err != nil) != wantLocal {
				t.Error("recheck failure/foreign alias did not preserve local record")
			}
			if scenario == "foreign" && !errors.Is(outcomes[1].Err, ErrAccountMismatch) {
				t.Error("foreign alias was classified as absent")
			}
			wantMutations := int32(1)
			if scenario == "appeared" {
				wantMutations = 2
			}
			if client.deactivateCalls.Load() != wantMutations || client.deleteCalls.Load() != wantMutations {
				t.Error("fresh recheck did not preserve Apple-first deletion")
			}
			assertStoredAppleSessionToken(t, service, repo, 3, "fresh-recheck")
		})
	}
}

func TestDeleteAliasesAmbiguityRefreshesEntireDirectoryAndNeverRollsBackSession(t *testing.T) {
	for _, operation := range []string{"deactivate", "delete"} {
		for _, recovery := range []string{"absent", "present", "read failed", "foreign next"} {
			t.Run(operation+"/"+recovery, func(t *testing.T) {
				client := &fakeAppleClient{}
				service, repo, ids, full := newAliasDeletionBatchFixture(t, 2, client, &fakeLocker{})
				lists, mutations, expected := 0, 0, "token-0"
				rotate := func(session apple.Session, token string) apple.Session {
					if session.SessionToken != expected {
						t.Errorf("session rolled back before %s", token)
					}
					session.SessionToken, expected = token, token
					return session
				}
				client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) {
					return rotate(session, "validated"), nil
				}
				client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
					lists++
					session = rotate(session, fmt.Sprintf("list-%d", lists))
					if lists == 1 {
						return full, session, nil
					}
					if lists == 2 && recovery == "read failed" {
						return apple.ListResult{}, session, ambiguousAliasMutationError("list")
					}
					result := aliasDeletionDirectory()
					if recovery == "present" {
						first := full.Aliases[0]
						first.IsActive = operation == "deactivate"
						result.Aliases = append(result.Aliases, first)
					}
					second := full.Aliases[1]
					second.IsActive = false
					second.AnonymousID = "refreshed-second"
					if recovery == "foreign next" {
						second.ForwardToEmail = "other@example.com"
					}
					result.Aliases = append(result.Aliases, second)
					return result, session, nil
				}
				client.deactivate = func(_ context.Context, session apple.Session, id string) (apple.Session, error) {
					if id != "remote-41" {
						t.Error("stale directory caused re-deactivation of the second alias")
					}
					mutations++
					session = rotate(session, "deactivated")
					if operation == "deactivate" {
						return session, ambiguousAliasMutationError("deactivate")
					}
					return session, nil
				}
				client.deleteRemote = func(_ context.Context, session apple.Session, id string) (apple.Session, error) {
					mutations++
					session = rotate(session, "deleted-"+id)
					if id == "remote-41" && operation == "delete" {
						return session, ambiguousAliasMutationError("delete")
					}
					if id != "refreshed-second" || lists < 2 || recovery == "foreign next" {
						t.Error("permanent deletion used a stale remote ID or ownership")
					}
					return session, nil
				}
				outcomes, err := service.DeleteAliases(context.Background(), ids)
				if err != nil || len(outcomes) != 2 {
					t.Fatalf("batch result err=%v items=%d", err, len(outcomes))
				}
				firstFailed := recovery == "present" || recovery == "read failed"
				if repo.hasAlias(ids[0]) != firstFailed || (outcomes[0].Err != nil) != firstFailed {
					t.Error("ambiguous first result was not authoritatively reconciled")
				}
				secondFailed := recovery == "foreign next"
				if repo.hasAlias(ids[1]) != secondFailed || (outcomes[1].Err != nil) != secondFailed {
					t.Error("second item used stale ownership or failed to recover")
				}
				wantLists := 2
				if recovery == "read failed" {
					wantLists++
				}
				wantMutations := 2
				if operation == "delete" {
					wantMutations++
				}
				if recovery == "foreign next" {
					wantMutations--
				}
				if lists != wantLists || mutations != wantMutations {
					t.Errorf("calls: lists=%d want=%d mutations=%d want=%d", lists, wantLists, mutations, wantMutations)
				}
				assertStoredAppleSessionToken(t, service, repo, 3, expected)
			})
		}
	}
}
