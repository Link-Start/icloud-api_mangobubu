package apple

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const accountEmailProfileFixture = `{"apiKey":"portal-api-key","appleID":{"name":"owner@example.com"},"primaryEmailAddress":{"id":1,"address":"owner@example.com","vetted":true},"alternateEmailAddresses":[{"id":3,"address":"alternate@example.com","vetted":true}],"shouldAllowAddAlternateEmail":true}`

func TestAccountPortalAuthenticationUsesItsOwnBootstrapAndNoICloudExchange(t *testing.T) {
	for _, completeStatus := range []int{http.StatusOK, http.StatusConflict} {
		t.Run(http.StatusText(completeStatus), func(t *testing.T) {
			fixture := newAuthFixture(t, completeStatus)
			fixture.verifyStatus = http.StatusNoContent
			var profileReads, tokenReads int
			client, err := NewClient(Config{Random: deterministicRandom(), Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch request.URL.Path {
				case "/":
					if request.URL.Host != "account.apple.com" {
						t.Fatal("incorrect portal origin")
					}
					return testResponse(request, 200, "<html></html>", nil), nil
				case "/bootstrap/portal":
					return testResponse(request, 200, `{"serviceKey":"portal-service-key"}`, nil), nil
				case "/appleauth/auth/authorize/signin":
					if request.URL.Query().Get("client_id") != "portal-service-key" || request.URL.Query().Get("redirect_uri") != accountPortalOrigin {
						t.Fatal("portal reused the iCloud authorization context")
					}
					headers := http.Header{"Scnt": {"scnt-one"}, "X-Apple-Id-Session-Id": {"session-one"}, "X-Apple-Auth-Attributes": {"attributes-one"}}
					headers.Add("Set-Cookie", "idms=portal; Domain=.apple.com; Path=/; Secure; HttpOnly")
					return testResponse(request, 200, "", headers), nil
				case "/account/manage/gs/ws/token":
					tokenReads++
					return testResponse(request, 200, `{"token":"unused-service-token"}`, http.Header{"Scnt": {"portal-scnt"}}), nil
				case "/account/manage":
					profileReads++
					if request.Header.Get("scnt") != "portal-scnt" || !strings.Contains(request.Header.Get("Cookie"), "idms=portal") {
						t.Fatal("portal lost session headers/cookies")
					}
					return testResponse(request, 200, accountEmailProfileFixture, nil), nil
				default:
					if request.Header.Get("X-Apple-Widget-Key") != "portal-service-key" || request.Header.Get("X-Apple-OAuth-Redirect-URI") != accountPortalOrigin {
						t.Fatal("wrong portal authentication headers")
					}
					return fixture.RoundTrip(request)
				}
			})})
			if err != nil {
				t.Fatal(err)
			}
			state, needsCode, err := client.SignInAccount(context.Background(), AccountWebSession{AppleID: fixture.appleID}, fixture.password)
			if err != nil || needsCode != (completeStatus == http.StatusConflict) {
				t.Fatalf("sign-in: %v, needsCode=%v", err, needsCode)
			}
			if needsCode {
				state, err = client.VerifyAccountCode(context.Background(), state, "123456")
				if err != nil {
					t.Fatal(err)
				}
			}
			if state.APIKey != "portal-api-key" || state.ServiceKey != "portal-service-key" || tokenReads != 1 || profileReads != 1 || fixture.trustRequests != 0 || fixture.accountLoginRequests != 0 {
				t.Fatalf("unexpected portal authentication state: token reads=%d profile reads=%d", tokenReads, profileReads)
			}
			encoded, _ := json.Marshal(state)
			if strings.Contains(string(encoded), fixture.password) || strings.Contains(string(encoded), "123456") {
				t.Fatal("transient credential persisted")
			}
		})
	}
}

