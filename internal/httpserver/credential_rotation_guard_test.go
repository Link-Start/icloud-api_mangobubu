package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"

	"icloud-api/internal/secure"
)

func TestAliasCredentialRotationGuardLinearizesPublicCredentialRequests(t *testing.T) {
	env := newAdminAPITestEnv(t)
	account := adminAPITestCreateAccount(t, env, "public-rotation-guard@icloud.com")
	alias, _ := createV2AliasFixture(
		t, env, account.ID, "public-rotation-guard-alias@icloud.com",
	)
	oldToken, err := env.cipher.RecentMailToken(alias.ID, alias.APIKeyHash)
	if err != nil {
		t.Fatal(err)
	}

	handlerEntered := make(chan struct{})
	releaseHandler := make(chan struct{})
	router := gin.New()
	router.Use(env.server.requestContext())
	router.GET(
		"/api/v1/mail/recent",
		env.server.credentialRotationReadGuard(),
		env.server.apiKeyQueryAuth(),
		func(c *gin.Context) {
			close(handlerEntered)
			<-releaseHandler
			c.Status(http.StatusNoContent)
		},
	)

	responseDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responseDone <- serveV2Request(
			router,
			http.MethodGet,
			"/api/v1/mail/recent?api_key="+url.QueryEscape(oldToken),
			"",
			nil,
		)
	}()
	<-handlerEntered

	if env.server.credentialRotationMu.TryLock() {
		env.server.credentialRotationMu.Unlock()
		t.Fatal("credential rotation writer crossed an authenticated public response")
	}
	rotationDone := make(chan error, 1)
	go func() {
		env.server.credentialRotationMu.Lock()
		defer env.server.credentialRotationMu.Unlock()
		_, rotateErr := env.store.RotateAllAliasCredentials(context.Background())
		rotationDone <- rotateErr
	}()

	close(releaseHandler)
	response := <-responseDone
	if response.Code != http.StatusNoContent {
		t.Fatalf("in-flight public response = %d %s", response.Code, response.Body.String())
	}
	if err := <-rotationDone; err != nil {
		t.Fatalf("rotate all credentials: %v", err)
	}

	fullRouter, err := env.server.Router()
	if err != nil {
		t.Fatal(err)
	}
	rejected := serveV2Request(
		fullRouter,
		http.MethodGet,
		"/api/v1/mail/recent?api_key="+url.QueryEscape(oldToken),
		"",
		nil,
	)
	if rejected.Code != http.StatusUnauthorized {
		t.Fatalf("old public token after rotation = %d %s", rejected.Code, rejected.Body.String())
	}
}

func TestExternalAliasRoutesWaitForCredentialRotation(t *testing.T) {
	for _, routePath := range []string{"/api/v1/aliases", "/api/v1/aliases/"} {
		t.Run(routePath, func(t *testing.T) {
			env := newAdminAPITestEnv(t)
			const oauthToken = "external-rotation-guard-token"
			env.server.oauthTokenConfigured = true
			env.server.oauthTokenHash = secure.HashToken(oauthToken)
			account := adminAPITestCreateAccount(
				t, env, "external-rotation-guard@icloud.com",
			)

			guardEntered := make(chan struct{})
			env.server.beforeCredentialRotationReadLock = func() {
				close(guardEntered)
			}
			router, err := env.server.Router()
			if err != nil {
				t.Fatal(err)
			}

			env.server.credentialRotationMu.Lock()
			writerLocked := true
			defer func() {
				if writerLocked {
					env.server.credentialRotationMu.Unlock()
				}
			}()

			query := url.Values{
				externalAliasAddressField: {"external-rotation-guard-alias@icloud.com"},
				externalAliasAccountField: {account.Email},
			}.Encode()
			responseDone := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				request := httptest.NewRequest(
					http.MethodPost, routePath+"?"+query, nil,
				)
				request.Header.Set("Authorization", "Bearer "+oauthToken)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				responseDone <- response
			}()

			<-guardEntered
			assertExternalAliasCount(t, env, 0)
			env.server.credentialRotationMu.Unlock()
			writerLocked = false

			response := <-responseDone
			if response.Code != http.StatusCreated {
				t.Fatalf(
					"external alias response = %d %s",
					response.Code, response.Body.String(),
				)
			}
			assertExternalAliasCount(t, env, 1)
		})
	}
}

