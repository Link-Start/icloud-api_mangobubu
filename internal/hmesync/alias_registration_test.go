package hmesync

import (
	"context"
	"errors"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

type registrationRepository struct {
	*fakeRepository
	create func(context.Context, domain.Alias) (domain.Alias, error)
}

func (r *registrationRepository) CreateAlias(ctx context.Context, alias domain.Alias) (domain.Alias, error) {
	r.creates.Add(1)
	return r.create(ctx, alias)
}

func TestRegisterExistingAliasVerifiesOnlyTargetBeforePublication(t *testing.T) {
	now := time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC)
	base := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com"}, now)
	locker := &fakeLocker{}
	groupID := int64(8)
	repo := &registrationRepository{fakeRepository: base}
	repo.create = func(_ context.Context, alias domain.Alias) (domain.Alias, error) {
		if locker.held.Load() == 0 {
			t.Fatal("registration published outside the account lock")
		}
		if alias.Address != "existing@icloud.com" || alias.AccountID != 3 || alias.Label != "manual note" ||
			alias.GroupID == nil || *alias.GroupID != groupID || !alias.Enabled {
			t.Fatalf("registration changed the requested alias: %#v", alias)
		}
		alias.ID = 12
		alias.CredentialVersion = 1
		return alias, nil
	}
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
			assertNetworkOutsideAccountLock(t, locker)
			return session, nil
		},
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			assertNetworkOutsideAccountLock(t, locker)
			session.SessionToken = "refreshed"
			return apple.ListResult{
				SelectedForwardTo: "forwarder@example.com",
				Aliases: []apple.Alias{
					{HME: "EXISTING@ICLOUD.COM", ForwardToEmail: "forwarder@example.com", IsActive: true},
					{HME: "unrelated@icloud.com", ForwardToEmail: "forwarder@example.com", IsActive: true},
				},
			}, session, nil
		},
	}
	service := newTestService(t, repo, client, locker, func() time.Time { return now })
	storeSession(t, service, base, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})
	created, err := service.RegisterExistingAlias(context.Background(), domain.Alias{
		AccountID: 3, Address: " Existing@iCloud.com ", Label: "manual note", GroupID: &groupID, Enabled: true,
	})
	if err != nil || created.ID != 12 || created.CredentialVersion != 1 {
		t.Fatalf("registration result = %#v, %v", created, err)
	}
	if base.creates.Load() != 1 || base.imports.Load() != 0 || client.createCalls.Load() != 0 {
		t.Fatal("single-address registration imported or reserved extra aliases")
	}
	assertStoredAppleSessionToken(t, service, base, 3, "refreshed")
}

