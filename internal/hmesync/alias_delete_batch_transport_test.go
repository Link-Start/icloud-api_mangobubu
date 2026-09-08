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
	"time"

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

func TestAliasDeletionRecoveryRealClientPersistentThrottleDefers416Items(t *testing.T) {
	service, repo, ids, _ := newAliasDeletionBatchFixture(t, 416, &fakeAppleClient{}, &fakeLocker{})
	clock := &aliasDeletionMockClock{value: service.now()}
	WithClock(clock.now)(service)
	WithAliasDeletionWaiter(clock.wait)(service)
	requests := 0
	var callTimes []time.Time
	client, err := apple.NewClient(apple.Config{Transport: aliasDeletionRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/setup/ws/1/validate" {
			t.Errorf("blocked validation started %s", request.URL.Path)
		}
		assertStoredAppleSessionToken(t, service, repo, 3, fmt.Sprintf("wire-%d", requests))
		requests++
		callTimes = append(callTimes, clock.now())
		headers := make(http.Header)
		headers.Set("Content-Type", "application/json")
		headers.Set("Retry-After", "90")
		headers.Set("X-Apple-Session-Token", fmt.Sprintf("wire-%d", requests))
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: headers,
			Body: io.NopCloser(strings.NewReader(`{"errorCode":"RATE_LIMITED"}`)), Request: request}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	service.client = client
	storeSession(t, service, repo, 3, apple.Session{
		AppleID: "owner@example.com", Region: apple.RegionGlobal, DSID: "42", SessionToken: "wire-0",
	})
	reports, starts, clears := 0, 0, 0
	ctx := WithAliasDeletionProgress(context.Background(), func(out AliasDeletionOutcome) { reports++ })
	ctx = WithAliasDeletionRecovery(ctx, func(state AliasDeletionWait) {
		if reports != 0 || state.Operation != "validate" || state.HTTPStatus != 429 || state.AliasID != ids[0] {
			t.Error("real client wait finalized items early or lost throttle diagnostics")
		}
		if state.Waiting {
			starts++
		} else {
			clears++
		}
	})
	out, err := service.DeleteAliases(ctx, ids)
	if err != nil || len(out) != 416 || reports != 416 || requests != 4 || starts != 3 || clears != 3 {
		t.Fatalf("real client exhaustion: err=%v items=%d reports=%d requests=%d starts=%d clears=%d", err, len(out), reports, requests, starts, clears)
	}
	for i, outcome := range out {
		want := CodeBatchDeferred
		if i == 0 {
			want = CodeRateLimited
		}
		if Code(outcome.Err) != want || !repo.hasAlias(outcome.AliasID) {
			t.Errorf("item %d=%v want=%s and preserved local record", i, outcome.Err, want)
		}
	}
	for i, delay := range []time.Duration{90 * time.Second, 2 * time.Minute, 4 * time.Minute} {
		if gap := callTimes[i+1].Sub(callTimes[i]); gap != delay {
			t.Errorf("wire retry %d delay=%s want=%s", i+1, gap, delay)
		}
	}
	assertStoredAppleSessionToken(t, service, repo, 3, "wire-4")
}

type aliasDeletionTimeoutBody struct{}

func (aliasDeletionTimeoutBody) Read([]byte) (int, error) { return 0, context.DeadlineExceeded }
func (aliasDeletionTimeoutBody) Close() error             { return nil }

