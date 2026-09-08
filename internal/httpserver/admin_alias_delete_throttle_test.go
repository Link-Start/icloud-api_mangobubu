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

func TestAliasDeletionJobWaitsOnFirstAppleThrottleWithoutFailingRemainingItems(t *testing.T) {
	for _, scenario := range []struct{ stop, bodyTimeout bool }{{}, {stop: true}, {bodyTimeout: true}, {stop: true, bodyTimeout: true}} {
		t.Run(fmt.Sprintf("stop=%t/body-timeout=%t", scenario.stop, scenario.bodyTimeout), func(t *testing.T) {
			stopWhileWaiting := scenario.stop
			env := newAdminAPITestEnv(t)
			cookie, csrf, admin := env.createSession(t, "throttle-wait-admin", "unused-password")
			account := adminAPITestCreateAccount(t, env, "throttle-owner@icloud.com")
			first := adminAPITestCreateDeleteAlias(t, env, account.ID, "throttle-first@icloud.com")
			second := adminAPITestCreateDeleteAlias(t, env, account.ID, "throttle-second@icloud.com")
			directory := apple.ListResult{SelectedForwardTo: account.Email, ForwardToEmails: []string{account.Email}}
			for _, alias := range []domain.Alias{first, second} {
				directory.Aliases = append(directory.Aliases, apple.Alias{
					HME: alias.Address, AnonymousID: fmt.Sprint(alias.ID), ForwardToEmail: account.Email, IsActive: true,
				})
			}
			directoryJSON := adminAPITestJSON(t, map[string]any{"success": true, "result": directory})
			var requests, validates atomic.Int32
			client, err := apple.NewClient(apple.Config{Transport: batchDeletionTestTransport(func(request *http.Request) (*http.Response, error) {
				requests.Add(1)
				status := http.StatusOK
				headers := http.Header{"Content-Type": []string{"application/json"}}
				body := `{"success":true}`
				switch request.URL.Path {
				case "/setup/ws/1/validate":
					if validates.Add(1) == 1 {
						status = http.StatusTooManyRequests
						headers.Set("Retry-After", "90")
						body = `{"success":false}`
					} else {
						body = `{"dsInfo":{"dsid":"42","primaryEmail":"throttle-apple@example.com","hsaVersion":2},"hsaTrustedBrowser":true,"webservices":{"premiummailsettings":{"url":"https://p01-maildomainws.icloud.com"}}}`
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
			waiting, resume := make(chan struct{}), make(chan struct{})
			service, err := hmesync.New(env.store, env.cipher, client, batchDeletionTestLocker{},
				hmesync.WithClock(func() time.Time { return clockBase.Add(time.Duration(clockOffset.Load())) }),
				hmesync.WithAliasDeletionWaiter(func(ctx context.Context, delay time.Duration) error {
					if delay >= time.Minute {
						if delay < 90*time.Second {
							return errors.New("server Retry-After was shortened")
						}
						close(waiting)
						select {
						case <-resume:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					if err := ctx.Err(); err != nil {
						return err
					}
					clockOffset.Add(int64(delay))
					return nil
				}),
			)
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
			_, err = env.store.UpsertAppleWebSession(context.Background(), domain.AppleWebSession{
				AccountID: account.ID, AppleID: session.AppleID, Region: "global", Authenticated: true,
				Ciphertext: ciphertext, LastValidatedAt: &session.ValidatedAt,
			})
			if err != nil {
				t.Fatal(err)
			}
			env.server.SetHMESyncService(service)
			stop, stopped := startTestAliasDeletionJobs(t, env)
			response := env.request(t, http.MethodDelete, "/admin/api/v1/aliases/batch?async=1", adminAPITestJSON(t, map[string]any{
				"alias_ids": []int64{first.ID, second.ID}, "operation_id": testAliasDeletionJobID,
			}), "application/json", []*http.Cookie{cookie}, csrf)
			if response.Code != http.StatusAccepted {
				t.Fatalf("admission = %d %s", response.Code, response.Body.String())
			}
			waitTestAliasDeletionSignal(t, waiting)
			progress := env.request(t, http.MethodGet, "/admin/api/v1/aliases/batch/jobs/"+testAliasDeletionJobID, nil, "", []*http.Cookie{cookie}, "")
			state := decodeTestAliasDeletionJob(t, progress)
			if progress.Code != http.StatusOK || state.Status != domain.AliasDeletionJobRunning || state.Processed != 0 || state.Failed != 0 || len(state.Results) != 0 || len(state.Waits) != 1 {
				t.Fatalf("throttle was fanned out as completed failures: %d %#v", progress.Code, state)
			}
			wait := state.Waits[0]
			if wait.AccountID != account.ID || wait.AliasID != first.ID || wait.Operation != "validate" || wait.HTTPStatus != 429 || wait.Attempt != 1 || wait.MaxAttempts != 3 || requests.Load() != 1 {
				t.Fatalf("waiting evidence = %#v requests=%d", wait, requests.Load())
			}
			retryAt, err := time.Parse(time.RFC3339Nano, wait.RetryAt)
			if err != nil || retryAt.Before(clockBase.Add(90*time.Second)) {
				t.Fatalf("Retry-After deadline = %q err=%v", wait.RetryAt, err)
			}
			if stopWhileWaiting {
				stop()
				waitTestAliasDeletionSignal(t, stopped)
			} else {
				close(resume)
			}
			job := waitTestAliasDeletionJob(t, env, admin.ID, testAliasDeletionJobID)
			final := env.server.adminAPIAliasDeletionJobSnapshot(job)
			if len(final.Waits) != 0 {
				t.Error("cooldown leaked into terminal snapshot")
			}
			if stopWhileWaiting {
				if final.Status != domain.AliasDeletionJobInterrupted || final.Deleted != 0 || requests.Load() != 1 {
					t.Fatalf("shutdown sent another request or lost interruption: %#v requests=%d", final, requests.Load())
				}
			} else if final.Status != domain.AliasDeletionJobCompleted || final.Deleted != 2 || final.Failed != 0 {
				t.Fatalf("cooldown recovery failed: %#v", final)
			}
		})
	}
}

func TestAliasDeletionWaitStateIsOwnerScopedAndNotACompletedResult(t *testing.T) {
	env := newAdminAPITestEnv(t)
	job := domain.AliasDeletionJob{ID: testAliasDeletionJobID, AdminID: 1, Status: domain.AliasDeletionJobRunning,
		Items: []domain.AliasDeletionJobItem{{ID: 9, Address: "pending@icloud.com"}},
	}
	wait := hmesync.AliasDeletionWait{AccountID: 3, AliasID: 9, Operation: "delete", RetryAt: time.Now().Add(time.Minute), Attempt: 1, MaxAttempts: 3, Waiting: true, HTTPStatus: 200, ServiceCode: "-41015"}
	env.server.recordAliasDeletionWait(job, wait)
	snapshot := env.server.adminAPIAliasDeletionJobSnapshot(job)
	if len(snapshot.Waits) != 1 || snapshot.Processed != 0 || snapshot.Failed != 0 || snapshot.Deleted != 0 {
		t.Fatalf("wait counted as a result: %#v", snapshot)
	}
	other := job
	other.AdminID = 2
	if len(env.server.adminAPIAliasDeletionJobSnapshot(other).Waits) != 0 {
		t.Error("same job ID leaked another administrator's wait")
	}
	wait.Waiting = false
	env.server.recordAliasDeletionWait(job, wait)
	if len(env.server.adminAPIAliasDeletionJobSnapshot(job).Waits) != 0 {
		t.Error("wait was not cleared")
	}
	job.Items[0] = domain.AliasDeletionJobItem{ID: 9, Address: "pending@icloud.com", Done: true, Code: "APPLE_BATCH_DEFERRED", LocalRetained: true}
	snapshot = adminAPIAliasDeletionJobFromRecord(job)
	if snapshot.Deferred != 1 || snapshot.Failed != 1 || snapshot.Deleted != 0 {
		t.Fatalf("unattempted count = %#v", snapshot)
	}
}

func TestAliasDeletionWaitSanitizesProtocolDiagnostics(t *testing.T) {
	env := newAdminAPITestEnv(t)
	job := domain.AliasDeletionJob{ID: testAliasDeletionJobID, AdminID: 1, Status: domain.AliasDeletionJobRunning}
	env.server.recordAliasDeletionWait(job, hmesync.AliasDeletionWait{
		AccountID: 3, AliasID: 4, Operation: "secret-token-url", ServiceCode: "secret=cookie-value", HTTPStatus: -1,
		RetryAt: time.Now().Add(time.Minute), Attempt: 1, MaxAttempts: 3, Waiting: true,
	})
	state := env.server.adminAPIAliasDeletionJobSnapshot(job)
	if len(state.Waits) != 1 || state.Waits[0].Operation != "" || state.Waits[0].ServiceCode != "" || state.Waits[0].HTTPStatus != 0 {
		t.Fatalf("unsafe diagnostics were copied: %#v", state.Waits)
	}
}

func TestAliasDeletionDeferredCauseDoesNotBecomeAnAppleRequestFailure(t *testing.T) {
	err := errors.Join(hmesync.ErrBatchDeferred, hmesync.ErrRateLimited)
	apiErr := adminAPIBatchAliasDeleteError(err)
	if apiErr.Code != hmesync.CodeBatchDeferred || !strings.Contains(apiErr.Message, "尚未执行") {
		t.Fatalf("unattempted item misclassified: %#v", apiErr)
	}
}
