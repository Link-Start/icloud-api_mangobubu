package hmesync

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

func TestCreateAutoAliasCompleteReserveWaitsForDirectoryBeforeCredentialDelivery(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	db, err := store.Open(filepath.Join(t.TempDir(), "auto-authority.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cipher, err := secure.NewCipher([]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	db.ConfigureAliasCredentialFactory(func(id, version int64) (domain.AliasCredentialMaterial, error) {
		_, material, err := secure.NewAliasCredentialMaterial(cipher, id, version)
		return material, err
	})
	db.ConfigureAliasCredentialReuseFactory(func(id, version int64, ciphertext string) (domain.AliasCredentialMaterial, error) {
		apiKey, err := cipher.DecryptPendingAliasAPIKey(ciphertext)
		if err != nil {
			return domain.AliasCredentialMaterial{}, err
		}
		_, material, err := secure.NewAliasCredentialMaterialWithAPIKey(cipher, id, version, apiKey)
		return material, err
	})
	account, err := db.CreateAccount(ctx, domain.Account{
		Name: "Directory authority", Email: "primary@icloud.com", IMAPHost: "imap.mail.me.com",
		IMAPPort: 993, IMAPUsername: "primary@icloud.com", PasswordCiphertext: "fixture", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	remote := apple.Alias{
		HME: "reserved-candidate@icloud.com", IsActive: true, ForwardToEmail: account.Email,
		AnonymousID: "candidate-id", Label: autoCreateLabel,
	}
	visible := false
	lists := 0
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil },
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			lists++
			directory := apple.ListResult{SelectedForwardTo: account.Email}
			if visible {
				directory.Aliases = []apple.Alias{remote}
			}
			return directory, session, nil
		},
		create: func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
			return remote, session, nil
		},
	}
	service, err := New(db, cipher, client, newFakeAcquiringLocker(), WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	service.autoCreateConfirmationDelays = []time.Duration{0, 0}
	if _, err := service.saveSession(ctx, account.ID, apple.Session{
		AppleID: "owner@example.com", Region: apple.RegionGlobal, DSID: "trusted-dsid", ValidatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := service.CreateAutoAlias(ctx, account.ID)
	if !errors.Is(err, ErrAliasConfirmationPending) || result.ID != 0 || client.createCalls.Load() != 1 || lists != 4 {
		t.Fatalf("unlisted reserve result: id=%d err=%v reserves=%d lists=%d", result.ID, err, client.createCalls.Load(), lists)
	}
	pending, err := db.GetPendingAutoAliasConfirmation(ctx, account.ID)
	if err != nil || pending.Enabled || pending.Address != remote.HME {
		t.Fatalf("unlisted candidate was not retained disabled: err=%v enabled=%v", err, pending.Enabled)
	}
	if keys, err := db.ListPendingAliasAPIKeysByAccount(ctx, account.ID); err != nil || len(keys) != 0 {
		t.Fatalf("unconfirmed reserve released credentials: keys=%d err=%v", len(keys), err)
	}
	if count, err := db.CountEnabledAliasesByAccount(ctx, account.ID); err != nil || count != 0 {
		t.Fatalf("unconfirmed reserve became enabled: count=%d err=%v", count, err)
	}
	// Another schedule slot must reconcile the durable candidate and must not
	// reserve another address while Apple still omits the first one.
	if _, err := service.CreateAutoAlias(ctx, account.ID); !errors.Is(err, ErrAliasConfirmationPending) {
		t.Fatalf("unlisted candidate retry: %v", err)
	}
	visible = true
	confirmed, err := service.CreateAutoAlias(ctx, account.ID)
	if err != nil || confirmed.ID != pending.ID || !confirmed.Enabled || client.createCalls.Load() != 1 {
		t.Fatalf("later directory confirmation: id=%d enabled=%v err=%v reserves=%d", confirmed.ID, confirmed.Enabled, err, client.createCalls.Load())
	}
	keys, err := db.ListPendingAliasAPIKeysByAccount(ctx, account.ID)
	if err != nil || len(keys) != 1 || keys[0].ID != pending.ID || keys[0].APIKeyCiphertext != pending.APIKeyCiphertext {
		t.Fatalf("confirmation did not release only the original credential: keys=%d err=%v", len(keys), err)
	}
}

func TestCreateAutoAliasRequiresConsistentIdentityMatchedDirectoryForConfirmation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*apple.ListResult, *apple.Session)
		want   error
	}{
		{name: "duplicate address", want: ErrUpstream, mutate: func(list *apple.ListResult, _ *apple.Session) {
			list.Aliases = append(list.Aliases, list.Aliases[0])
		}},
		{name: "duplicate remote identity", want: ErrUpstream, mutate: func(list *apple.ListResult, _ *apple.Session) {
			other := list.Aliases[0]
			other.HME = "other@icloud.com"
			list.Aliases = append(list.Aliases, other)
		}},
		{name: "malformed other entry", want: ErrUpstream, mutate: func(list *apple.ListResult, _ *apple.Session) {
			list.Aliases = append(list.Aliases, apple.Alias{HME: "invalid"})
		}},
		{name: "wrong DSID", want: ErrAccountMismatch, mutate: func(_ *apple.ListResult, session *apple.Session) { session.DSID = "other-dsid" }},
		{name: "wrong Apple ID", want: ErrAccountMismatch, mutate: func(_ *apple.ListResult, session *apple.Session) { session.AppleID = "other@example.com" }},
		{name: "missing identity", want: ErrSessionExpired, mutate: func(_ *apple.ListResult, session *apple.Session) { *session = apple.Session{} }},
		{name: "inactive candidate", want: ErrAliasInactive, mutate: func(list *apple.ListResult, _ *apple.Session) { list.Aliases[0].IsActive = false }},
		{name: "wrong forwarding", want: ErrAccountMismatch, mutate: func(list *apple.ListResult, _ *apple.Session) { list.Aliases[0].ForwardToEmail = "other@icloud.com" }},
	} {
		for _, existing := range []bool{false, true} {
			stage := "new reserve"
			if existing {
				stage = "pending recovery"
			}
			t.Run(test.name+"/"+stage, func(t *testing.T) {
				now := time.Now().UTC()
				repo := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
				remote := apple.Alias{HME: "candidate@icloud.com", IsActive: true, ForwardToEmail: "primary@icloud.com", AnonymousID: "candidate-id"}
				if existing {
					repo.pending = &domain.Alias{ID: 77, AccountID: 3, Address: remote.HME, LastSyncError: domain.AppleAliasConfirmationPending}
				}
				lists := 0
				client := &fakeAppleClient{
					validate: func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil },
					list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
						lists++
						directory := apple.ListResult{SelectedForwardTo: "primary@icloud.com"}
						if existing || lists > 1 {
							directory.Aliases = []apple.Alias{remote}
							test.mutate(&directory, &session)
						}
						return directory, session, nil
					},
					create: func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
						return remote, session, nil
					},
				}
				service := newTestService(t, repo, client, newFakeAcquiringLocker(), func() time.Time { return now })
				service.autoCreateConfirmationDelays = nil
				storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})
				result, err := service.CreateAutoAlias(context.Background(), 3)
				if !errors.Is(err, test.want) || result.ID != 0 || repo.confirms.Load() != 0 || repo.pending == nil || repo.pending.Enabled {
					t.Fatalf("invalid directory published candidate: result id=%d err=%v confirmations=%d", result.ID, err, repo.confirms.Load())
				}
				wantReserves := int32(1)
				if existing {
					wantReserves = 0
				}
				if client.createCalls.Load() != wantReserves {
					t.Fatalf("reserve count=%d, want %d", client.createCalls.Load(), wantReserves)
				}
			})
		}
	}
}
