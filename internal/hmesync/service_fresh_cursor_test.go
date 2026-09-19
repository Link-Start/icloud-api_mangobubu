package hmesync

import (
	"context"
	"errors"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

func TestCreateAutoAliasFreshConfirmationRouting(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name               string
		minimalResponse    bool
		crossRoundPending  bool
		legacyRepository   bool
		withoutAcquirer    bool
		preexistingAddress bool
		ambiguousReserve   bool
		wantFresh          bool
	}{
		{name: "complete successful reserve uses fresh confirmation", wantFresh: true},
		{name: "minimal successful reserve confirmed in same lock uses fresh confirmation", minimalResponse: true, wantFresh: true},
		{name: "pending candidate from earlier round uses traditional confirmation", minimalResponse: true, crossRoundPending: true},
		{name: "legacy repository retains traditional confirmation", legacyRepository: true},
		{name: "locker without account acquirer uses traditional confirmation", withoutAcquirer: true},
		{name: "reserve returns preexisting directory address uses traditional confirmation", preexistingAddress: true},
		{name: "ambiguous reserve reconciled successfully uses traditional confirmation", ambiguousReserve: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			now := time.Date(2026, 9, 11, 7, 0, 0, 0, time.UTC)
			accountVersion := now.Add(-time.Minute)
			base := newFakeRepository(domain.Account{
				ID: 3, Email: "primary@icloud.com", Enabled: true, UpdatedAt: accountVersion,
			}, now)
			acquirer := newFakeAcquiringLocker()
			var locker AccountLocker = acquirer
			if test.withoutAcquirer {
				locker = &fakeLocker{}
			}
			repo := &freshConfirmationRecordingRepository{fakeRepository: base}
			repo.onFresh = func(expected time.Time) {
				if len(acquirer.token) != 0 {
					t.Fatal("fresh confirmation ran without holding the account lock")
				}
				if !expected.Equal(accountVersion) {
					t.Fatalf("fresh confirmation version = %v, want pre-reserve version %v", expected, accountVersion)
				}
			}
			var persistence Repository = repo
			if test.legacyRepository {
				persistence = base
			}
			confirmedAlias := apple.Alias{
				HME: "new-fresh@icloud.com", IsActive: true, ForwardToEmail: "primary@icloud.com",
			}
			listCalls := 0
			client := &fakeAppleClient{
				validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
					return session, nil
				},
				list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
					listCalls++
					result := apple.ListResult{SelectedForwardTo: "primary@icloud.com"}
					visibleAt := 2
					if test.crossRoundPending {
						visibleAt = 3
					}
					if listCalls >= visibleAt {
						result.Aliases = []apple.Alias{confirmedAlias}
					} else if test.preexistingAddress {
						existing := confirmedAlias
						existing.HME = " NEW-FRESH@ICLOUD.COM "
						result.Aliases = []apple.Alias{existing}
					}
					return result, session, nil
				},
				create: func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
					if !test.withoutAcquirer && len(acquirer.token) != 0 {
						t.Fatal("reserve ran without holding the account lock")
					}
					created := confirmedAlias
					if test.minimalResponse {
						created.ForwardToEmail = ""
					}
					if test.ambiguousReserve {
						return created, session, errors.New("reserve response was lost after remote side effect")
					}
					return created, session, nil
				},
			}
			service := newTestService(t, persistence, client, locker, func() time.Time { return now })
			service.autoCreateConfirmationDelays = nil
			storeSession(t, service, base, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})
			if test.crossRoundPending {
				_, err := service.CreateAutoAlias(ctx, 3)
				if !errors.Is(err, ErrAliasConfirmationPending) {
					t.Fatalf("first round error = %v, want ErrAliasConfirmationPending", err)
				}
				if base.confirms.Load() != 0 || repo.freshCalls != 0 {
					t.Fatal("first round unexpectedly confirmed the invisible candidate")
				}
				if _, err := base.GetPendingAutoAliasConfirmation(ctx, 3); err != nil {
					t.Fatalf("first round lost durable pending candidate: %v", err)
				}
			}

			created, err := service.CreateAutoAlias(ctx, 3)
			if err != nil || !created.Enabled || created.Address != confirmedAlias.HME {
				t.Fatalf("create/confirm automatic alias = %#v, err=%v", created, err)
			}
			wantFreshCalls := 0
			if test.wantFresh {
				wantFreshCalls = 1
			}
			if repo.freshCalls != wantFreshCalls {
				t.Fatalf("fresh confirmation calls = %d, want %d", repo.freshCalls, wantFreshCalls)
			}
			if base.confirms.Load() != 1 || base.creates.Load() != 1 || client.createCalls.Load() != 1 {
				t.Fatalf("confirmation side effects: confirms=%d candidate writes=%d reserves=%d",
					base.confirms.Load(), base.creates.Load(), client.createCalls.Load())
			}
			wantListCalls := 2
			if test.crossRoundPending {
				wantListCalls = 3
			}
			if listCalls != wantListCalls {
				t.Fatalf("directory calls = %d, want %d", listCalls, wantListCalls)
			}
			if !test.withoutAcquirer && len(acquirer.token) != 1 {
				t.Fatal("completed automatic creation did not release the account lock")
			}
		})
	}
}

type freshConfirmationRecordingRepository struct {
	*fakeRepository
	freshCalls int
	onFresh    func(time.Time)
}

func (r *freshConfirmationRecordingRepository) ConfirmFreshAutoAlias(
	ctx context.Context,
	session domain.AppleWebSession,
	aliasID int64,
	expectedAccountVersion time.Time,
) (domain.Alias, domain.AppleWebSession, error) {
	r.freshCalls++
	if r.onFresh != nil {
		r.onFresh(expectedAccountVersion)
	}
	return r.fakeRepository.ConfirmPendingAutoAlias(ctx, session, aliasID)
}

var _ FreshAutoAliasConfirmationRepository = (*freshConfirmationRecordingRepository)(nil)
