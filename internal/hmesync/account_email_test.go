package hmesync

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

type fakeAccountEmailClient struct {
	*fakeAppleClient
	profile                              apple.AccountEmailProfile
	beginCalls, verifyCalls, deleteCalls int
	needsAuthCode                        bool
	deleteErr                            error
}

func (f *fakeAccountEmailClient) SignInAccount(_ context.Context, state apple.AccountWebSession, password string) (apple.AccountWebSession, bool, error) {
	if password != "portal-password" {
		return state, false, apple.ErrAuthentication
	}
	state.ServiceKey = "portal-service"
	state.APIKey = "portal-api"
	return state, f.needsAuthCode, nil
}
func (f *fakeAccountEmailClient) VerifyAccountCode(_ context.Context, state apple.AccountWebSession, code string) (apple.AccountWebSession, error) {
	if code != "123456" {
		return state, apple.ErrTwoFactorCode
	}
	state.APIKey = "verified-portal-api"
	return state, nil
}
func (f *fakeAccountEmailClient) ListAccountEmails(_ context.Context, state apple.AccountWebSession) (apple.AccountEmailProfile, apple.AccountWebSession, error) {
	if state.ServiceKey == "" {
		return apple.AccountEmailProfile{}, state, apple.ErrAccountWebAuth
	}
	return f.profile, state, nil
}
func (f *fakeAccountEmailClient) BeginAccountEmail(_ context.Context, state apple.AccountWebSession, address string) (apple.EmailVerification, apple.AccountWebSession, error) {
	f.beginCalls++
	return apple.EmailVerification{ID: "upstream-private-verification-id", Address: address, Length: 6}, state, nil
}
func (f *fakeAccountEmailClient) VerifyAccountEmail(_ context.Context, state apple.AccountWebSession, verification apple.EmailVerification, code string) (apple.AccountWebSession, error) {
	f.verifyCalls++
	if code != "654321" {
		return state, apple.ErrAccountEmailCode
	}
	f.profile.Emails = append(f.profile.Emails, apple.AccountEmail{ID: 9, Address: verification.Address, Removable: true})
	state.SCNT = "after-email-verification"
	return state, nil
}
func (f *fakeAccountEmailClient) DeleteAccountEmail(_ context.Context, state apple.AccountWebSession, email apple.AccountEmail) (apple.AccountWebSession, error) {
	f.deleteCalls++
	state.SCNT = "after-email-deletion"
	if f.deleteErr != nil {
		return state, f.deleteErr
	}
	for i, item := range f.profile.Emails {
		if item.ID == email.ID {
			f.profile.Emails = append(f.profile.Emails[:i], f.profile.Emails[i+1:]...)
			break
		}
	}
	return state, nil
}

func newAccountEmailService(t *testing.T) (*Service, *fakeRepository, *fakeAccountEmailClient) {
	t.Helper()
	now := time.Now().UTC()
	repo := newFakeRepository(domain.Account{ID: 7, Email: "owner@example.com"}, now)
	client := &fakeAccountEmailClient{fakeAppleClient: &fakeAppleClient{}, profile: apple.AccountEmailProfile{AppleID: "owner@example.com", CanAdd: true,
		Emails: []apple.AccountEmail{{ID: 1, Address: "owner@example.com"}, {ID: 3, Address: "alternate@example.com", Removable: true}}}}
	client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
		list := apple.ListResult{SelectedForwardTo: "owner@example.com"}
		for _, email := range client.profile.Emails {
			list.ForwardToEmails = append(list.ForwardToEmails, email.Address)
		}
		session.SessionToken = "icloud-rotated"
		return list, session, nil
	}
	service := newTestService(t, repo, client, &fakeLocker{}, func() time.Time { return now })
	storeSession(t, service, repo, 7, apple.Session{AppleID: "owner@example.com", Region: apple.RegionChina, SessionToken: "icloud-original",
		AccountWeb: &apple.AccountWebSession{AppleID: "owner@example.com", ServiceKey: "portal-service", APIKey: "portal-api"}})
	return service, repo, client
}

