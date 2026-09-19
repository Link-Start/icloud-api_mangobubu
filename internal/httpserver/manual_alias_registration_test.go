package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/hmesync"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

type manualRegistrationAppleClient struct {
	hmesync.AppleClient
	directory   apple.ListResult
	validateErr error
	listErr     error
	listCalls   int
}

func (client *manualRegistrationAppleClient) Validate(_ context.Context, session apple.Session) (apple.Session, error) {
	return session, client.validateErr
}

func (client *manualRegistrationAppleClient) ListAliases(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
	client.listCalls++
	return client.directory, session, client.listErr
}

// Existing compatibility tests exercise the real registration service with a
// fixed Apple directory. They never accept an unverified address by default.
func connectManualAliasDirectory(t *testing.T, env *adminAPITestEnv, accountID int64, addresses ...string) *manualRegistrationAppleClient {
	t.Helper()
	ctx := context.Background()
	account, err := env.store.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatalf("get fixture account: %v", err)
	}
	client := &manualRegistrationAppleClient{directory: apple.ListResult{SelectedForwardTo: account.Email}}
	for _, address := range addresses {
		client.directory.Aliases = append(client.directory.Aliases, apple.Alias{
			HME: address, ForwardToEmail: account.Email, IsActive: true,
		})
	}
	service, err := hmesync.New(env.store, env.cipher, client, batchDeletionTestLocker{})
	if err != nil {
		t.Fatalf("create fixture Apple service: %v", err)
	}
	env.server.SetHMESyncService(service)
	session := apple.Session{AppleID: "fixture-owner@example.com", DSID: "fixture-dsid", Region: apple.RegionGlobal, ValidatedAt: time.Now().UTC()}
	payload, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("encode fixture Apple session: %v", err)
	}
	ciphertext, err := env.cipher.EncryptAppleSession(string(payload))
	if err != nil {
		t.Fatalf("encrypt fixture Apple session: %v", err)
	}
	if _, err := env.store.UpsertAppleWebSession(ctx, domain.AppleWebSession{
		AccountID: account.ID, AppleID: session.AppleID, Region: hmesync.RegionGlobal,
		Ciphertext: ciphertext, Authenticated: true, LastValidatedAt: &session.ValidatedAt,
	}); err != nil {
		t.Fatalf("store fixture Apple session: %v", err)
	}
	return client
}