func TestAccountEmailRequestsFollowCapturedProtocolAndRotateSession(t *testing.T) {
	var calls []string
	client, err := NewClient(Config{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls = append(calls, request.Method+" "+request.URL.Path)
		if request.URL.Host != "appleid.apple.com" || request.Header.Get("Origin") != accountPortalOrigin ||
			request.Header.Get("X-Apple-Api-Key") != "portal-api-key" || request.Header.Get("X-Apple-I-Request-Context") != "ca" {
			t.Fatal("incorrect email management headers")
		}
		if len(calls) > 1 && request.Header.Get("scnt") != "rotated" {
			t.Fatal("SCNT rotation lost")
		}
		headers := http.Header{"Scnt": {"rotated"}, "Set-Cookie": {"account-email=rotated; Path=/; Secure; HttpOnly"}}
		var payload map[string]any
		if request.Body != nil {
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
		}
		switch request.Method {
		case http.MethodPost:
			if !reflect.DeepEqual(payload, map[string]any{"address": "new@example.com"}) {
				t.Fatalf("add payload: %#v", payload)
			}
			return testResponse(request, 201, `{"verificationId":"email-challenge","address":"new@example.com","length":6,"canGenerateNew":false,"newICloudEmail":false}`, headers), nil
		case http.MethodPut:
			if !reflect.DeepEqual(payload, map[string]any{"address": "new@example.com", "verificationInfo": map[string]any{"id": "email-challenge", "answer": "654321"}}) {
				t.Fatalf("verify payload: %#v", payload)
			}
			return testResponse(request, 200, `{"id":3,"address":"new@example.com","type":"profile","vetted":true}`, headers), nil
		case http.MethodDelete:
			if request.Body != nil {
				t.Fatal("DELETE must have no request body")
			}
			return testResponse(request, 200, `{"primaryEmailAddress":{"id":1,"address":"owner@example.com"},"alternateEmailAddresses":[],"emailAddressRemoved":{"id":3,"address":"new@example.com"}}`, headers), nil
		}
		return nil, errors.New("unexpected request")
	})})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	state := AccountWebSession{AppleID: "owner@example.com", ServiceKey: "portal-service", APIKey: "portal-api-key"}
	verification, state, err := client.BeginAccountEmail(ctx, state, " NEW@EXAMPLE.COM ")
	if err != nil {
		t.Fatal(err)
	}
	state, err = client.VerifyAccountEmail(ctx, state, verification, "654321")
	if err != nil {
		t.Fatal(err)
	}
	state, err = client.DeleteAccountEmail(ctx, state, AccountEmail{ID: 3, Address: "new@example.com", Removable: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /account/manage/email/alternate/add/verification", "PUT /account/manage/email/alternate/verification", "DELETE /account/manage/email/alternate/3"}
	if !reflect.DeepEqual(calls, want) || len(state.Cookies) == 0 {
		t.Fatalf("calls=%v cookies=%d", calls, len(state.Cookies))
	}
}

func TestAccountEmailProfileRejectsWrongIdentityAndIncompleteResponses(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
		want       error
	}{
		{"expired", `{}`, 401, ErrAccountWebAuth},
		{"wrong account", strings.ReplaceAll(accountEmailProfileFixture, "owner@example.com", "stranger@example.com"), 200, ErrAccountWebIdentity},
		{"missing email list", `{"apiKey":"key","appleID":{"name":"owner@example.com"},"primaryEmailAddress":{"address":"owner@example.com"}}`, 200, ErrInvalidResponse},
		{"duplicate ID", strings.Replace(accountEmailProfileFixture, `"id":3`, `"id":1`, 1), 200, ErrInvalidResponse},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := NewClient(Config{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return testResponse(request, tt.status, tt.body, nil), nil
			})})
			_, _, err := client.ListAccountEmails(context.Background(), AccountWebSession{AppleID: "owner@example.com", ServiceKey: "key"})
			if !errors.Is(err, tt.want) {
				t.Fatalf("err=%v want=%v", err, tt.want)
			}
		})
	}
}

func TestAccountEmailDeletionNeverRetriesOrClaimsMismatchedSuccess(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  int
		body    string
		failure error
		want    error
	}{
		{"transport", 0, "", io.ErrUnexpectedEOF, ErrService},
		{"expired", 403, `{}`, nil, ErrAccountWebAuth},
		{"Apple rejected", 400, `{"serviceErrors":[{"code":"invalid"}]}`, nil, ErrAccountEmailInvalid},
		{"wrong deleted address", 200, `{"emailAddressRemoved":{"id":4,"address":"other@example.com"}}`, nil, ErrInvalidResponse},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			client, _ := NewClient(Config{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if tt.failure != nil {
					return nil, tt.failure
				}
				return testResponse(request, tt.status, tt.body, nil), nil
			})})
			_, err := client.DeleteAccountEmail(context.Background(), AccountWebSession{AppleID: "owner@example.com", ServiceKey: "key", APIKey: "api"}, AccountEmail{ID: 3, Address: "new@example.com", Removable: true})
			if !errors.Is(err, tt.want) || calls != 1 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
			var typed *Error
			if errors.As(err, &typed) && typed.Retryable {
				t.Fatal("mutation may be replayed")
			}
		})
	}
}
