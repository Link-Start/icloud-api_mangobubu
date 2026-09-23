package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"icloud-api/internal/apple"
	"icloud-api/internal/hmesync"
)

type fakeAdminAccountEmailService struct {
	fakeHMESyncService
	calls              int
	ownerID, accountID int64
	input              string
}

func (f *fakeAdminAccountEmailService) GetAccountEmails(_ context.Context, id int64) (apple.AccountEmailProfile, error) {
	f.calls++
	f.accountID = id
	return apple.AccountEmailProfile{AppleID: "owner@example.com", CanAdd: true, Emails: []apple.AccountEmail{{ID: 1, Address: "owner@example.com"}, {ID: 3, Address: "extra@example.com", Removable: true}}}, nil
}
func (f *fakeAdminAccountEmailService) StartEmailAccountAuth(_ context.Context, owner, id int64, password string) (hmesync.EmailActionResult, error) {
	f.calls++
	f.ownerID, f.accountID, f.input = owner, id, password
	return hmesync.EmailActionResult{Status: hmesync.StatusVerificationRequired, ChallengeID: "auth-challenge"}, nil
}
func (f *fakeAdminAccountEmailService) VerifyEmailAccountAuth(_ context.Context, owner, id int64, challenge, code string) (hmesync.EmailActionResult, error) {
	f.calls++
	f.ownerID, f.accountID, f.input = owner, id, challenge+":"+code
	return hmesync.EmailActionResult{Status: hmesync.StatusAuthenticated}, nil
}
func (f *fakeAdminAccountEmailService) BeginAccountEmail(_ context.Context, owner, id int64, address string) (hmesync.EmailActionResult, error) {
	f.calls++
	f.ownerID, f.accountID, f.input = owner, id, address
	return hmesync.EmailActionResult{Status: "email_verification_required", ChallengeID: "email-challenge", Address: address}, nil
}
func (f *fakeAdminAccountEmailService) VerifyAccountEmail(_ context.Context, owner, id int64, challenge, code string) (hmesync.EmailActionResult, error) {
	f.calls++
	f.ownerID, f.accountID, f.input = owner, id, challenge+":"+code
	return hmesync.EmailActionResult{Status: "complete", Address: "extra@example.com"}, nil
}
func (f *fakeAdminAccountEmailService) DeleteAccountEmail(_ context.Context, id int64, address string) error {
	f.calls++
	f.accountID, f.input = id, address
	return nil
}

func TestAdminAPIAccountEmailOperationsRequireSessionCSRFAndScopedInputs(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, admin := env.createSession(t, "account-email-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "owner@example.com")
	service := &fakeAdminAccountEmailService{}
	env.server.SetHMESyncService(service)
	base := fmt.Sprintf("/admin/api/v1/accounts/%d/forwarding/", account.ID)
	for _, tt := range []struct {
		method, path, body, wantInput string
		status                        int
	}{
		{http.MethodGet, "emails", "", "", 200},
		{http.MethodPost, "email-auth", `{"password":"private-password"}`, "private-password", 202},
		{http.MethodPost, "email-auth/verify", `{"challenge_id":"auth-challenge","code":"123456"}`, "auth-challenge:123456", 200},
		{http.MethodPost, "emails", `{"address":"extra@example.com"}`, "extra@example.com", 202},
		{http.MethodPost, "emails/verify", `{"challenge_id":"email-challenge","code":"654321"}`, "email-challenge:654321", 200},
		{http.MethodDelete, "emails", `{"address":"extra@example.com"}`, "extra@example.com", 200},
	} {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			before := service.calls
			unauth := env.request(t, tt.method, base+tt.path, []byte(tt.body), "application/json", nil, csrf)
			if unauth.Code != http.StatusUnauthorized || service.calls != before {
				t.Fatal("unauthenticated request reached service")
			}
			if tt.method != http.MethodGet {
				noCSRF := env.request(t, tt.method, base+tt.path, []byte(tt.body), "application/json", []*http.Cookie{cookie}, "")
				if noCSRF.Code != http.StatusForbidden || service.calls != before {
					t.Fatal("request without CSRF reached service")
				}
			}
			response := env.request(t, tt.method, base+tt.path, []byte(tt.body), "application/json", []*http.Cookie{cookie}, csrf)
			if response.Code != tt.status || service.accountID != account.ID || service.calls != before+1 {
				t.Fatalf("response=%d %s", response.Code, response.Body.String())
			}
			if tt.method == http.MethodPost && service.ownerID != admin.ID {
				t.Fatal("challenge owner not bound to signed-in administrator")
			}
			if tt.wantInput != "" && service.input != tt.wantInput {
				t.Fatalf("service input=%q", service.input)
			}
			if strings.Contains(response.Body.String(), "private-password") || strings.Contains(response.Body.String(), "654321") {
				t.Fatal("response leaked transient credentials")
			}
		})
	}
	for _, tt := range []struct{ path, method, body string }{
		{"email-auth", http.MethodPost, `{"password":""}`},
		{"email-auth/verify", http.MethodPost, `{"challenge_id":"id","code":"123"}`},
		{"emails", http.MethodPost, `{"address":"bad-address"}`},
		{"emails", http.MethodDelete, `{"address":"extra@example.com","id":1}`},
		{"emails", http.MethodDelete, `{"address":["extra@example.com","owner@example.com"]}`},
	} {
		before := service.calls
		response := env.request(t, tt.method, base+tt.path, []byte(tt.body), "application/json", []*http.Cookie{cookie}, csrf)
		if response.Code != http.StatusBadRequest || service.calls != before {
			t.Fatalf("invalid request: %d %s", response.Code, response.Body.String())
		}
	}
}