func TestManualAliasRegistrationRequiresAppleVerificationAtBothEntrypoints(t *testing.T) {
	for _, entrypoint := range []string{"admin", "external"} {
		for _, test := range []struct {
			name       string
			setup      func(*testing.T, *adminAPITestEnv, domain.Account, *manualRegistrationAppleClient)
			wantStatus int
			wantCode   string
		}{
			{name: "no Apple service", setup: func(_ *testing.T, env *adminAPITestEnv, _ domain.Account, _ *manualRegistrationAppleClient) {
				env.server.SetHMESyncService(nil)
			}, wantStatus: http.StatusServiceUnavailable, wantCode: "APPLE_REGISTRATION_UNAVAILABLE"},
			{name: "older Apple service", setup: func(_ *testing.T, env *adminAPITestEnv, _ domain.Account, _ *manualRegistrationAppleClient) {
				env.server.SetHMESyncService(&fakeHMESyncService{})
			}, wantStatus: http.StatusServiceUnavailable, wantCode: "APPLE_REGISTRATION_UNAVAILABLE"},
			{name: "no Apple session", setup: func(t *testing.T, env *adminAPITestEnv, account domain.Account, _ *manualRegistrationAppleClient) {
				if err := env.store.DeleteAppleWebSession(context.Background(), account.ID); err != nil {
					t.Fatal(err)
				}
			}, wantStatus: http.StatusConflict, wantCode: hmesync.CodeLoginRequired},
			{name: "expired Apple session", setup: func(_ *testing.T, _ *adminAPITestEnv, _ domain.Account, client *manualRegistrationAppleClient) {
				client.validateErr = apple.ErrInvalidSession
			}, wantStatus: http.StatusConflict, wantCode: hmesync.CodeSessionExpired},
			{name: "address absent at Apple", setup: func(_ *testing.T, _ *adminAPITestEnv, _ domain.Account, client *manualRegistrationAppleClient) {
				client.directory.Aliases = nil
			}, wantStatus: http.StatusUnprocessableEntity, wantCode: hmesync.CodeAliasNotFound},
			{name: "address inactive at Apple", setup: func(_ *testing.T, _ *adminAPITestEnv, _ domain.Account, client *manualRegistrationAppleClient) {
				client.directory.Aliases[0].IsActive = false
			}, wantStatus: http.StatusConflict, wantCode: hmesync.CodeAliasInactive},
			{name: "wrong forwarding destination", setup: func(_ *testing.T, _ *adminAPITestEnv, _ domain.Account, client *manualRegistrationAppleClient) {
				client.directory.Aliases[0].ForwardToEmail = "other@example.com"
			}, wantStatus: http.StatusConflict, wantCode: hmesync.CodeAccountMismatch},
			{name: "Apple directory unavailable", setup: func(_ *testing.T, _ *adminAPITestEnv, _ domain.Account, client *manualRegistrationAppleClient) {
				client.listErr = errors.New("fixture transport failure")
			}, wantStatus: http.StatusBadGateway, wantCode: hmesync.CodeUpstreamError},
		} {
			t.Run(entrypoint+"/"+test.name, func(t *testing.T) {
				env := newAdminAPITestEnv(t)
				account := adminAPITestCreateAccount(t, env, "verify-registration@icloud.com")
				client := connectManualAliasDirectory(t, env, account.ID, "verified-alias@icloud.com")
				cursor := seedManualRegistrationCursor(t, env, account)
				account, err := env.store.GetAccount(context.Background(), account.ID)
				if err != nil {
					t.Fatal(err)
				}
				test.setup(t, env, account, client)
				response := requestManualRegistration(t, env, entrypoint, account, "verified-alias@icloud.com")
				if response.Code != test.wantStatus || adminAPITestErrorCode(t, response) != test.wantCode {
					t.Fatalf("registration status = %d, body = %s", response.Code, response.Body.String())
				}
				aliases, err := env.store.ListAliasesByAccount(context.Background(), account.ID)
				if err != nil || len(aliases) != 0 {
					t.Fatalf("rejected registration published aliases: %#v, %v", aliases, err)
				}
				currentCursor, err := env.store.GetIMAPSyncState(context.Background(), account.ID)
				if err != nil || currentCursor != cursor {
					t.Fatalf("rejected registration reset IMAP cursor: %#v, %v", currentCursor, err)
				}
				currentAccount, err := env.store.GetAccount(context.Background(), account.ID)
				if err != nil || !currentAccount.UpdatedAt.Equal(account.UpdatedAt) {
					t.Fatalf("rejected registration invalidated in-progress synchronization: %v", err)
				}
			})
		}
	}
}

func TestManualAliasRegistrationPublishesOnlyVerifiedAddressAtBothEntrypoints(t *testing.T) {
	for _, entrypoint := range []string{"admin", "external"} {
		t.Run(entrypoint, func(t *testing.T) {
			env := newAdminAPITestEnv(t)
			account := adminAPITestCreateAccount(t, env, "verified-registration@icloud.com")
			client := connectManualAliasDirectory(t, env, account.ID, "verified-alias@icloud.com", "unrelated-alias@icloud.com")
			response := requestManualRegistration(t, env, entrypoint, account, "verified-alias@icloud.com")
			if response.Code != http.StatusCreated {
				t.Fatalf("registration status = %d, body = %s", response.Code, response.Body.String())
			}
			aliases, err := env.store.ListAliasesByAccount(context.Background(), account.ID)
			if err != nil || len(aliases) != 1 || aliases[0].Address != "verified-alias@icloud.com" || !aliases[0].Enabled {
				t.Fatalf("registration published aliases: %#v, %v", aliases, err)
			}
			credentials, err := env.cipher.DecryptAliasCredentials(aliases[0].ID, aliases[0].CredentialCiphertext)
			if err != nil || !strings.Contains(response.Body.String(), credentials.APIKey) || client.listCalls != 1 {
				t.Fatalf("verified registration credential/directory contract failed: err=%v calls=%d", err, client.listCalls)
			}
		})
	}
}

