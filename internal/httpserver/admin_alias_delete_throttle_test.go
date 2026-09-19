package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/hmesync"
)

type aliasThrottleDeadlineBody struct{}

func (aliasThrottleDeadlineBody) Read([]byte) (int, error) { return 0, context.DeadlineExceeded }
func (aliasThrottleDeadlineBody) Close() error             { return nil }

// Actual Apple throttles become a durable timer; the worker never sleeps while
// holding the account or credential rotation locks.
func TestAliasDeletionJobWaitsOnFirstAppleThrottleWithoutFailingRemainingItems(t *testing.T) {
	for _, scenario := range []struct{ stop, bodyTimeout bool }{{}, {stop: true}, {bodyTimeout: true}, {stop: true, bodyTimeout: true}} {
		t.Run(fmt.Sprintf("stop=%t/body-timeout=%t", scenario.stop, scenario.bodyTimeout), func(t *testing.T) {
			env := newAdminAPITestEnv(t)
			cookie, csrf, admin := env.createSession(t, "throttle-wait-admin", "unused-password")
			account := adminAPITestCreateAccount(t, env, "throttle-owner@icloud.com")
			first := adminAPITestCreateDeleteAlias(t, env, account.ID, "throttle-first@icloud.com")
			second := adminAPITestCreateDeleteAlias(t, env, account.ID, "throttle-second@icloud.com")
			directory := apple.ListResult{SelectedForwardTo: account.Email, ForwardToEmails: []string{account.Email}}
			for _, alias := range []domain.Alias{first, second} {
				directory.Aliases = append(directory.Aliases, apple.Alias{HME: alias.Address, AnonymousID: fmt.Sprint(alias.ID), ForwardToEmail: account.Email, IsActive: true})
			}
			directoryJSON := adminAPITestJSON(t, map[string]any{"success": true, "result": directory})
			var requests, validates atomic.Int32
			client, err := apple.NewClient(apple.Config{Transport: batchDeletionTestTransport(func(request *http.Request) (*http.Response, error) {
				requests.Add(1)
				status := http.StatusOK
				headers := http.Header{"Content-Type": []string{"application/json"}}
				body := "{\"success\":true}"
				switch request.URL.Path {
				case "/setup/ws/1/validate":
					if validates.Add(1) == 1 {
						status = http.StatusTooManyRequests
						headers.Set("Retry-After", "90")
						body = "{\"success\":false}"
					} else {
						body = "{\"dsInfo\":{\"dsid\":\"42\",\"primaryEmail\":\"throttle-apple@example.com\",\"hsaVersion\":2},\"hsaTrustedBrowser\":true,\"webservices\":{\"premiummailsettings\":{\"url\":\"https://p01-maildomainws.icloud.com\"}}}"
					}
				case "/v2/hme/list":
					body = string(directoryJSON)
				case "/v1/hme/deactivate", "/v1/hme/delete":
				default:
					return nil, errors.New("unexpected fixture request")
				}
				var reader io.ReadCloser = io.NopCloser(strings.NewReader(body))
				if scenario.bodyTimeout && status == http.StatusTooManyRequests {
					reader = aliasThrottleDeadlineBody{}
				}
				return &http.Response{StatusCode: status, Header: headers, Body: reader, Request: request}, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			clockBase := time.Now().UTC()
			var clockOffset atomic.Int64
			clock := func() time.Time { return clockBase.Add(time.Duration(clockOffset.Load())) }
			env.server.now = clock
			service, err := hmesync.New(env.store, env.cipher, client, batchDeletionTestLocker{}, hmesync.WithClock(clock), hmesync.WithAliasDeletionWaiter(func(ctx context.Context, delay time.Duration) error {
				if delay >= time.Minute {
					return errors.New("worker slept through persistent cooldown")
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				clockOffset.Add(int64(delay))
				return nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			session := apple.Session{AppleID: "throttle-apple@example.com", Region: apple.RegionGlobal, DSID: "42", SessionToken: "fixture-token", ValidatedAt: clockBase}
			encoded, err := json.Marshal(session)
			if err != nil {
				t.Fatal(err)
			}
			ciphertext, err := env.cipher.EncryptAppleSession(string(encoded))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := env.store.UpsertAppleWebSession(context.Background(), domain.AppleWebSession{AccountID: account.ID, AppleID: session.AppleID, Region: "global", Authenticated: true, Ciphertext: ciphertext, LastValidatedAt: &session.ValidatedAt}); err != nil {
				t.Fatal(err)
			}
			env.server.SetHMESyncService(service)
			stop, stopped := startTestAliasDeletionJobs(t, env)
			submitTestDeletionJob(t, env, cookie, csrf, testAliasDeletionJobID, first.ID, second.ID)
			job := waitTestAliasDeletionState(t, env, admin.ID, testAliasDeletionJobID, func(job domain.AliasDeletionJob) bool { return job.Items[0].State == domain.AliasDeletionWorkWaiting })
			state := env.server.adminAPIAliasDeletionJobSnapshot(job)
			if state.Processed != 0 || state.Pending != 2 || state.Failed != 0 || len(state.Results) != 0 || len(state.Waits) != 1 || len(state.Accounts) != 1 || state.Accounts[0].Status != "waiting" {
				t.Fatalf("throttle became failure: %#v", state)
			}
			wait := state.Waits[0]
			if wait.AccountID != account.ID || wait.AliasID != first.ID || wait.Operation != "validate" || wait.HTTPStatus != 429 || wait.Reason != "upstream_rate_limit" || requests.Load() != 1 {
				t.Fatalf("wait=%#v requests=%d", wait, requests.Load())
			}
			retryAt, err := time.Parse(time.RFC3339Nano, wait.RetryAt)
			if err != nil || retryAt.Before(clockBase.Add(90*time.Second)) {
				t.Fatalf("retry_at=%s err=%v", wait.RetryAt, err)
			}
			if scenario.stop {
				stop()
				waitTestAliasDeletionSignal(t, stopped)
				persisted, err := env.store.GetAliasDeletionJob(context.Background(), testAliasDeletionJobID, admin.ID)
				if err != nil || persisted.Items[0].Done || persisted.Status == domain.AliasDeletionJobInterrupted || requests.Load() != 1 {
					t.Fatalf("shutdown lost waiting queue: %#v err=%v", persisted, err)
				}
			} else {
				clockOffset.Store(int64(retryAt.Sub(clockBase) + time.Second))
				env.server.wakeAliasDeletionQueue()
				final := adminAPIAliasDeletionJobFromRecord(waitTestAliasDeletionJob(t, env, admin.ID, testAliasDeletionJobID))
				if final.Deleted != 2 || final.Failed != 0 || len(final.Waits) != 0 {
					t.Fatalf("cooldown recovery=%#v", final)
				}
			}
		})
	}
}

func TestAliasDeletionWaitSnapshotUsesDurableStateAndSanitizedDiagnostics(t *testing.T) {
	job := domain.AliasDeletionJob{ID: testAliasDeletionJobID, AdminID: 1, Status: domain.AliasDeletionJobRunning, Items: []domain.AliasDeletionJobItem{
		{ID: 9, AccountID: 3, Address: "pending@icloud.com", State: domain.AliasDeletionWorkWaiting, RetryAt: time.Now().Add(time.Hour), WaitReason: "quota", Used: 200, Limit: 200, Operation: "secret-token-url", HTTPStatus: -1, ServiceCode: "secret=cookie-value"},
	}}
	state := adminAPIAliasDeletionJobFromRecord(job)
	if state.Processed != 0 || state.Failed != 0 || state.Pending != 1 || len(state.Waits) != 1 {
		t.Fatalf("waiting counted as completed=%#v", state)
	}
	wait := state.Waits[0]
	if wait.Operation != "" || wait.HTTPStatus != 0 || wait.ServiceCode != "" || wait.Used != 200 || wait.Limit != 200 {
		t.Fatalf("unsafe diagnostics=%#v", wait)
	}
	job.Items[0].Done = true
	job.Items[0].State = domain.AliasDeletionWorkCancelled
	state = adminAPIAliasDeletionJobFromRecord(job)
	if state.Cancelled != 1 || state.Processed != 1 || state.Failed != 0 || state.Pending != 0 || len(state.Waits) != 0 {
		t.Fatalf("cancel counted as failure=%#v", state)
	}
}

func TestAliasDeletionQuotaErrorsExposeRetryTime(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, _ := env.createSession(t, "quota-api-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "quota-api@icloud.com")
	alias := adminAPITestCreateDeleteAlias(t, env, account.ID, "quota-api-alias@icloud.com")
	retryAt := time.Now().UTC().Add(time.Hour)
	wait := &hmesync.AliasDeletionWaitError{Reason: "quota", RetryAt: retryAt, Used: 200, Limit: 200}
	env.server.SetHMESyncService(&fakeHMESyncService{
		getSession: func(context.Context, int64) (hmesync.SessionInfo, error) {
			return hmesync.SessionInfo{Status: hmesync.StatusAuthenticated}, nil
		},
		deleteAlias: func(context.Context, int64) error { return wait },
		deleteAliases: func(context.Context, []int64) ([]hmesync.AliasDeletionOutcome, error) {
			return []hmesync.AliasDeletionOutcome{{AliasID: alias.ID, Err: wait}}, nil
		},
	})
	single := env.request(t, http.MethodDelete, fmt.Sprintf("/admin/api/v1/aliases/%d", alias.ID), nil, "", []*http.Cookie{cookie}, csrf)
	if single.Code != http.StatusTooManyRequests || !strings.Contains(single.Body.String(), "retry_at") || !strings.Contains(single.Body.String(), retryAt.Format(time.RFC3339Nano)) {
		t.Fatalf("single quota=%d %s", single.Code, single.Body.String())
	}
	batch := env.request(t, http.MethodDelete, "/admin/api/v1/aliases/batch", adminAPITestJSON(t, map[string]any{"alias_ids": []int64{alias.ID}}), "application/json", []*http.Cookie{cookie}, csrf)
	if batch.Code != http.StatusOK || !strings.Contains(batch.Body.String(), "retry_at") || !strings.Contains(batch.Body.String(), retryAt.Format(time.RFC3339Nano)) {
		t.Fatalf("batch quota=%d %s", batch.Code, batch.Body.String())
	}
}

func TestAliasDeletionDeferredCauseDoesNotBecomeAnAppleRequestFailure(t *testing.T) {
	apiErr := adminAPIBatchAliasDeleteError(errors.Join(hmesync.ErrBatchDeferred, hmesync.ErrRateLimited))
	if apiErr.Code != hmesync.CodeBatchDeferred || !strings.Contains(apiErr.Message, "尚未执行") {
		t.Fatalf("unattempted item misclassified=%#v", apiErr)
	}
}
