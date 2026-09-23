package hmesync

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

func newForwardingTestService(t *testing.T) (*Service, *fakeRepository, *fakeAppleClient) {
	t.Helper()
	now := time.Now().UTC()
	repo := newFakeRepository(domain.Account{ID: 7, Email: "primary@icloud.com"}, now)
	selected := "primary@icloud.com"
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil },
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			return apple.ListResult{
				SelectedForwardTo: selected,
				ForwardToEmails:   []string{"primary@icloud.com", "other@example.com"},
			}, session, nil
		},
		update: func(_ context.Context, session apple.Session, target string) (apple.Session, error) {
			selected = target
			session.SessionToken = "updated-token"
			return session, nil
		},
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 7, apple.Session{AppleID: "owner@example.com", Region: apple.RegionChina})
	return service, repo, client
}

func TestForwardingSettingsReadAndUpdate(t *testing.T) {
	service, repo, client := newForwardingTestService(t)
	ctx := context.Background()
	settings, err := service.GetForwardingSettings(ctx, 7)
	if err != nil || settings.SelectedForwardTo != "primary@icloud.com" ||
		!reflect.DeepEqual(settings.ForwardToEmails, []string{"primary@icloud.com", "other@example.com"}) {
		t.Fatalf("read settings = %#v, %v", settings, err)
	}
	if client.updateCalls.Load() != 0 {
		t.Fatal("reading forwarding settings changed Apple state")
	}

	settings, err = service.UpdateForwardingSettings(ctx, 7, " OTHER@EXAMPLE.COM ")
	if err != nil || settings.SelectedForwardTo != "other@example.com" || client.updateCalls.Load() != 1 {
		t.Fatalf("update settings = %#v, %v; writes=%d", settings, err, client.updateCalls.Load())
	}
	stored, err := service.decryptSession(repo.mustSession(t, 7))
	if err != nil || stored.SessionToken != "updated-token" {
		t.Fatalf("rotated session not preserved: %#v, %v", stored, err)
	}
	if _, err := service.UpdateForwardingSettings(ctx, 7, "other@example.com"); err != nil || client.updateCalls.Load() != 1 {
		t.Fatalf("unchanged setting wrote again: %v; writes=%d", err, client.updateCalls.Load())
	}
	if repo.imports.Load() != 0 || repo.creates.Load() != 0 || repo.aliasDeletes.Load() != 0 || repo.account.Email != "primary@icloud.com" {
		t.Fatal("forwarding changed local aliases or account configuration")
	}
}

func TestForwardingSettingsRejectsUnavailableTargets(t *testing.T) {
	for _, target := range []string{"", "not-an-email", "Someone <other@example.com>", "removed@example.com"} {
		t.Run(target, func(t *testing.T) {
			service, _, client := newForwardingTestService(t)
			_, err := service.UpdateForwardingSettings(context.Background(), 7, target)
			if !errors.Is(err, ErrForwardingTargetInvalid) || client.updateCalls.Load() != 0 {
				t.Fatalf("target %q: err=%v writes=%d", target, err, client.updateCalls.Load())
			}
		})
	}
}

func TestForwardingSettingsRequiresLiveMatchingIdentity(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Service, *fakeRepository, *fakeAppleClient)
		want   error
	}{
		{"missing session", func(_ *Service, repo *fakeRepository, _ *fakeAppleClient) { delete(repo.sessions, 7) }, ErrLoginRequired},
		{"custom mailbox", func(_ *Service, repo *fakeRepository, _ *fakeAppleClient) {
			repo.account.MailboxType = domain.MailboxTypeCustom
		}, ErrAccountChanged},
		{"expired session", func(_ *Service, _ *fakeRepository, client *fakeAppleClient) {
			client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) {
				return session, apple.ErrInvalidSession
			}
		}, ErrSessionExpired},
		{"different Apple identity", func(_ *Service, _ *fakeRepository, client *fakeAppleClient) {
			client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) {
				session.DSID = "different-dsid"
				return session, nil
			}
		}, ErrAccountMismatch},
		{"account changed during read", func(_ *Service, repo *fakeRepository, client *fakeAppleClient) {
			originalList := client.list
			client.list = func(ctx context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
				repo.account.Email = "changed@example.com"
				return originalList(ctx, session)
			}
		}, ErrAccountChanged},
		{"session replaced during read", func(service *Service, repo *fakeRepository, client *fakeAppleClient) {
			originalList := client.list
			client.list = func(ctx context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
				storeSession(t, service, repo, 7, apple.Session{AppleID: "new-owner@example.com", Region: apple.RegionChina})
				return originalList(ctx, session)
			}
		}, ErrAccountChanged},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, repo, client := newForwardingTestService(t)
			tt.change(service, repo, client)
			_, err := service.UpdateForwardingSettings(context.Background(), 7, "other@example.com")
			if !errors.Is(err, tt.want) || client.updateCalls.Load() != 0 {
				t.Fatalf("err=%v, want=%v; writes=%d", err, tt.want, client.updateCalls.Load())
			}
		})
	}
}

func TestForwardingSettingsPreservesSessionReplacedDuringExpiredRead(t *testing.T) {
	service, repo, client := newForwardingTestService(t)
	client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) {
		storeSession(t, service, repo, 7, apple.Session{AppleID: "replacement@example.com", Region: apple.RegionChina})
		return session, apple.ErrInvalidSession
	}
	_, err := service.GetForwardingSettings(context.Background(), 7)
	if !errors.Is(err, ErrSessionExpired) || repo.mustSession(t, 7).AppleID != "replacement@example.com" {
		t.Fatalf("expired read removed replacement session: %v", err)
	}
}

func TestForwardingSettingsDoesNotClaimUnconfirmedSuccessOrRetryMutation(t *testing.T) {
	for _, failure := range []string{"unconfirmed", "transport", "session expired", "confirmation read"} {
		t.Run(failure, func(t *testing.T) {
			service, repo, client := newForwardingTestService(t)
			want := ErrForwardingNotConfirmed
			client.update = func(_ context.Context, session apple.Session, _ string) (apple.Session, error) {
				session.SessionToken = "rotated-on-failure"
				switch failure {
				case "transport":
					want = ErrUpstream
					return session, errors.New("response lost")
				case "session expired":
					want = ErrSessionExpired
					return session, apple.ErrInvalidSession
				case "confirmation read":
					want = ErrUpstream
					client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
						return apple.ListResult{}, session, apple.ErrService
					}
				}
				return session, nil
			}
			_, err := service.UpdateForwardingSettings(context.Background(), 7, "other@example.com")
			if !errors.Is(err, want) || client.updateCalls.Load() != 1 {
				t.Fatalf("err=%v, want=%v; writes=%d", err, want, client.updateCalls.Load())
			}
			if failure == "session expired" {
				if repo.sessionCount() != 0 {
					t.Fatal("expired session was retained")
				}
			} else {
				stored, err := service.decryptSession(repo.mustSession(t, 7))
				if err != nil || stored.SessionToken != "rotated-on-failure" {
					t.Fatalf("failed operation lost session: %#v, %v", stored, err)
				}
			}
		})
	}
}
