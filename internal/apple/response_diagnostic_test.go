package apple

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCreateAliasRetainsSanitizedBusinessFailure(t *testing.T) {
	body := `{"success":false,"error":{"errorCode":-27577,"errorMessage":"Apple rejected the requested alias for owner@example.com; echo abcdef-ghijk / abcdef"},"sessionToken":"abcdef","nested":[{"authorization":"Bearer bearer-secret","SESSION-ID":"abcdef-ghijk","credential":{"password":"password-secret"},"dsid":123456789012,"forwardToEmail":"owner@example.com"}]}`
	alias, upstream, requests := createWithDiagnosticResponse(t, 0, func(request *http.Request) *http.Response {
		return testResponse(request, http.StatusOK, body, http.Header{
			"X-Apple-Session-Token": {"response-header-secret"},
			"Set-Cookie":            {"credential=cookie-secret; Path=/"},
		})
	})
	if requests != 2 || alias != (Alias{}) || upstream.ServiceCode != "-27577" || !upstream.ServiceRejected || upstream.Retryable || !errors.Is(upstream, ErrService) {
		t.Fatalf("creation behavior changed: requests=%d alias=%#v error=%#v", requests, alias, upstream)
	}
	diagnostic, ok := upstream.ResponseDiagnostic()
	if !ok || diagnostic.Format != "json" || diagnostic.Truncated || diagnostic.OriginalBytes != len(body) || diagnostic.ServiceCode != "-27577" {
		t.Fatalf("diagnostic metadata = %#v, present=%v", diagnostic, ok)
	}
	if !json.Valid([]byte(diagnostic.Body)) || !strings.Contains(diagnostic.Body, `"errorCode": -27577`) || !strings.Contains(diagnostic.Body, "Apple rejected the requested alias") {
		t.Fatalf("business detail missing from diagnostic: %s", diagnostic.Body)
	}
	for _, secret := range []string{"owner@example.com", "abcdef", "ghijk", "bearer-secret", "password-secret", "123456789012", "response-header-secret", "cookie-secret", "request-session-secret"} {
		if strings.Contains(diagnostic.Body, secret) {
			t.Errorf("diagnostic retained %q: %s", secret, diagnostic.Body)
		}
	}
	encoded, err := json.Marshal(upstream)
	if err != nil {
		t.Fatal(err)
	}
	for _, generic := range []string{upstream.Error(), fmt.Sprintf("%+v %#v", upstream, upstream), string(encoded)} {
		if strings.Contains(generic, "Apple rejected the requested alias") {
			t.Fatalf("generic error formatting exposed the diagnostic: %s", generic)
		}
	}
	diagnostic.Body = "changed by caller"
	diagnostic.ServiceCode = "CHANGED"
	again, _ := upstream.ResponseDiagnostic()
	if again.Body == diagnostic.Body || again.ServiceCode == diagnostic.ServiceCode {
		t.Fatal("changing the returned value changed the stored diagnostic")
	}
}

