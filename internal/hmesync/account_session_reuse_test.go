package hmesync

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"icloud-api/internal/apple"
)

type reusableAccountEmailClient struct {
	*fakeAccountEmailClient
	listErr, reuseErr     error
	listCalls, reuseCalls int
	reusedSession         apple.Session
}

func (f *reusableAccountEmailClient) ListAccountEmails(ctx context.Context, state apple.AccountWebSession) (apple.AccountEmailProfile, apple.AccountWebSession, error) {
	f.listCalls++
	if f.listErr != nil {
		return apple.AccountEmailProfile{}, state, f.listErr
	}
	return f.fakeAccountEmailClient.ListAccountEmails(ctx, state)
}

func (f *reusableAccountEmailClient) ResumeAccountSession(_ context.Context, session apple.Session) (apple.AccountEmailProfile, apple.AccountWebSession, error) {
	f.reuseCalls++
	f.reusedSession = session
	return f.profile, apple.AccountWebSession{AppleID: session.AppleID, ServiceKey: "reused-service", APIKey: "reused-api", SCNT: "reused-scnt"}, f.reuseErr
}

func TestAccountEmailProfileReusesCurrentICloudLoginBeforeRequestingAuthentication(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		missingPortal        bool
		listErr, reuseErr    error
		wantCode             string
		wantLists, wantReuse int
	}{
		{"first use", true, nil, nil, "", 0, 1},
		{"valid portal", false, nil, nil, "", 1, 0},
		{"expired portal", false, apple.ErrAccountWebAuth, nil, "", 1, 1},
		{"Apple requires login", true, nil, apple.ErrAccountWebAuth, CodeEmailAccountAuthRequired, 0, 1},
		{"reuse unavailable", true, nil, apple.ErrService, CodeUpstreamError, 0, 1},
		{"expired portal then network failure", false, apple.ErrAccountWebAuth, apple.ErrService, CodeUpstreamError, 1, 1},
		{"portal service failure", false, apple.ErrService, nil, CodeUpstreamError, 1, 0},
		{"identity mismatch", true, nil, apple.ErrAccountWebIdentity, CodeAccountMismatch, 0, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			service, repo, base := newAccountEmailService(t)
			original, err := service.decryptSession(repo.mustSession(t, 7))
			if err != nil {
				t.Fatal(err)
			}
			original.SCNT = "existing-icloud-scnt"
			original.SessionID = "existing-icloud-session"
			original.Cookies = []apple.PersistentCookie{{Name: "existing-login", Value: "trusted", Domain: "apple.com", Path: "/", Secure: true}}
			if tt.missingPortal {
				original.AccountWeb = nil
			}
			storeSession(t, service, repo, 7, original)
			client := &reusableAccountEmailClient{fakeAccountEmailClient: base, listErr: tt.listErr, reuseErr: tt.reuseErr}
			service.client = client
			_, err = service.GetAccountEmails(context.Background(), 7)
			if Code(err) != tt.wantCode || (err != nil && tt.wantCode == "") {
				t.Fatalf("err=%v, code=%q, want=%q", err, Code(err), tt.wantCode)
			}
			if client.listCalls != tt.wantLists || client.reuseCalls != tt.wantReuse {
				t.Fatalf("list=%d, reuse=%d", client.listCalls, client.reuseCalls)
			}
			if tt.wantReuse == 1 && !reflect.DeepEqual(client.reusedSession.Cookies, original.Cookies) {
				t.Fatal("reuse lost the current Apple login cookies")
			}
			stored, err := service.decryptSession(repo.mustSession(t, 7))
			if err != nil {
				t.Fatal(err)
			}
			if stored.SessionToken != original.SessionToken || stored.SCNT != original.SCNT || stored.SessionID != original.SessionID || !reflect.DeepEqual(stored.Cookies, original.Cookies) {
				t.Fatal("portal reuse overwrote the iCloud login")
			}
			if tt.wantCode == "" && tt.wantReuse == 1 && stored.AccountWeb.APIKey != "reused-api" {
				t.Fatal("reused portal session was not persisted")
			}
		})
	}
}

func TestAccountEmailAdditionWaitsForSuccessfulSessionReuse(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		service, repo, base := newAccountEmailService(t)
		original, _ := service.decryptSession(repo.mustSession(t, 7))
		original.AccountWeb = nil
		storeSession(t, service, repo, 7, original)
		client := &reusableAccountEmailClient{fakeAccountEmailClient: base}
		if rejected {
			client.reuseErr = apple.ErrAccountWebAuth
		}
		service.client = client
		result, err := service.BeginAccountEmail(context.Background(), 11, 7, "new@example.com")
		if client.reuseCalls != 1 {
			t.Fatal("addition did not attempt session reuse")
		}
		if rejected {
			if Code(err) != CodeEmailAccountAuthRequired || base.beginCalls != 0 {
				t.Fatalf("rejected auth reached email write: %v", err)
			}
		} else if err != nil || result.Status != "email_verification_required" || base.beginCalls != 1 {
			t.Fatalf("addition after reuse: %#v, %v", result, err)
		}
	}
}

func TestAccountEmailReuseHonorsMissingICloudLogin(t *testing.T) {
	service, repo, base := newAccountEmailService(t)
	delete(repo.sessions, 7)
	client := &reusableAccountEmailClient{fakeAccountEmailClient: base}
	service.client = client
	_, err := service.GetAccountEmails(context.Background(), 7)
	if !errors.Is(err, ErrLoginRequired) || client.reuseCalls != 0 {
		t.Fatalf("missing iCloud login: %v", err)
	}
}