func TestAccountEmailAdditionIsVerifiedOwnerBoundAndSingleUse(t *testing.T) {
	service, repo, client := newAccountEmailService(t)
	ctx := context.Background()
	started, err := service.BeginAccountEmail(ctx, 11, 7, " NEW@EXAMPLE.COM ")
	if err != nil || started.Status != "email_verification_required" || started.ChallengeID == "" || strings.Contains(started.ChallengeID, "upstream-private") || len(client.profile.Emails) != 2 {
		t.Fatalf("start=%#v err=%v", started, err)
	}
	if _, err := service.VerifyAccountEmail(ctx, 12, 7, started.ChallengeID, "654321"); Code(err) != CodeEmailFlowExpired || client.verifyCalls != 0 {
		t.Fatalf("other admin reached Apple: %v", err)
	}
	if _, err := service.VerifyAccountEmail(ctx, 11, 7, started.ChallengeID, "000000"); Code(err) != CodeEmailCodeInvalid {
		t.Fatalf("invalid code: %v", err)
	}
	result, err := service.VerifyAccountEmail(ctx, 11, 7, started.ChallengeID, "654321")
	if err != nil || result.Status != "complete" || result.Address != "new@example.com" || len(client.profile.Emails) != 3 {
		t.Fatalf("verification=%#v %v", result, err)
	}
	if _, err := service.VerifyAccountEmail(ctx, 11, 7, started.ChallengeID, "654321"); Code(err) != CodeEmailFlowExpired || client.verifyCalls != 2 {
		t.Fatalf("challenge replay: %v", err)
	}
	stored, err := service.decryptSession(repo.mustSession(t, 7))
	if err != nil || stored.SessionToken != "icloud-original" || stored.AccountWeb.SCNT != "after-email-verification" {
		t.Fatal("portal overwrote iCloud credentials or lost rotated state")
	}
	plaintext, _ := service.cipher.DecryptAppleSession(repo.mustSession(t, 7).Ciphertext)
	for _, secret := range []string{"654321", "upstream-private-verification-id", "portal-password"} {
		if strings.Contains(plaintext, secret) {
			t.Fatalf("persisted transient secret %q", secret)
		}
	}
	if repo.imports.Load() != 0 || repo.aliasDeletes.Load() != 0 {
		t.Fatal("email management changed local alias directory")
	}
}

func TestAccountEmailChallengesExpireAndLimitAttempts(t *testing.T) {
	for _, scenario := range []string{"expired", "five invalid codes", "session replaced", "wrong purpose"} {
		t.Run(scenario, func(t *testing.T) {
			service, repo, client := newAccountEmailService(t)
			started, err := service.BeginAccountEmail(context.Background(), 11, 7, "new@example.com")
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "expired":
				service.now = func() time.Time { return repo.now.Add(time.Hour) }
			case "five invalid codes":
				for i := 0; i < 5; i++ {
					if _, err := service.VerifyAccountEmail(context.Background(), 11, 7, started.ChallengeID, "000000"); Code(err) != CodeEmailCodeInvalid {
						t.Fatal(err)
					}
				}
			case "session replaced":
				repo.account.Email = "changed@example.com"
			case "wrong purpose":
				_, err = service.VerifyEmailAccountAuth(context.Background(), 11, 7, started.ChallengeID, "123456")
				if Code(err) != CodeEmailFlowExpired {
					t.Fatal(err)
				}
				return
			}
			previousCalls := client.verifyCalls
			_, err = service.VerifyAccountEmail(context.Background(), 11, 7, started.ChallengeID, "654321")
			if Code(err) != CodeEmailFlowExpired || client.verifyCalls != previousCalls {
				t.Fatalf("invalid challenge reached upstream: %v", err)
			}
		})
	}
}