func TestCreateAliasDiagnosticForNonJSONAndIncompleteResponses(t *testing.T) {
	partial := `{"success":false,"token":"partial-secret"`
	tests := []struct {
		name          string
		status        int
		body          string
		contentType   string
		readFailure   bool
		limit         int64
		wantFormat    string
		wantText      string
		wantKind      error
		wantCandidate bool
		wantTruncated bool
	}{
		{
			name: "plain service error", status: http.StatusBadRequest, contentType: "text/plain",
			body:       "Alias reservation rejected (-27577) for owner@example.com\nAuthorization: Bearer bearer-secret\nCookie: a=one-secret; b=two-secret\npassword=correct horse battery staple\nDSID 123456789012",
			wantFormat: "text", wantText: "Alias reservation rejected (-27577)", wantKind: ErrService,
		},
		{
			name: "malformed JSON", status: http.StatusOK, body: partial, contentType: "text/plain",
			wantFormat: "omitted", wantText: "malformed JSON", wantKind: ErrInvalidResponse, wantCandidate: true,
		},
		{
			name: "JSON with trailing data", status: http.StatusOK, body: `{"success":false} token=trailing-secret`,
			wantFormat: "omitted", wantText: "complete JSON document", wantKind: ErrInvalidResponse, wantCandidate: true,
		},
		{
			name: "HTML gateway error", status: http.StatusBadGateway, body: `<html><script>token="script-secret"</script></html>`, contentType: "text/html",
			wantFormat: "omitted", wantText: "HTML or XML", wantKind: ErrService, wantCandidate: true,
		},
		{
			name: "response limit", status: http.StatusOK, body: strings.Repeat("limit-secret", 100), limit: 1024,
			wantFormat: "omitted", wantText: "not read completely", wantKind: ErrResponseTooLarge, wantCandidate: true, wantTruncated: true,
		},
		{
			name: "interrupted read", status: http.StatusOK, body: partial, readFailure: true,
			wantFormat: "omitted", wantText: "not read completely", wantKind: ErrInvalidResponse, wantCandidate: true, wantTruncated: true,
		},
		{
			name: "binary response", status: http.StatusOK, body: "\xff binary-secret", contentType: "application/octet-stream",
			wantFormat: "omitted", wantText: "valid UTF-8", wantKind: ErrInvalidResponse, wantCandidate: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			alias, upstream, requests := createWithDiagnosticResponse(t, test.limit, func(request *http.Request) *http.Response {
				response := testResponse(request, test.status, test.body, http.Header{"Content-Type": {test.contentType}})
				if test.readFailure {
					response.Body = &diagnosticFailingBody{body: test.body}
				}
				return response
			})
			if requests != 2 || upstream.Retryable || !errors.Is(upstream, test.wantKind) || (alias.HME != "") != test.wantCandidate {
				t.Fatalf("creation behavior changed: requests=%d alias=%#v error=%#v", requests, alias, upstream)
			}
			diagnostic, ok := upstream.ResponseDiagnostic()
			wantBytes := len(test.body)
			if test.limit > 0 {
				wantBytes = min(wantBytes, int(test.limit)+1)
			}
			if !ok || diagnostic.Format != test.wantFormat || diagnostic.Truncated != test.wantTruncated || diagnostic.OriginalBytes != wantBytes || !strings.Contains(diagnostic.Body, test.wantText) {
				t.Fatalf("diagnostic = %#v, present=%v", diagnostic, ok)
			}
			for _, secret := range []string{"owner@example.com", "bearer-secret", "one-secret", "two-secret", "correct horse battery staple", "123456789012", "partial-secret", "trailing-secret", "script-secret", "limit-secret", "binary-secret"} {
				if strings.Contains(diagnostic.Body, secret) {
					t.Errorf("diagnostic retained %q: %s", secret, diagnostic.Body)
				}
			}
			if test.readFailure && !errors.Is(upstream, io.ErrUnexpectedEOF) {
				t.Fatal("body read cause was lost")
			}
		})
	}
}

func TestCreateAliasDiagnosticBoundsAndServiceCode(t *testing.T) {
	for _, size := range []int{300, 700, maxDiagnosticInputBytes} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			body := `{"success":false,"details":"` + strings.Repeat("错误", size) + `","error":{"errorCode":-27577,"errorMessage":"Alias rejected"}}`
			_, upstream, _ := createWithDiagnosticResponse(t, 0, func(request *http.Request) *http.Response {
				return testResponse(request, http.StatusOK, body, nil)
			})
			diagnostic, ok := upstream.ResponseDiagnostic()
			if !ok || !diagnostic.Truncated || len(diagnostic.Body) > maxResponseDiagnosticBytes || !utf8.ValidString(diagnostic.Body) || diagnostic.OriginalBytes != len(body) || diagnostic.ServiceCode != "-27577" {
				t.Fatalf("bounded diagnostic = %#v, present=%v", diagnostic, ok)
			}
		})
	}
	for _, code := range []string{"-27577", "GENERATE_REJECTED", "Bearer private-secret", "owner@example.com", strings.Repeat("A", 65)} {
		t.Run(code, func(t *testing.T) {
			encoded, err := json.Marshal(map[string]any{"success": false, "errorCode": code})
			if err != nil {
				t.Fatal(err)
			}
			_, upstream, _ := createWithDiagnosticResponse(t, 0, func(request *http.Request) *http.Response {
				return testResponse(request, http.StatusOK, string(encoded), nil)
			})
			diagnostic, _ := upstream.ResponseDiagnostic()
			wantCode := ""
			if code == "-27577" || code == "GENERATE_REJECTED" {
				wantCode = code
			}
			if diagnostic.ServiceCode != wantCode || upstream.ServiceCode != code {
				t.Fatalf("safe code=%q raw code=%q, want %q / %q", diagnostic.ServiceCode, upstream.ServiceCode, wantCode, code)
			}
		})
	}
}

