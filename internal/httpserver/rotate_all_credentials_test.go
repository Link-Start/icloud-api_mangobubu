package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

func TestAdminAPIRotateAllCredentialsGuardsResponseRevocationAndAudit(t *testing.T) {
	env := newAdminAPITestEnv(t)
	ctx := context.Background()
	account := adminAPITestCreateAccount(t, env, "rotate-all-http@icloud.com")

	legacyKey, legacyHash, legacyPrefix, err := secure.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := env.store.CreateAlias(ctx, domain.Alias{
		AccountID: account.ID, Address: "rotate-all-http-legacy@icloud.com",
		APIKeyHash: legacyHash, APIKeyPrefix: legacyPrefix,
		CredentialMode: domain.AliasCredentialModeLegacy, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	oldLegacyDirect, err := env.cipher.DirectLinkToken(legacy.ID, legacy.APIKeyHash)
	if err != nil {
		t.Fatal(err)
	}

	v2, oldCredentials := createV2AliasFixture(
		t, env, account.ID, "rotate-all-http-v2@icloud.com",
	)
	oldRecentToken, err := env.cipher.RecentMailToken(v2.ID, v2.APIKeyHash)
	if err != nil {
		t.Fatal(err)
	}
	oldOTPToken, err := env.cipher.OTPToken(v2.ID, v2.APIKeyHash)
	if err != nil {
		t.Fatal(err)
	}
	tokenNow := time.Date(2026, time.August, 30, 2, 3, 4, 0, time.UTC)
	oldAccessToken, err := env.cipher.IssueAliasAccessToken(
		v2.ID, v2.CredentialVersion, v2.RefreshTokenHash, tokenNow.Add(time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}

	pending, oldPendingCredentials := createV2AliasFixture(
		t, env, account.ID, "rotate-all-http-pending@icloud.com",
	)
	oldPendingCiphertext, err := env.cipher.EncryptPendingAliasAPIKey(oldPendingCredentials.APIKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.DB().ExecContext(ctx, `
		UPDATE aliases SET enabled = FALSE, last_sync_error = ? WHERE id = ?`,
		domain.AppleAliasConfirmationPending, pending.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.DB().ExecContext(ctx, `
		INSERT INTO pending_alias_api_keys(alias_id, api_key_ciphertext, created_at)
		VALUES(?, ?, 1)`, pending.ID, oldPendingCiphertext,
	); err != nil {
		t.Fatal(err)
	}

	sessionCookie, csrf, admin := env.createSession(t, "rotate-all-admin", "unused-password")
	path := "/admin/api/v1/aliases/rotate-all-credentials"
	validBody := adminAPITestJSON(t, map[string]string{
		"confirmation": "ROTATE_ALL", "current_password": "unused-password",
	})
	legacyBefore, _ := env.store.GetAlias(ctx, legacy.ID)
	v2Before, _ := env.store.GetAlias(ctx, v2.ID)
	pendingBefore, _ := env.store.GetAlias(ctx, pending.ID)

	for _, test := range []struct {
		name       string
		body       []byte
		cookies    []*http.Cookie
		csrf       string
		wantStatus int
		wantCode   string
	}{
		{
			name: "without session", body: validBody,
			wantStatus: http.StatusUnauthorized, wantCode: "AUTH_REQUIRED",
		},
		{
			name: "without CSRF", body: validBody, cookies: []*http.Cookie{sessionCookie},
			wantStatus: http.StatusForbidden, wantCode: "CSRF_INVALID",
		},
		{
			name: "wrong confirmation",
			body: adminAPITestJSON(t, map[string]string{
				"confirmation": "rotate_all", "current_password": "unused-password",
			}),
			cookies: []*http.Cookie{sessionCookie}, csrf: csrf,
			wantStatus: http.StatusBadRequest, wantCode: "VALIDATION_FAILED",
		},
		{
			name: "wrong current password",
			body: adminAPITestJSON(t, map[string]string{
				"confirmation": "ROTATE_ALL", "current_password": "wrong-password",
			}),
			cookies: []*http.Cookie{sessionCookie}, csrf: csrf,
			wantStatus: http.StatusUnauthorized, wantCode: "CURRENT_PASSWORD_INVALID",
		},
		{
			name: "unknown JSON field",
			body: adminAPITestJSON(t, map[string]any{
				"confirmation":       "ROTATE_ALL",
				"current_password":   "unused-password",
				"return_credentials": true,
			}),
			cookies: []*http.Cookie{sessionCookie}, csrf: csrf,
			wantStatus: http.StatusBadRequest, wantCode: "INVALID_JSON",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := env.request(
				t, http.MethodPost, path, test.body, "application/json",
				test.cookies, test.csrf,
			)
			if response.Code != test.wantStatus ||
				adminAPITestErrorCode(t, response) != test.wantCode {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
		})
	}

	legacyAfterGuards, _ := env.store.GetAlias(ctx, legacy.ID)
	v2AfterGuards, _ := env.store.GetAlias(ctx, v2.ID)
	pendingAfterGuards, _ := env.store.GetAlias(ctx, pending.ID)
	if !reflect.DeepEqual(legacyAfterGuards, legacyBefore) ||
		!reflect.DeepEqual(v2AfterGuards, v2Before) ||
		!reflect.DeepEqual(pendingAfterGuards, pendingBefore) {
		t.Fatal("a rejected bulk-rotation request mutated alias credentials")
	}
	failedAudits, err := env.store.ListAuditLogsFiltered(ctx, store.AuditLogFilter{
		Action: "rotate_all_credentials", Result: "failed", Limit: 10,
	})
	if err != nil || len(failedAudits) != 1 ||
		failedAudits[0].Detail != "current_password_invalid" {
		t.Fatalf("password failure audit rows = %#v, err=%v", failedAudits, err)
	}

	env.server.cfg.AdminPath = "/0123456789abcdef0123456789abcdef/admin"
	response := env.request(
		t, http.MethodPost, path, validBody, "application/json",
		[]*http.Cookie{sessionCookie}, csrf,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("rotate-all response = %d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("rotate-all Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode rotate-all response: %v", err)
	}
	if len(envelope) != 1 {
		t.Fatalf("rotate-all response envelope keys = %#v", envelope)
	}
	var result struct {
		Total                    int  `json:"total"`
		Rotated                  int  `json:"rotated"`
		MigratedLegacy           int  `json:"migrated_legacy"`
		RotatedV2                int  `json:"rotated_v2"`
		RotatedPending           int  `json:"rotated_pending"`
		ReauthenticationRequired bool `json:"reauthentication_required"`
	}
	if err := json.Unmarshal(envelope["data"], &result); err != nil {
		t.Fatalf("decode rotate-all counts: %v", err)
	}
	if result.Total != 3 || result.Rotated != 3 || result.MigratedLegacy != 1 ||
		result.RotatedV2 != 2 || result.RotatedPending != 1 ||
		!result.ReauthenticationRequired {
		t.Fatalf("rotate-all result = %#v", result)
	}
	lowerBody := strings.ToLower(response.Body.String())
	for _, forbidden := range []string{
		"api_key", "ciphertext", "password", "refresh_token", "access_token", "client_id",
	} {
		if strings.Contains(lowerBody, forbidden) {
			t.Fatalf("rotate-all response leaked %q: %s", forbidden, response.Body.String())
		}
	}

	audits, err := env.store.ListAuditLogsFiltered(ctx, store.AuditLogFilter{
		Action: "rotate_all_credentials", Result: "success", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 1 || audits[0].AdminID == nil || *audits[0].AdminID != admin.ID ||
		audits[0].Username != admin.Username || audits[0].ResourceType != "alias" ||
		audits[0].ResourceID != "all" || audits[0].Result != "success" {
		t.Fatalf("rotate-all audit rows = %#v", audits)
	}
	for _, expectedDetail := range []string{
		`"total":3`, `"rotated":3`, `"migrated_legacy":1`,
		`"rotated_v2":2`, `"rotated_pending":1`,
	} {
		if !strings.Contains(audits[0].Detail, expectedDetail) {
			t.Fatalf("rotate-all audit detail = %q", audits[0].Detail)
		}
	}
	adminAfter, err := env.store.GetAdminByID(ctx, admin.ID)
	if err != nil || adminAfter.PasswordVersion != admin.PasswordVersion+1 {
		t.Fatalf("admin after rotate-all = %#v, err=%v", adminAfter, err)
	}
	rawSession := "admin-api-session-" + admin.Username
	if _, err := env.store.GetSessionByHash(
		ctx, secure.HashToken(rawSession),
	); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old admin session lookup error = %v", err)
	}
	clearedPaths := map[string]bool{}
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "icloud_admin_session" && cookie.MaxAge < 0 {
			clearedPaths[cookie.Path] = true
		}
	}
	if !clearedPaths["/admin"] ||
		!clearedPaths["/0123456789abcdef0123456789abcdef/admin"] {
		t.Fatalf("cleared session cookie paths = %#v", clearedPaths)
	}

	rotatedPending, err := env.store.GetAlias(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	var pendingCiphertext string
	var pendingCreatedAt int64
	if err := env.store.DB().QueryRowContext(ctx, `
		SELECT api_key_ciphertext, created_at
		FROM pending_alias_api_keys WHERE alias_id = ?`, pending.ID,
	).Scan(&pendingCiphertext, &pendingCreatedAt); err != nil {
		t.Fatal(err)
	}
	newPendingKey, err := env.cipher.DecryptPendingAliasAPIKey(pendingCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	newPendingCredentials, err := env.cipher.DecryptAliasCredentials(
		rotatedPending.ID, rotatedPending.CredentialCiphertext,
	)
	if err != nil {
		t.Fatal(err)
	}
	if pendingCiphertext == oldPendingCiphertext ||
		newPendingKey != newPendingCredentials.APIKey ||
		newPendingKey == oldPendingCredentials.APIKey || pendingCreatedAt != 1 {
		t.Fatalf("HTTP pending rotation mismatch")
	}

	fullRouter, err := env.server.Router()
	if err != nil {
		t.Fatal(err)
	}
	for name, fixture := range map[string]struct {
		method        string
		target        string
		body          string
		authorization string
		contentType   string
	}{
		"legacy direct link": {
			method: http.MethodGet,
			target: "/api/v1/mail/recent?api_key=" + url.QueryEscape(oldLegacyDirect),
		},
		"v2 recent link": {
			method: http.MethodGet,
			target: "/api/v1/mail/recent?api_key=" + url.QueryEscape(oldRecentToken),
		},
		"v2 OTP link": {
			method: http.MethodGet,
			target: "/api/v1/otp?token=" + url.QueryEscape(oldOTPToken),
		},
		"v2 API key": {
			method: http.MethodGet, target: "/api/v1/otp",
			authorization: "Bearer " + oldCredentials.APIKey,
		},
		"v2 OAuth refresh": {
			method: http.MethodPost, target: "/oauth2/v2.0/token",
			body: url.Values{
				"grant_type":    {"refresh_token"},
				"client_id":     {oldCredentials.ClientID},
				"refresh_token": {oldCredentials.RefreshToken},
			}.Encode(),
			contentType: "application/x-www-form-urlencoded",
		},
	} {
		headers := map[string]string{}
		if fixture.authorization != "" {
			headers["Authorization"] = fixture.authorization
		}
		if fixture.contentType != "" {
			headers["Content-Type"] = fixture.contentType
		}
		rejected := serveV2Request(
			fullRouter, fixture.method, fixture.target, fixture.body, headers,
		)
		if rejected.Code != http.StatusUnauthorized {
			t.Fatalf("old %s response = %d %s", name, rejected.Code, rejected.Body.String())
		}
	}

	rotatedV2, err := env.store.GetAlias(ctx, v2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if env.cipher.VerifyAliasAccessToken(
		oldAccessToken, rotatedV2.ID, rotatedV2.CredentialVersion,
		rotatedV2.RefreshTokenHash, tokenNow,
	) {
		t.Fatal("old OAuth access token survived bulk credential rotation")
	}
	if legacyKey == "" {
		t.Fatal("legacy fixture API key unexpectedly empty")
	}
}

func TestAdminAPIRotateAllCredentialsCurrentPasswordLimiterIsBounded(t *testing.T) {
	env := newAdminAPITestEnv(t)
	ctx := context.Background()
	account := adminAPITestCreateAccount(t, env, "rotate-all-limit@icloud.com")
	alias, _ := createV2AliasFixture(t, env, account.ID, "rotate-all-limit-alias@icloud.com")
	before, err := env.store.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatal(err)
	}
	sessionCookie, csrf, _ := env.createSession(t, "rotate-all-limit-admin", "correct-password")
	body := adminAPITestJSON(t, map[string]string{
		"confirmation": "ROTATE_ALL", "current_password": "wrong-password",
	})
	path := "/admin/api/v1/aliases/rotate-all-credentials"

	for attempt := 1; attempt <= 6; attempt++ {
		response := env.request(
			t, http.MethodPost, path, body, "application/json",
			[]*http.Cookie{sessionCookie}, csrf,
		)
		if attempt <= 5 {
			if response.Code != http.StatusUnauthorized ||
				adminAPITestErrorCode(t, response) != "CURRENT_PASSWORD_INVALID" {
				t.Fatalf("password attempt %d = %d %s", attempt, response.Code, response.Body.String())
			}
			continue
		}
		if response.Code != http.StatusTooManyRequests ||
			adminAPITestErrorCode(t, response) != "RATE_LIMITED" {
			t.Fatalf("limited attempt = %d %s", response.Code, response.Body.String())
		}
	}

	after, err := env.store.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatal("password attempts changed alias credentials")
	}
	audits, err := env.store.ListAuditLogsFiltered(ctx, store.AuditLogFilter{
		Action: "rotate_all_credentials", Result: "failed", Limit: 10,
	})
	if err != nil || len(audits) != 5 {
		t.Fatalf("bounded password failure audits = %#v, err=%v", audits, err)
	}
	for _, audit := range audits {
		if audit.Detail != "current_password_invalid" {
			t.Fatalf("password failure audit detail = %q", audit.Detail)
		}
	}
}

func TestAdminAPIRotateAllCredentialsRevalidatesSessionAfterWaitingForWriteLock(t *testing.T) {
	env := newAdminAPITestEnv(t)
	ctx := context.Background()
	account := adminAPITestCreateAccount(t, env, "rotate-all-session-race@icloud.com")
	alias, _ := createV2AliasFixture(
		t, env, account.ID, "rotate-all-session-race-alias@icloud.com",
	)
	before, err := env.store.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatal(err)
	}
	sessionCookieValue, csrf, admin := env.createSession(
		t, "rotate-all-session-race-admin", "correct-password",
	)
	rawSession := "admin-api-session-" + admin.Username
	reachedBeforeLock := make(chan struct{})
	releaseWriteLock := make(chan struct{})
	env.server.beforeCredentialRotationLock = func() {
		close(reachedBeforeLock)
		<-releaseWriteLock
	}
	t.Cleanup(func() {
		env.server.beforeCredentialRotationLock = nil
	})

	body := adminAPITestJSON(t, map[string]string{
		"confirmation": "ROTATE_ALL", "current_password": "correct-password",
	})
	responseDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responseDone <- env.request(
			t, http.MethodPost, "/admin/api/v1/aliases/rotate-all-credentials",
			body, "application/json", []*http.Cookie{sessionCookieValue}, csrf,
		)
	}()
	<-reachedBeforeLock
	if err := env.store.DeleteSession(ctx, secure.HashToken(rawSession)); err != nil {
		close(releaseWriteLock)
		t.Fatal(err)
	}
	close(releaseWriteLock)

	response := <-responseDone
	if response.Code != http.StatusUnauthorized ||
		adminAPITestErrorCode(t, response) != "SESSION_EXPIRED" {
		t.Fatalf("session-race response = %d %s", response.Code, response.Body.String())
	}
	after, err := env.store.GetAlias(ctx, alias.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatal("revoked session rotated alias credentials after acquiring the write lock")
	}
	adminAfter, err := env.store.GetAdminByID(ctx, admin.ID)
	if err != nil || adminAfter.PasswordVersion != admin.PasswordVersion {
		t.Fatalf("admin changed after rejected session race = %#v, err=%v", adminAfter, err)
	}
	successAudits, err := env.store.ListAuditLogsFiltered(ctx, store.AuditLogFilter{
		Action: "rotate_all_credentials", Result: "success", Limit: 10,
	})
	if err != nil || len(successAudits) != 0 {
		t.Fatalf("success audits after rejected session race = %#v, err=%v", successAudits, err)
	}
}
