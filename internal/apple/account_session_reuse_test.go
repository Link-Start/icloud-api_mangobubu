package apple

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func accountReuseSession() Session {
	return Session{
		AppleID: "owner@example.com", Region: RegionChina, DSID: "test-dsid",
		SessionToken: "icloud-token", SCNT: "icloud-scnt", SessionID: "icloud-session",
		AuthAttributes: "icloud-attributes", FrameID: "icloud-frame",
		Cookies: []PersistentCookie{
			{Name: "shared-auth", Value: "trusted", Domain: "apple.com", Path: "/", Secure: true},
			{Name: "idmsa-auth", Value: "trusted", Domain: "idmsa.apple.com", Path: "/", HostOnly: true, Secure: true},
			{Name: "icloud-only", Value: "private", Domain: "icloud.com.cn", Path: "/", Secure: true},
		},
	}
}

func reuseResponse(request *http.Request) (*http.Response, error) {
	switch request.URL.Path {
	case "/":
		return testResponse(request, http.StatusOK, "<html></html>", http.Header{"Scnt": {"portal-scnt"}}), nil
	case "/bootstrap/portal":
		return testResponse(request, http.StatusOK, `{"serviceKey":"portal-service-key"}`, nil), nil
	case "/appleauth/auth/authorize/signin":
		return testResponse(request, http.StatusOK, "<html></html>", http.Header{
			"Scnt": {"authorized-scnt"}, "Set-Cookie": {"portal-auth=granted; Domain=.apple.com; Path=/; Secure; HttpOnly"},
		}), nil
	case "/account/manage/gs/ws/token":
		return testResponse(request, http.StatusOK, `{"token":"portal-token"}`, nil), nil
	case "/account/manage":
		return testResponse(request, http.StatusOK, accountEmailProfileFixture, nil), nil
	default:
		return nil, errors.New("unexpected reuse endpoint")
	}
}

func TestResumeAccountSessionReusesScopedCookiesWithoutPasswordsOrChangingICloud(t *testing.T) {
	var paths []string
	client, err := NewClient(Config{Random: deterministicRandom(), Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		if request.Method != http.MethodGet || request.Body != nil {
			t.Fatalf("session reuse attempted a write: %s %s", request.Method, request.URL.Path)
		}
		cookies := request.Header.Get("Cookie")
		if !strings.Contains(cookies, "shared-auth=trusted") || strings.Contains(cookies, "icloud-only") {
			t.Fatal("login cookies were missing or crossed their domain boundary")
		}
		if request.URL.Host != "idmsa.apple.com" && strings.Contains(cookies, "idmsa-auth") {
			t.Fatal("host-only IDMSA cookie sent to another host")
		}
		if request.URL.Path == "/appleauth/auth/authorize/signin" {
			if !strings.Contains(cookies, "idmsa-auth=trusted") || request.URL.Query().Get("client_id") != "portal-service-key" ||
				request.URL.Query().Get("redirect_uri") != accountPortalOrigin || request.Header.Get("X-Apple-ID-Session-Id") != "icloud-session" ||
				request.Header.Get("scnt") != "portal-scnt" {
				t.Fatal("portal authorization did not reuse the authenticated context")
			}
		}
		if request.URL.Path == "/account/manage" && (!strings.Contains(cookies, "portal-auth=granted") || request.Header.Get("scnt") != "authorized-scnt") {
			t.Fatal("management request lost the newly authorized portal session")
		}
		return reuseResponse(request)
	})})
	if err != nil {
		t.Fatal(err)
	}
	original := accountReuseSession()
	before := accountReuseSession()
	profile, portal, err := client.ResumeAccountSession(context.Background(), original)
	if err != nil || profile.AppleID != original.AppleID || portal.APIKey != "portal-api-key" {
		t.Fatalf("profile=%#v, err=%v", profile, err)
	}
	if !reflect.DeepEqual(original, before) {
		t.Fatal("reuse changed the iCloud session")
	}
	wantPaths := []string{"/", "/bootstrap/portal", "/appleauth/auth/authorize/signin", "/account/manage/gs/ws/token", "/account/manage"}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("requests=%v", paths)
	}
	paths = nil
	if _, _, err := client.ListAccountEmails(context.Background(), portal); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths, []string{"/account/manage"}) {
		t.Fatalf("valid portal session was reauthorized: %v", paths)
	}
}

func TestResumeAccountSessionRequiresLoginOnlyOnUpstreamAuthDenial(t *testing.T) {
	for _, tt := range []struct {
		name, path    string
		status        int
		body          string
		failure, want error
	}{
		{"unauthorized", "/account/manage/gs/ws/token", 401, `{}`, nil, ErrAccountWebAuth},
		{"forbidden", "/account/manage", 403, `{}`, nil, ErrAccountWebAuth},
		{"authorization denied", "/appleauth/auth/authorize/signin", 401, `{}`, nil, ErrAccountWebAuth},
		{"authorization outage", "/appleauth/auth/authorize/signin", 503, `{}`, nil, ErrService},
		{"rate limited", "/appleauth/auth/authorize/signin", 429, `{}`, nil, ErrService},
		{"terms required", "/appleauth/auth/authorize/signin", 412, `{}`, nil, ErrTermsRequired},
		{"network timeout", "/bootstrap/portal", 0, "", context.DeadlineExceeded, context.DeadlineExceeded},
		{"missing service key", "/bootstrap/portal", 200, `{}`, nil, ErrInvalidResponse},
		{"invalid profile", "/account/manage", 200, `<html>unavailable</html>`, nil, ErrInvalidResponse},
		{"wrong identity", "/account/manage", 200, strings.ReplaceAll(accountEmailProfileFixture, "owner@example.com", "other@example.com"), nil, ErrAccountWebIdentity},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requests := 0
			client, err := NewClient(Config{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests++
				if request.Method != http.MethodGet {
					t.Fatal("reuse attempted password or device-code authentication")
				}
				if request.URL.Path == tt.path {
					if tt.failure != nil {
						return nil, tt.failure
					}
					return testResponse(request, tt.status, tt.body, nil), nil
				}
				return reuseResponse(request)
			})})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = client.ResumeAccountSession(context.Background(), accountReuseSession())
			if !errors.Is(err, tt.want) {
				t.Fatalf("err=%v, want=%v", err, tt.want)
			}
			if tt.want != ErrAccountWebAuth && errors.Is(err, ErrAccountWebAuth) {
				t.Fatal("non-auth failure requested a new login")
			}
			if requests < 1 {
				t.Fatal("local state rejected reuse before contacting Apple")
			}
		})
	}
}
