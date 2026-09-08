package hmesync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"icloud-api/internal/apple"
)

type aliasDeletionRoundTripFunc func(*http.Request) (*http.Response, error)

func (f aliasDeletionRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestDeleteAliasesRealClientUses2NPlus2InterceptedRequests(t *testing.T) {
	const count = 8
	service, repo, ids, full := newAliasDeletionBatchFixture(t, count, &fakeAppleClient{}, &fakeLocker{})
	active := make(map[string]bool)
	for _, remote := range full.Aliases {
		active[remote.AnonymousID] = true
	}
	requests, validates, lists := 0, 0, 0
	// This transport handles every request in memory and never delegates to a
	// network transport, even though the client validates its Apple-host URLs.
	client, err := apple.NewClient(apple.Config{Transport: aliasDeletionRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		cookie, err := request.Cookie("batch-token")
		if err != nil || cookie.Value != fmt.Sprintf("wire-%d", requests) {
			t.Error("real client did not roll cookies forward between requests")
		}
		assertStoredAppleSessionToken(t, service, repo, 3, fmt.Sprintf("wire-%d", requests))
		requests++
		body := `{"success":true}`
		switch request.URL.Path {
		case "/setup/ws/1/validate":
			validates++
			body = `{"dsInfo":{"dsid":"42","primaryEmail":"owner@example.com","hsaVersion":2},"hsaTrustedBrowser":true,"webservices":{"premiummailsettings":{"url":"https://p01-maildomainws.icloud.com"}}}`
		case "/v2/hme/list":
			lists++
			encoded, err := json.Marshal(struct {
				Success bool             `json:"success"`
				Result  apple.ListResult `json:"result"`
			}{true, full})
			if err != nil {
				return nil, err
			}
			body = string(encoded)
		case "/v1/hme/deactivate", "/v1/hme/delete":
			var payload map[string]string
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				return nil, err
			}
			id := payload["anonymousId"]
			isActive, present := active[id]
			if !present {
				return nil, errors.New("fixture mutation targeted an unknown remote ID")
			}
			if request.URL.Path == "/v1/hme/deactivate" {
				if !isActive {
					return nil, errors.New("fixture alias was deactivated twice")
				}
				active[id] = false
			} else {
				if isActive {
					return nil, errors.New("fixture alias was deleted before deactivation")
				}
				delete(active, id)
			}
		default:
			return nil, errors.New("unexpected intercepted request")
		}
		headers := make(http.Header)
		headers.Set("Content-Type", "application/json")
		headers.Set("X-Apple-Session-Token", fmt.Sprintf("wire-%d", requests))
		headers.Set("Set-Cookie", fmt.Sprintf("batch-token=wire-%d; Domain=.icloud.com; Path=/; Secure; HttpOnly", requests))
		return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	service.client = client
	storeSession(t, service, repo, 3, apple.Session{
		AppleID: "owner@example.com", Region: apple.RegionGlobal, DSID: "42", SessionToken: "wire-0",
		Cookies: []apple.PersistentCookie{{Name: "batch-token", Value: "wire-0", Domain: ".icloud.com", Path: "/", Secure: true}},
	})
	repo.deleteAliasFn = func(_ context.Context, id int64) error {
		if _, present := active[fmt.Sprintf("remote-%d", id)]; present {
			t.Error("local deletion preceded confirmed permanent deletion")
		}
		return nil
	}
	outcomes, err := service.DeleteAliases(context.Background(), ids)
	if err != nil || len(outcomes) != count {
		t.Fatalf("batch error=%v items=%d", err, len(outcomes))
	}
	for _, outcome := range outcomes {
		if outcome.Err != nil {
			t.Errorf("item %d failed: %s", outcome.AliasID, Code(outcome.Err))
		}
	}
	if requests != 2*count+2 || validates != 1 || lists != 1 || len(active) != 0 {
		t.Errorf("intercepted requests=%d want=%d validate=%d list=%d remaining=%d", requests, 2*count+2, validates, lists, len(active))
	}
	assertStoredAppleSessionToken(t, service, repo, 3, fmt.Sprintf("wire-%d", requests))
}
