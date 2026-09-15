package hmesync

import (
	"context"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

// A reserve can succeed remotely before its address appears in Apple's list.
// The local confirmation marker must not make that stale row undeletable: an
// omitted directory entry is already an idempotent remote deletion result.
func TestDeletePendingAliasAllowsDirectoryAbsenceForSingleAndBatch(t *testing.T) {
	for _, mode := range []string{"single", "batch"} {
		t.Run(mode, func(t *testing.T) {
			service, repo, client := newPendingAliasDeleteFixture(t, false)

			if mode == "single" {
				if err := service.DeleteAlias(context.Background(), 41); err != nil {
					t.Fatalf("single pending alias deletion = %v", err)
				}
			} else {
				outcomes, err := service.DeleteAliases(context.Background(), []int64{41})
				if err != nil || len(outcomes) != 1 || outcomes[0].AliasID != 41 || outcomes[0].Err != nil {
					t.Fatalf("batch pending alias deletion: outcomes=%#v err=%v", outcomes, err)
				}
			}
			if repo.hasAlias(41) || repo.aliasDeletes.Load() != 1 {
				t.Fatalf("stale pending row was retained: exists=%v deletes=%d", repo.hasAlias(41), repo.aliasDeletes.Load())
			}
			if client.deactivateCalls.Load() != 0 || client.deleteCalls.Load() != 0 {
				t.Fatalf("directory absence triggered remote mutation: deactivate=%d delete=%d", client.deactivateCalls.Load(), client.deleteCalls.Load())
			}
		})
	}
}
func TestDeletePendingAliasContinuesRemoteDeletionWhenDirectoryContainsIt(t *testing.T) {
	for _, mode := range []string{"single", "batch"} {
		t.Run(mode, func(t *testing.T) {
			service, repo, client := newPendingAliasDeleteFixture(t, true)

			if mode == "single" {
				if err := service.DeleteAlias(context.Background(), 41); err != nil {
					t.Fatalf("single pending alias deletion = %v", err)
				}
			} else {
				outcomes, err := service.DeleteAliases(context.Background(), []int64{41})
				if err != nil || len(outcomes) != 1 || outcomes[0].AliasID != 41 || outcomes[0].Err != nil {
					t.Fatalf("batch pending alias deletion: outcomes=%#v err=%v", outcomes, err)
				}
			}
			if repo.hasAlias(41) || repo.aliasDeletes.Load() != 1 {
				t.Fatalf("pending row was retained after remote deletion: exists=%v deletes=%d", repo.hasAlias(41), repo.aliasDeletes.Load())
			}
			if client.deactivateCalls.Load() != 1 || client.deleteCalls.Load() != 1 {
				t.Fatalf("directory entry did not complete remote deletion: deactivate=%d delete=%d", client.deactivateCalls.Load(), client.deleteCalls.Load())
			}
		})
	}
}

func newPendingAliasDeleteFixture(t *testing.T, remotePresent bool) (*Service, *fakeRepository, *fakeAppleClient) {
	t.Helper()
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	repo.addAlias(domain.Alias{
		ID: 41, AccountID: 3, Address: "pending@icloud.com", Enabled: false,
		LastSyncError: "  " + domain.AppleAliasConfirmationPending + "  ",
	})
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			result := aliasDeletionDirectory()
			if remotePresent {
				result.Aliases = []apple.Alias{{
					AnonymousID: "pending-remote", HME: "pending@icloud.com",
					ForwardToEmail: "primary@icloud.com", IsActive: true,
				}}
			}
			return result, session, nil
		},
		deactivate: func(_ context.Context, session apple.Session, anonymousID string) (apple.Session, error) {
			if anonymousID != "pending-remote" {
				t.Fatalf("deactivate remote ID = %q", anonymousID)
			}
			return session, nil
		},
		deleteRemote: func(_ context.Context, session apple.Session, anonymousID string) (apple.Session, error) {
			if anonymousID != "pending-remote" {
				t.Fatalf("delete remote ID = %q", anonymousID)
			}
			return session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})
	return service, repo, client
}