func TestManualAliasRegistrationCustomMailboxRemainsLocal(t *testing.T) {
	for _, entrypoint := range []string{"admin", "external"} {
		t.Run(entrypoint, func(t *testing.T) {
			env := newAdminAPITestEnv(t)
			account := adminAPITestCreateAccount(t, env, "custom-registration@example.com")
			account.MailboxType = domain.MailboxTypeCustom
			account.EmailSuffix = "example.com"
			var err error
			account, err = env.store.UpdateAccount(context.Background(), account)
			if err != nil {
				t.Fatal(err)
			}
			response := requestManualRegistration(t, env, entrypoint, account, "local-alias@example.com")
			if response.Code != http.StatusCreated {
				t.Fatalf("custom registration status = %d, body = %s", response.Code, response.Body.String())
			}
			if _, err := env.store.GetAliasByAddress(context.Background(), "local-alias@example.com"); err != nil {
				t.Fatalf("custom registration not persisted: %v", err)
			}
		})
	}
}

func TestManualAliasRegistrationCustomIdentityConflictIsValidationFailure(t *testing.T) {
	for _, address := range []string{"custom-registration@example.com", "forwarding-inbox@example.net"} {
		t.Run(address, func(t *testing.T) {
			env := newAdminAPITestEnv(t)
			account := adminAPITestCreateAccount(t, env, "custom-registration@example.com")
			account.MailboxType = domain.MailboxTypeCustom
			account.EmailSuffix = "example.com"
			account.IMAPUsername = "forwarding-inbox@example.net"
			var err error
			account, err = env.store.UpdateAccount(context.Background(), account)
			if err != nil {
				t.Fatal(err)
			}
			response := requestManualRegistration(t, env, "admin", account, address)
			if response.Code != http.StatusBadRequest || adminAPITestErrorCode(t, response) != "VALIDATION_FAILED" {
				t.Fatalf("custom identity conflict status = %d, body = %s", response.Code, response.Body.String())
			}
			aliases, err := env.store.ListAliasesByAccount(context.Background(), account.ID)
			if err != nil || len(aliases) != 0 {
				t.Fatalf("conflicting alias persisted: %#v, %v", aliases, err)
			}
		})
	}
}

func seedManualRegistrationCursor(t *testing.T, env *adminAPITestEnv, account domain.Account) domain.IMAPSyncState {
	t.Helper()
	ctx := context.Background()
	if err := env.store.ApplyMailboxSync(ctx, account.ID, account.UpdatedAt, nil, domain.MailboxSyncResult{
		State: domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 91, LastUID: 1200},
		Reset: true,
	}, time.Now().UTC()); err != nil {
		t.Fatalf("seed mailbox cursor: %v", err)
	}
	cursor, err := env.store.GetIMAPSyncState(ctx, account.ID)
	if err != nil {
		t.Fatalf("read seeded mailbox cursor: %v", err)
	}
	return cursor
}

func requestManualRegistration(t *testing.T, env *adminAPITestEnv, entrypoint string, account domain.Account, address string) *httptest.ResponseRecorder {
	t.Helper()
	if entrypoint == "admin" {
		cookie, csrf, _ := env.createSession(t, "manual-registration-admin", "unused-password")
		return env.request(t, http.MethodPost, fmt.Sprintf("/admin/api/v1/accounts/%d/aliases", account.ID),
			adminAPITestJSON(t, map[string]string{"address": address}), "application/json", []*http.Cookie{cookie}, csrf)
	}
	env.server.oauthTokenConfigured = true
	env.server.oauthTokenHash = secure.HashToken("registration-fixture-token")
	router, err := env.server.Router()
	if err != nil {
		t.Fatalf("build registration router: %v", err)
	}
	form := url.Values{externalAliasAddressField: {address}, externalAliasAccountField: {account.Email}}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/aliases", strings.NewReader(form.Encode()))
	request.Header.Set("Authorization", "Bearer registration-fixture-token")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

var _ HMEAliasRegistrationService = (*hmesync.Service)(nil)
var _ hmesync.AliasRegistrationRepository = (*store.Store)(nil)