func TestAccountEmailPortalLoginKeepsICloudSessionAndBindsTwoFactorChallenge(t *testing.T) {
	service, repo, client := newAccountEmailService(t)
	client.needsAuthCode = true
	started, err := service.StartEmailAccountAuth(context.Background(), 11, 7, "portal-password")
	if err != nil || started.Status != StatusVerificationRequired {
		t.Fatalf("login=%#v %v", started, err)
	}
	before, _ := service.decryptSession(repo.mustSession(t, 7))
	if before.AccountWeb.APIKey != "portal-api" {
		t.Fatal("unverified portal session published")
	}
	if _, err := service.VerifyEmailAccountAuth(context.Background(), 12, 7, started.ChallengeID, "123456"); Code(err) != CodeEmailFlowExpired {
		t.Fatal("challenge was not owner-bound")
	}
	verified, err := service.VerifyEmailAccountAuth(context.Background(), 11, 7, started.ChallengeID, "123456")
	if err != nil || verified.Status != StatusAuthenticated {
		t.Fatalf("verify=%#v %v", verified, err)
	}
	after, _ := service.decryptSession(repo.mustSession(t, 7))
	if after.AccountWeb.APIKey != "verified-portal-api" || after.SessionToken != before.SessionToken || after.DSID != before.DSID {
		t.Fatal("portal and iCloud sessions were mixed")
	}
}

func TestAccountEmailDeletionChecksFreshChoicesAndProtectsPrimary(t *testing.T) {
	for _, tt := range []struct {
		name, target, want string
		change             func(*fakeAccountEmailClient)
	}{
		{"only one", "owner@example.com", CodeEmailLastAddress, func(c *fakeAccountEmailClient) { c.profile.Emails = c.profile.Emails[:1] }},
		{"primary", "owner@example.com", CodeEmailNotRemovable, func(*fakeAccountEmailClient) {}},
		{"unavailable", "unknown@example.com", CodeEmailNotRemovable, func(*fakeAccountEmailClient) {}},
		{"stale single choice", "alternate@example.com", CodeEmailLastAddress, func(c *fakeAccountEmailClient) {
			c.list = func(_ context.Context, s apple.Session) (apple.ListResult, apple.Session, error) {
				return apple.ListResult{ForwardToEmails: []string{"alternate@example.com"}}, s, nil
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			service, _, client := newAccountEmailService(t)
			tt.change(client)
			err := service.DeleteAccountEmail(context.Background(), 7, tt.target)
			if Code(err) != tt.want || client.deleteCalls != 0 {
				t.Fatalf("err=%v code=%q calls=%d", err, Code(err), client.deleteCalls)
			}
		})
	}
}

func TestAccountEmailConcurrentDeletesCannotRemoveLastAddress(t *testing.T) {
	service, repo, client := newAccountEmailService(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- service.DeleteAccountEmail(context.Background(), 7, "alternate@example.com")
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if Code(err) != CodeEmailLastAddress {
			t.Fatal(err)
		}
	}
	if successes != 1 || client.deleteCalls != 1 || len(client.profile.Emails) != 1 {
		t.Fatalf("success=%d calls=%d emails=%d", successes, client.deleteCalls, len(client.profile.Emails))
	}
	stored, _ := service.decryptSession(repo.mustSession(t, 7))
	if stored.SessionToken != "icloud-rotated" || stored.AccountWeb.SCNT != "after-email-deletion" {
		t.Fatal("session rotations not saved")
	}
}

func TestAccountEmailDeletionFailurePreservesRotatedSessionWithoutRetry(t *testing.T) {
	service, repo, client := newAccountEmailService(t)
	client.deleteErr = errors.New("lost response")
	err := service.DeleteAccountEmail(context.Background(), 7, "alternate@example.com")
	if !errors.Is(err, ErrUpstream) || client.deleteCalls != 1 {
		t.Fatalf("err=%v calls=%d", err, client.deleteCalls)
	}
	stored, _ := service.decryptSession(repo.mustSession(t, 7))
	if stored.AccountWeb.SCNT != "after-email-deletion" {
		t.Fatal("rotated state discarded after uncertain deletion")
	}
}