func TestRegisterExistingAliasRejectsUnverifiedDirectoryWithoutPublication(t *testing.T) {
	for _, test := range []struct {
		name       string
		changeList func(*apple.ListResult)
		validate   func(*apple.Session) error
		list       func(*apple.Session) error
		noSession  bool
		noDSID     bool
		want       error
	}{
		{name: "missing session", noSession: true, want: ErrLoginRequired},
		{name: "session lacks DSID", noDSID: true, want: ErrSessionExpired},
		{name: "absent from Apple", changeList: func(list *apple.ListResult) { list.Aliases = nil }, want: ErrAliasNotFound},
		{name: "inactive at Apple", changeList: func(list *apple.ListResult) { list.Aliases[0].IsActive = false }, want: ErrAliasInactive},
		{name: "wrong forwarding", changeList: func(list *apple.ListResult) { list.Aliases[0].ForwardToEmail = "other@example.com" }, want: ErrAccountMismatch},
		{name: "missing forwarding", changeList: func(list *apple.ListResult) { list.Aliases[0].ForwardToEmail = "" }, want: ErrAccountMismatch},
		{name: "duplicate directory entry", changeList: func(list *apple.ListResult) { list.Aliases = append(list.Aliases, list.Aliases[0]) }, want: ErrUpstream},
		{name: "invalid directory entry", changeList: func(list *apple.ListResult) { list.Aliases = append(list.Aliases, apple.Alias{HME: "bad"}) }, want: ErrUpstream},
		{name: "expired Apple session", validate: func(*apple.Session) error { return apple.ErrInvalidSession }, want: ErrSessionExpired},
		{name: "changed validate DSID", validate: func(session *apple.Session) error { session.DSID = "other"; return nil }, want: ErrAccountMismatch},
		{name: "changed list DSID", list: func(session *apple.Session) error { session.DSID = "other"; return nil }, want: ErrAccountMismatch},
		{name: "missing list DSID", list: func(session *apple.Session) error { session.DSID = ""; return nil }, want: ErrSessionExpired},
		{name: "changed Apple ID", validate: func(session *apple.Session) error { session.AppleID = "other@example.com"; return nil }, want: ErrAccountMismatch},
		{name: "changed Apple region", list: func(session *apple.Session) error { session.Region = apple.RegionChina; return nil }, want: ErrAccountMismatch},
		{name: "directory request fails", list: func(*apple.Session) error { return errors.New("upstream unavailable") }, want: ErrUpstream},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC)
			base := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com"}, now)
			repo := &registrationRepository{fakeRepository: base, create: func(context.Context, domain.Alias) (domain.Alias, error) {
				t.Fatal("unverified address reached credential publication")
				return domain.Alias{}, nil
			}}
			list := apple.ListResult{SelectedForwardTo: "primary@icloud.com", Aliases: []apple.Alias{
				{HME: "existing@icloud.com", ForwardToEmail: "primary@icloud.com", IsActive: true},
			}}
			if test.changeList != nil {
				test.changeList(&list)
			}
			client := &fakeAppleClient{
				validate: func(_ context.Context, session apple.Session) (apple.Session, error) {
					var err error
					if test.validate != nil {
						err = test.validate(&session)
					}
					return session, err
				},
				list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
					var err error
					if test.list != nil {
						err = test.list(&session)
					}
					return list, session, err
				},
			}
			service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
			if !test.noSession {
				session := apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal}
				if test.noDSID {
					storeSessionExactly(t, service, base, 3, session)
				} else {
					storeSession(t, service, base, 3, session)
				}
			}
			upserts := base.upserts.Load()
			result, err := service.RegisterExistingAlias(context.Background(), domain.Alias{AccountID: 3, Address: "existing@icloud.com", Enabled: true})
			if !errors.Is(err, test.want) || result.ID != 0 {
				t.Fatalf("registration result = %#v, %v; want %v", result, err, test.want)
			}
			if base.creates.Load() != 0 || base.imports.Load() != 0 || base.upserts.Load() != upserts {
				t.Fatal("rejected registration wrote local state")
			}
		})
	}
}

func TestRegisterExistingAliasRechecksAccountAndSessionBeforePublication(t *testing.T) {
	for _, mutation := range []string{"email", "imap username", "mailbox type", "session removed", "session replaced", "session deauthenticated"} {
		t.Run(mutation, func(t *testing.T) {
			now := time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC)
			base := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com"}, now)
			repo := &registrationRepository{fakeRepository: base, create: func(context.Context, domain.Alias) (domain.Alias, error) {
				t.Fatal("changed account reached alias publication")
				return domain.Alias{}, nil
			}}
			client := &fakeAppleClient{
				validate: func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil },
				list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
					base.mu.Lock()
					defer base.mu.Unlock()
					switch mutation {
					case "email":
						base.account.Email = "changed@icloud.com"
					case "imap username":
						base.account.IMAPUsername = "changed@icloud.com"
					case "mailbox type":
						base.account.MailboxType = domain.MailboxTypeCustom
					case "session removed":
						delete(base.sessions, 3)
					case "session replaced":
						record := base.sessions[3]
						record.Ciphertext = "replaced"
						base.sessions[3] = record
					case "session deauthenticated":
						record := base.sessions[3]
						record.Authenticated = false
						base.sessions[3] = record
					}
					return apple.ListResult{SelectedForwardTo: "primary@icloud.com", Aliases: []apple.Alias{
						{HME: "existing@icloud.com", ForwardToEmail: "primary@icloud.com", IsActive: true},
					}}, session, nil
				},
			}
			service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
			storeSession(t, service, base, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})
			upserts := base.upserts.Load()
			_, err := service.RegisterExistingAlias(context.Background(), domain.Alias{AccountID: 3, Address: "existing@icloud.com", Enabled: true})
			if !errors.Is(err, ErrAccountChanged) || base.upserts.Load() != upserts || base.creates.Load() != 0 {
				t.Fatalf("changed account registration error = %v; creates = %d, upserts = %d", err, base.creates.Load(), base.upserts.Load()-upserts)
			}
		})
	}
}