func TestResponseDiagnosticRestrictedToHMEOperations(t *testing.T) {
	body := []byte(`{"errorCode":"REJECTED","message":"private auth detail"}`)
	for _, operation := range []string{"initialize sign in", "federate Apple ID", "initialize SRP", "decode SRP challenge", "complete SRP sign in", "request two-factor code", "trust Apple session", "exchange Apple session token", "decode Apple account", "validate Apple session", "decode Apple session", "unrecognized Hide My Email operation"} {
		upstream := (responseData{body: body}).operationError(operation, ErrService, nil)
		if diagnostic, ok := upstream.ResponseDiagnostic(); ok {
			t.Errorf("%s retained diagnostic: %#v", operation, diagnostic)
		}
	}
	for _, operation := range []string{"list Hide My Email aliases", "decode Hide My Email list", "generate Hide My Email alias", "decode generate Hide My Email alias", "decode generated Hide My Email alias", "reserve Hide My Email alias", "decode reserve Hide My Email alias", "decode reserved Hide My Email alias", "update Hide My Email forwarding target", "decode update Hide My Email forwarding target", "deactivate Hide My Email alias", "decode deactivate Hide My Email alias", "delete Hide My Email alias", "decode delete Hide My Email alias"} {
		upstream := (responseData{body: body}).operationError(operation, ErrInvalidResponse, nil)
		if _, ok := upstream.ResponseDiagnostic(); !ok {
			t.Errorf("%s did not retain a diagnostic", operation)
		}
	}
	var upstream *Error
	if _, ok := upstream.ResponseDiagnostic(); ok {
		t.Fatal("nil error has a diagnostic")
	}
}

func TestResponseDiagnosticRedactsSensitiveAssignmentsInMessages(t *testing.T) {
	for _, message := range []string{
		`{"password":"fixture-secret","dsid":"123456789"}`,
		`failed: "token": "fixture-secret"`,
		"privateKey=fixture-secret\nsignature=fixture-secret\nuserId=123456789\nusername=fixture-secret",
		"X-Apple-Auth-Attributes: fixture-secret\nApple ID: 123456789\nclient_id=fixture-secret",
		"diagnostic: Bearer fixture-secret\nemail=owner%40icloud.com",
	} {
		t.Run(message, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"success": false, "errorCode": -27577, "errorMessage": "Alias rejected", "message": message})
			if err != nil {
				t.Fatal(err)
			}
			for _, response := range []struct {
				body []byte
				kind string
			}{{body, "application/json"}, {[]byte("Alias rejected\n" + message), "text/plain"}} {
				diagnostic := makeResponseDiagnostic(response.body, response.kind, false)
				if !strings.Contains(diagnostic.Body, "Alias rejected") {
					t.Fatalf("business message was removed: %#v", diagnostic)
				}
				for _, secret := range []string{"fixture-secret", "123456789", "owner%40icloud.com"} {
					if strings.Contains(diagnostic.Body, secret) {
						t.Errorf("message retained %q in %s diagnostic: %s", secret, response.kind, diagnostic.Body)
					}
				}
			}
		})
	}
}

func TestResponseDiagnosticDecodesEmbeddedJSONBeforeRedaction(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"success": false, "errorCode": -27577, "errorMessage": "Alias rejected",
		"details": `{"pass\u0077ord":"fixture-secret","dsid":"123456789"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	diagnostic := makeResponseDiagnostic(body, "application/json", false)
	if !strings.Contains(diagnostic.Body, "Alias rejected") || strings.Contains(diagnostic.Body, "fixture-secret") || strings.Contains(diagnostic.Body, "123456789") {
		t.Fatalf("embedded JSON was not sanitized: %s", diagnostic.Body)
	}
}

func createWithDiagnosticResponse(t *testing.T, limit int64, reserve func(*http.Request) *http.Response) (Alias, *Error, int) {
	t.Helper()
	requests := 0
	client, err := NewClient(Config{MaxResponseBytes: limit, Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		switch request.URL.Path {
		case "/v1/hme/generate":
			return testResponse(request, http.StatusOK, `{"success":true,"result":{"hme":"candidate@icloud.com"}}`, nil), nil
		case "/v1/hme/reserve":
			return reserve(request), nil
		default:
			return nil, fmt.Errorf("unexpected path %s", request.URL.Path)
		}
	})})
	if err != nil {
		t.Fatal(err)
	}
	session := retryAfterTestSession()
	session.SessionToken = "request-session-secret"
	alias, _, err := client.CreateAlias(context.Background(), session, "label", "note")
	var upstream *Error
	if !errors.As(err, &upstream) {
		t.Fatalf("expected an Apple failure, got %v", err)
	}
	return alias, upstream, requests
}

type diagnosticFailingBody struct {
	body string
}

func (body *diagnosticFailingBody) Read(buffer []byte) (int, error) {
	return copy(buffer, body.body), io.ErrUnexpectedEOF
}

func (*diagnosticFailingBody) Close() error { return nil }
