package hmesync

import (
	"context"
	"errors"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

type reconciliationRepository struct {
	*fakeRepository
	reconciliations    int
	candidates         []domain.AliasImportCandidate
	directoryAddresses []string
}

func (r *reconciliationRepository) ReconcileAppleAliasesWithCredentials(_ context.Context, _ int64, candidates []domain.AliasImportCandidate, directoryAddresses []string) (domain.AliasImportResult, []domain.AliasImportCredential, error) {
	r.reconciliations++
	r.candidates = candidates
	r.directoryAddresses = directoryAddresses
	return domain.AliasImportResult{MissingCount: 17, RemovedCount: 17, InactiveUpdatedCount: 2, RestoredCount: 1}, nil, nil
}

func TestSyncReconcilesOnlyCompleteIdentityMatchedDirectory(t *testing.T) {
	for _, test := range []struct {
		name      string
		mutate    func(*apple.ListResult, *apple.Session)
		listError error
		wantError error
	}{
		{name: "owned complete directory"},
		{name: "third-party forwarding directory", mutate: func(list *apple.ListResult, _ *apple.Session) {
			list.SelectedForwardTo = "forwarder@example.com"
			list.ForwardToEmails = []string{list.SelectedForwardTo}
			list.Aliases[0].ForwardToEmail = list.SelectedForwardTo
		}},
		{name: "owned empty directory", mutate: func(list *apple.ListResult, _ *apple.Session) { list.Aliases = nil }},
		{name: "mixed forwarding preserves present local entries", mutate: func(list *apple.ListResult, _ *apple.Session) {
			list.Aliases = append(list.Aliases, apple.Alias{HME: "foreign@icloud.com", ForwardToEmail: "other@icloud.com", IsActive: true})
		}},
		{name: "directory read failed", listError: apple.ErrService, wantError: ErrUpstream},
		{name: "directory identity changed", wantError: ErrAccountMismatch, mutate: func(_ *apple.ListResult, session *apple.Session) { session.DSID = "other-dsid" }},
		{name: "directory identity absent", wantError: ErrSessionExpired, mutate: func(_ *apple.ListResult, session *apple.Session) { session.DSID = "" }},
		{name: "duplicate directory", wantError: ErrUpstream, mutate: func(list *apple.ListResult, _ *apple.Session) { list.Aliases = append(list.Aliases, list.Aliases[0]) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 9, 19, 1, 0, 0, 0, time.UTC)
			base := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com"}, now)
			repo := &reconciliationRepository{fakeRepository: base}
			client := &fakeAppleClient{
				validate: func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil },
				list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
					list := apple.ListResult{SelectedForwardTo: "primary@icloud.com", Aliases: []apple.Alias{{HME: "owned@icloud.com", ForwardToEmail: "primary@icloud.com", IsActive: true}}}
					if test.mutate != nil {
						test.mutate(&list, &session)
					}
					return list, session, test.listError
				},
			}
			service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
			storeSession(t, service, base, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})
			result, err := service.SyncAliases(context.Background(), 3)
			if !errors.Is(err, test.wantError) {
				t.Fatalf("error = %v, want %v", err, test.wantError)
			}
			if test.wantError != nil {
				if repo.reconciliations != 0 {
					t.Fatal("unsafe directory reached local reconciliation")
				}
				if test.wantError != nil && base.imports.Load() != 0 {
					t.Fatal("unverified directory imported aliases")
				}
				return
			}
			if repo.reconciliations != 1 || base.imports.Load() != 0 {
				t.Fatal("complete directory did not use reconciliation")
			}
			if test.name == "third-party forwarding directory" &&
				(len(repo.candidates) != 1 || repo.candidates[0].Address != "owned@icloud.com") {
				t.Fatalf("third-party forwarding alias was not imported: %+v", repo.candidates)
			}
			if result.Summary.MissingCount != 17 || result.Summary.RemovedCount != 17 || result.Summary.InactiveUpdatedCount != 2 || result.Summary.RestoredCount != 1 {
				t.Fatalf("summary lost reconciliation counts: %+v", result.Summary)
			}
			if test.name == "mixed forwarding preserves present local entries" {
				if len(repo.candidates) != 1 || len(repo.directoryAddresses) != 2 || repo.directoryAddresses[1] != "foreign@icloud.com" {
					t.Fatalf("unfiltered presence was lost: candidates=%+v addresses=%v", repo.candidates, repo.directoryAddresses)
				}
			}
		})
	}
}

func TestSyncReconciliationRejectsReplacedSessionBeforePublication(t *testing.T) {
	now := time.Date(2026, 9, 19, 1, 0, 0, 0, time.UTC)
	base := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com"}, now)
	repo := &reconciliationRepository{fakeRepository: base}
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil },
		list: func(ctx context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			if err := base.DeleteAppleWebSession(ctx, 3); err != nil {
				return apple.ListResult{}, session, err
			}
			return apple.ListResult{SelectedForwardTo: "primary@icloud.com"}, session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, base, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})
	_, err := service.SyncAliases(context.Background(), 3)
	if !errors.Is(err, ErrAccountChanged) || repo.reconciliations != 0 {
		t.Fatalf("replaced session result: err=%v reconciliations=%d", err, repo.reconciliations)
	}
}