func TestAliasDeletionRecoveryRealClient429BodyTimeoutHonorsRetryAfter(t *testing.T) {
	for _, operation := range []string{"validate", "list", "deactivate", "delete"} {
		for _, scenario := range []string{"recovered", "job cancelled", "exhausted"} {
			t.Run(operation+"/"+scenario, func(t *testing.T) {
				service, repo, ids, directory := newAliasDeletionBatchFixture(t, 2, &fakeAppleClient{}, &fakeLocker{})
				clock := &aliasDeletionMockClock{value: service.now()}
				WithClock(clock.now)(service)
				WithAliasDeletionWaiter(clock.wait)(service)
				base, cancel := context.WithCancel(context.Background())
				defer cancel()
				requests, limited, deactivates, deletes := 0, 0, 0, 0
				var failedAt time.Time
				var calls []aliasDeletionRecoveryCall
				var waits []AliasDeletionWait
				reports := 0
				waiting := false
				client, err := apple.NewClient(apple.Config{Transport: aliasDeletionRoundTripFunc(func(request *http.Request) (*http.Response, error) {
					op := map[string]string{"/setup/ws/1/validate": "validate", "/v2/hme/list": "list",
						"/v1/hme/deactivate": "deactivate", "/v1/hme/delete": "delete"}[request.URL.Path]
					if op == "" {
						return nil, errors.New("unexpected intercepted request")
					}
					if !failedAt.IsZero() && (clock.now().Sub(failedAt) < 90*time.Second || waiting) {
						t.Error("request bypassed Retry-After or started during waiting")
					}
					assertStoredAppleSessionToken(t, service, repo, 3, fmt.Sprintf("wire-%d", requests))
					requests++
					calls = append(calls, aliasDeletionRecoveryCall{operation: op, at: clock.now()})
					body := `{"success":true}`
					switch op {
					case "validate":
						body = `{"dsInfo":{"dsid":"42","primaryEmail":"owner@example.com","hsaVersion":2},"hsaTrustedBrowser":true,"webservices":{"premiummailsettings":{"url":"https://p01-maildomainws.icloud.com"}}}`
					case "list":
						encoded, err := json.Marshal(struct {
							Success bool             `json:"success"`
							Result  apple.ListResult `json:"result"`
						}{true, directory})
						if err != nil {
							return nil, err
						}
						body = string(encoded)
					case "deactivate", "delete":
						var payload map[string]string
						if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
							return nil, err
						}
						found := false
						for i, remote := range directory.Aliases {
							if remote.AnonymousID != payload["anonymousId"] {
								continue
							}
							found = true
							if op == "deactivate" {
								deactivates++
								if !remote.IsActive {
									t.Error("replayed already-successful deactivation")
								}
							} else {
								deletes++
								if remote.IsActive {
									t.Error("deleted an active alias")
								}
							}
							// In exhausted tests the failing mutation never takes effect.
							if scenario != "exhausted" || op != operation {
								if op == "deactivate" {
									directory.Aliases[i].IsActive = false
								} else {
									directory.Aliases = append(directory.Aliases[:i], directory.Aliases[i+1:]...)
								}
							}
							break
						}
						if !found {
							t.Error("replayed already-successful deletion")
						}
					}
					headers := make(http.Header)
					headers.Set("Content-Type", "application/json")
					headers.Set("X-Apple-Session-Token", fmt.Sprintf("wire-%d", requests))
					response := &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: request}
					if op == operation && (limited == 0 || scenario == "exhausted") {
						limited++
						failedAt = clock.now()
						response.StatusCode, response.Body = http.StatusTooManyRequests, aliasDeletionTimeoutBody{}
						response.Header.Set("Retry-After", "90")
						if scenario == "job cancelled" {
							cancel()
						}
					}
					return response, nil
				})})
				if err != nil {
					t.Fatal(err)
				}
				service.client = client
				storeSession(t, service, repo, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal, DSID: "42", SessionToken: "wire-0"})
				ctx := WithAliasDeletionRecovery(base, func(state AliasDeletionWait) {
					waiting = state.Waiting
					waits = append(waits, state)
					if reports != 0 || state.HTTPStatus != 429 || state.Operation != operation {
						t.Error("body timeout lost wait classification or finalized item early")
					}
				})
				ctx = WithAliasDeletionProgress(ctx, func(out AliasDeletionOutcome) {
					if waiting {
						t.Error("reported terminal item while waiting")
					}
					reports++
				})
				out, err := service.DeleteAliases(ctx, ids)
				if err != nil || len(out) != 2 || reports != 2 {
					t.Fatalf("429 body timeout out=%v err=%v reports=%d", out, err, reports)
				}
				switch scenario {
				case "recovered":
					if out[0].Err != nil || out[1].Err != nil || len(waits) != 2 || limited != 1 || deactivates != 2 || deletes != 2 || repo.hasAlias(ids[0]) || repo.hasAlias(ids[1]) {
						t.Errorf("429 body timeout did not recover without replay: out=%v waits=%v mutation=%d/%d", out, waits, deactivates, deletes)
					}
				case "job cancelled":
					if len(waits) != 0 || limited != 1 || calls[len(calls)-1].operation != operation || !errors.Is(out[0].Err, context.Canceled) || !errors.Is(out[1].Err, context.Canceled) {
						t.Errorf("cancelled job retried/waited: out=%v waits=%v calls=%v", out, waits, calls)
					}
				case "exhausted":
					if len(waits) != 6 || limited != 4 || requests > 10 || Code(out[0].Err) != CodeRateLimited || Code(out[1].Err) != CodeBatchDeferred || !errors.Is(out[0].Err, context.DeadlineExceeded) {
						t.Errorf("body timeout exhaustion misclassified: out=%v waits=%v calls=%v", out, waits, calls)
					}
				}
				assertStoredAppleSessionToken(t, service, repo, 3, fmt.Sprintf("wire-%d", requests))
			})
		}
	}
}