func TestAdminAPICredentialRotationGuardRejectsSessionRevokedWhileWaiting(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookieValue, _, admin := env.createSession(
		t, "rotation-guard-waiter", "unused-password",
	)
	passedInitialAuth := make(chan struct{})
	handlerCalled := make(chan struct{}, 1)
	router := gin.New()
	router.Use(env.server.requestContext())
	protected := router.Group("/guard")
	protected.Use(
		env.server.adminAPIAuth(),
		env.server.adminAPICSRF(),
		func(c *gin.Context) {
			close(passedInitialAuth)
			c.Next()
		},
		env.server.adminAPICredentialRotationReadGuard(),
	)
	protected.GET("/resource", func(c *gin.Context) {
		handlerCalled <- struct{}{}
		c.Status(http.StatusNoContent)
	})

	env.server.credentialRotationMu.Lock()
	responseDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responseDone <- serveCredentialGuardRequest(
			router, "/guard/resource", sessionCookieValue,
		)
	}()
	<-passedInitialAuth
	if err := env.store.ChangeAdminPasswordAndRevokeSessions(
		t.Context(), admin.ID, admin.PasswordVersion, admin.PasswordHash,
	); err != nil {
		env.server.credentialRotationMu.Unlock()
		t.Fatal(err)
	}
	env.server.credentialRotationMu.Unlock()

	response := <-responseDone
	if response.Code != http.StatusUnauthorized ||
		adminAPITestErrorCode(t, response) != "SESSION_EXPIRED" {
		t.Fatalf("waiting old session response = %d %s", response.Code, response.Body.String())
	}
	select {
	case <-handlerCalled:
		t.Fatal("revoked waiting session entered the protected handler")
	default:
	}
}

func TestAdminAPICredentialRotationGuardLetsInFlightResponseFinishBeforeWriter(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookieValue, _, _ := env.createSession(
		t, "rotation-guard-inflight", "unused-password",
	)
	handlerEntered := make(chan struct{})
	releaseHandler := make(chan struct{})
	router := gin.New()
	router.Use(env.server.requestContext())
	protected := router.Group("/guard")
	protected.Use(
		env.server.adminAPIAuth(),
		env.server.adminAPICSRF(),
		env.server.adminAPICredentialRotationReadGuard(),
	)
	protected.GET("/resource", func(c *gin.Context) {
		close(handlerEntered)
		<-releaseHandler
		c.Status(http.StatusNoContent)
	})

	responseDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responseDone <- serveCredentialGuardRequest(
			router, "/guard/resource", sessionCookieValue,
		)
	}()
	<-handlerEntered

	if env.server.credentialRotationMu.TryLock() {
		env.server.credentialRotationMu.Unlock()
		t.Fatal("credential rotation writer crossed an in-flight protected response")
	}
	writerAcquired := make(chan struct{})
	go func() {
		env.server.credentialRotationMu.Lock()
		close(writerAcquired)
		env.server.credentialRotationMu.Unlock()
	}()

	close(releaseHandler)
	response := <-responseDone
	if response.Code != http.StatusNoContent {
		t.Fatalf("in-flight protected response = %d %s", response.Code, response.Body.String())
	}
	<-writerAcquired
}

func serveCredentialGuardRequest(
	router http.Handler,
	target string,
	cookie *http.Cookie,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "http://admin.example.test"+target, nil)
	request.Header.Set("Origin", "http://admin.example.test")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}
