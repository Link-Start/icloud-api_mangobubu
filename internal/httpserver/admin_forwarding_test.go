package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"icloud-api/internal/hmesync"
)

type fakeForwardingService struct {
	fakeHMESyncService
	settings  hmesync.ForwardingSettings
	accountID int64
	updates   int
	err       error
}

func (f *fakeForwardingService) GetForwardingSettings(_ context.Context, id int64) (hmesync.ForwardingSettings, error) {
	f.accountID = id
	return f.settings, f.err
}

func (f *fakeForwardingService) UpdateForwardingSettings(_ context.Context, id int64, email string) (hmesync.ForwardingSettings, error) {
	f.accountID = id
	f.updates++
	f.settings.SelectedForwardTo = email
	return f.settings, f.err
}

func TestAdminAPIForwardingSettings(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, _ := env.createSession(t, "forwarding-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "primary@icloud.com")
	service := &fakeForwardingService{settings: hmesync.ForwardingSettings{
		SelectedForwardTo: account.Email,
		ForwardToEmails:   []string{account.Email, "other@example.com"},
	}}
	env.server.SetHMESyncService(service)
	path := fmt.Sprintf("/admin/api/v1/accounts/%d/forwarding", account.ID)
	cookies := []*http.Cookie{cookie}
	body := []byte(`{"forward_to_email":" OTHER@EXAMPLE.COM "}`)

	for _, method := range []string{http.MethodGet, http.MethodPut} {
		response := env.request(t, method, path, body, "application/json", nil, "")
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthorized %s: %d", method, response.Code)
		}
	}
	response := env.request(t, http.MethodPut, path, body, "application/json", cookies, "")
	if response.Code != http.StatusForbidden || service.updates != 0 {
		t.Fatalf("missing CSRF: %d", response.Code)
	}
	response = env.request(t, http.MethodGet, path, nil, "", cookies, "")
	var payload struct {
		Data adminAPIForwardingSettingsDTO `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || len(payload.Data.ForwardToEmails) != 2 || payload.Data.SelectedForwardTo != account.Email || service.accountID != account.ID {
		t.Fatalf("read settings: %d %s", response.Code, response.Body.String())
	}
	response = env.request(t, http.MethodPut, path, body, "application/json", cookies, csrf)
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || payload.Data.SelectedForwardTo != "other@example.com" || service.updates != 1 {
		t.Fatalf("update settings: %d %s", response.Code, response.Body.String())
	}
	for _, invalid := range []string{`{}`, `{"forward_to_email":"invalid"}`, `{"forward_to_email":["a@example.com","b@example.com"]}`, `{"forward_to_email":"a@example.com","unexpected":true}`} {
		response = env.request(t, http.MethodPut, path, []byte(invalid), "application/json", cookies, csrf)
		if response.Code != http.StatusBadRequest || service.updates != 1 {
			t.Fatalf("invalid input: %d %s", response.Code, response.Body.String())
		}
	}
	for _, tt := range []struct {
		err    error
		status int
		code   string
	}{
		{hmesync.ErrSessionExpired, http.StatusConflict, hmesync.CodeSessionExpired},
		{hmesync.ErrForwardingTargetInvalid, http.StatusUnprocessableEntity, hmesync.CodeForwardingTargetInvalid},
		{hmesync.ErrForwardingNotConfirmed, http.StatusConflict, hmesync.CodeForwardingNotConfirmed},
		{hmesync.ErrUpstream, http.StatusBadGateway, hmesync.CodeUpstreamError},
	} {
		service.err = tt.err
		response = env.request(t, http.MethodPut, path, body, "application/json", cookies, csrf)
		if response.Code != tt.status || adminAPITestErrorCode(t, response) != tt.code {
			t.Fatalf("error mapping %v: %d %s", tt.err, response.Code, response.Body.String())
		}
	}
}
