package hmesync

import (
	"context"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

func TestCreateAutoAliasUsesSelectedForwardingTarget(t *testing.T) {
	for _, target := range []string{"primary@icloud.com", "forwarder@example.com", "another@icloud.com"} {
		for _, mode := range []string{"full reserve", "minimal reserve", "pending recovery"} {
			t.Run(target+"/"+mode, func(t *testing.T) {
				now := time.Now().UTC()
				repo := newFakeRepository(domain.Account{
					ID: 3, Email: "primary@icloud.com", Enabled: true,
					IMAPHost: "imap.example.net", IMAPUsername: "final-inbox@example.net",
				}, now)
				remote := apple.Alias{
					HME: "created@icloud.com", AnonymousID: "created-id", IsActive: true, ForwardToEmail: target,
				}
				reserved := mode == "pending recovery"
				if reserved {
					repo.pending = &domain.Alias{
						ID: 77, AccountID: 3, Address: remote.HME, LastSyncError: domain.AppleAliasConfirmationPending,
					}
				}
				lists := 0
				client := &fakeAppleClient{
					validate: func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil },
					list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
						lists++
						session.SessionToken = "latest-directory-session"
						// The local primary and final IMAP mailbox need not appear in Apple's candidates.
						directory := apple.ListResult{SelectedForwardTo: target, ForwardToEmails: []string{target}}
						if reserved {
							directory.Aliases = []apple.Alias{remote}
						}
						return directory, session, nil
					},
					create: func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
						reserved = true
						if mode == "minimal reserve" {
							return apple.Alias{HME: remote.HME}, session, nil
						}
						return remote, session, nil
					},
				}
				service := newTestService(t, repo, client, newFakeAcquiringLocker(), func() time.Time { return now })
				storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				created, err := service.CreateAutoAlias(ctx, 3)
				if err != nil || created.Address != remote.HME || !created.Enabled || repo.confirms.Load() != 1 {
					t.Fatalf("alias not confirmed: id=%d enabled=%v err=%v confirmations=%d", created.ID, created.Enabled, err, repo.confirms.Load())
				}
				wantReserves := int32(1)
				if mode == "pending recovery" {
					wantReserves = 0
					if created.ID != 77 {
						t.Fatalf("pending candidate was replaced: id=%d", created.ID)
					}
				}
				if client.createCalls.Load() != wantReserves || repo.creates.Load() != wantReserves ||
					client.updateCalls.Load() != 0 || lists != int(wantReserves)+1 {
					t.Fatalf("unexpected operations: reserves=%d writes=%d forwarding updates=%d lists=%d",
						client.createCalls.Load(), repo.creates.Load(), client.updateCalls.Load(), lists)
				}
				if repo.account.Email != "primary@icloud.com" || repo.account.IMAPUsername != "final-inbox@example.net" {
					t.Fatal("creation rewrote local mailbox configuration")
				}
				assertStoredAppleSessionToken(t, service, repo, 3, "latest-directory-session")
			})
		}
	}
}

func TestAutoAliasRecoveryDiscardsMissingCandidateWithThirdPartyForwarding(t *testing.T) {
	service, repo, client, _ := newRecoveryFixture(t, newFakeAcquiringLocker())
	client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
		return apple.ListResult{SelectedForwardTo: "forwarder@example.com"}, session, nil
	}
	_, err := service.CreateAutoAlias(context.Background(), 3)
	if Code(err) != CodeAliasCandidateDiscarded || repo.pending != nil || repo.discardCalls != 1 || client.createCalls.Load() != 0 {
		t.Fatalf("candidate recovery failed: err=%v discards=%d reserves=%d", err, repo.discardCalls, client.createCalls.Load())
	}
}

func TestDeleteAliasesUsesSelectedForwardingTarget(t *testing.T) {
	client := &fakeAppleClient{}
	service, repo, ids, directory := newAliasDeletionBatchFixture(t, 2, client, newFakeAcquiringLocker())
	directory.SelectedForwardTo = "forwarder@example.com"
	directory.ForwardToEmails = []string{directory.SelectedForwardTo}
	for i := range directory.Aliases {
		directory.Aliases[i].ForwardToEmail = directory.SelectedForwardTo
	}
	client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil }
	client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
		return directory, session, nil
	}
	client.deactivate = func(_ context.Context, session apple.Session, _ string) (apple.Session, error) { return session, nil }
	client.deleteRemote = func(_ context.Context, session apple.Session, _ string) (apple.Session, error) { return session, nil }
	outcomes, err := service.DeleteAliases(context.Background(), ids)
	if err != nil || len(outcomes) != len(ids) {
		t.Fatalf("batch failed: outcomes=%d err=%v", len(outcomes), err)
	}
	for _, outcome := range outcomes {
		if outcome.Err != nil || repo.hasAlias(outcome.AliasID) {
			t.Fatalf("forwarded alias deletion failed: id=%d err=%v", outcome.AliasID, outcome.Err)
		}
	}
	if client.deactivateCalls.Load() != 2 || client.deleteCalls.Load() != 2 || repo.aliasDeletes.Load() != 2 {
		t.Fatalf("unexpected deletion counts: deactivate=%d remote=%d local=%d",
			client.deactivateCalls.Load(), client.deleteCalls.Load(), repo.aliasDeletes.Load())
	}
}

func TestForwardingSettingsReadsThirdPartyTargetWithoutLocalMailboxCandidate(t *testing.T) {
	service, _, client := newForwardingTestService(t)
	client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
		return apple.ListResult{
			SelectedForwardTo: "forwarder@example.com", ForwardToEmails: []string{"forwarder@example.com"},
		}, session, nil
	}
	settings, err := service.GetForwardingSettings(context.Background(), 7)
	if err != nil || settings.SelectedForwardTo != "forwarder@example.com" || client.updateCalls.Load() != 0 {
		t.Fatalf("forwarding settings unavailable: err=%v writes=%d", err, client.updateCalls.Load())
	}
}
