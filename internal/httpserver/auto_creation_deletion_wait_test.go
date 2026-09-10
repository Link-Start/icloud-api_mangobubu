package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/autocreate"
	"icloud-api/internal/domain"
	"icloud-api/internal/hmesync"
	"icloud-api/internal/syncer"
)

func TestAdminAPIAutoCreationTogglesDuringAppleBatchDeletionCooldown(t *testing.T) {
	env := newAdminAPITestEnv(t)
	cookie, csrf, admin := env.createSession(t, "auto-create-delete-wait-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "auto-create-delete-wait@icloud.com")
	first := adminAPITestCreateDeleteAlias(t, env, account.ID, "auto-create-delete-first@icloud.com")
	second := adminAPITestCreateDeleteAlias(t, env, account.ID, "auto-create-delete-second@icloud.com")
	directory := apple.ListResult{SelectedForwardTo: account.Email, ForwardToEmails: []string{account.Email}}
	for _, alias := range []domain.Alias{first, second} {
		directory.Aliases = append(directory.Aliases, apple.Alias{
			AnonymousID: fmt.Sprint(alias.ID), HME: alias.Address, ForwardToEmail: account.Email, IsActive: true,
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
				body = `{"dsInfo":{"dsid":"42","primaryEmail":"auto-create-delete-apple@example.com","hsaVersion":2},"hsaTrustedBrowser":true,"webservices":{"premiummailsettings":{"url":"https://p01-maildomainws.icloud.com"}}}`
			}
		case "/v2/hme/list":
			body = string(directoryJSON)
		case "/v1/hme/deactivate", "/v1/hme/delete":
		default:
			return nil, fmt.Errorf("unexpected fixture Apple path %s", request.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}

	clockBase := time.Now().UTC()
	var clockOffset atomic.Int64
	waiting, resume := make(chan struct{}), make(chan struct{})
	// Both paths use the production keyed lock. A no-op fixture would hide the
	// regression where deletion holds this lock throughout the Apple cooldown.
	locker := syncer.New(env.store, env.cipher, nil, env.server.logger, time.Minute, 1)
	service, err := hmesync.New(env.store, env.cipher, client, locker,
		hmesync.WithClock(func() time.Time { return clockBase.Add(time.Duration(clockOffset.Load())) }),
		hmesync.WithAliasDeletionWaiter(func(ctx context.Context, delay time.Duration) error {
			if delay >= time.Minute {
				if delay < 90*time.Second {
					return fmt.Errorf("Apple Retry-After shortened to %s", delay)
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
	autoCreate, err := autocreate.New(env.store, service.CreateAlias, env.server.logger,
		autocreate.WithClock(func() time.Time { return clockBase }),
		autocreate.WithRandomFunc(func(int) int { return 0 }),
	)
	if err != nil {
		t.Fatal(err)
	}
	session := apple.Session{
		AppleID: "auto-create-delete-apple@example.com", Region: apple.RegionGlobal,
		DSID: "42", SessionToken: "fixture-token", ValidatedAt: clockBase,
	}
	ciphertext, err := env.cipher.EncryptAppleSession(string(adminAPITestJSON(t, session)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.UpsertAppleWebSession(context.Background(), domain.AppleWebSession{
		AccountID: account.ID, AppleID: session.AppleID, Region: "global", Authenticated: true,
		Ciphertext: ciphertext, LastValidatedAt: &session.ValidatedAt,
	}); err != nil {
		t.Fatal(err)
	}
	env.server.SetAccountLocker(locker.WithAccountLock)
	env.server.SetHMESyncService(service)
	env.server.SetAliasAutoCreationService(autoCreate)

	setEnabled := func(enabled bool) {
		t.Helper()
		// The old lock scope must fail promptly instead of hanging this test for
		// the entire frozen cooldown. The shared production lock honors ctx.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		request := httptest.NewRequest(http.MethodPut,
			fmt.Sprintf("http://admin.example.test/admin/api/v1/accounts/%d/aliases/auto-create", account.ID),
			bytes.NewReader(adminAPITestJSON(t, map[string]bool{"enabled": enabled})),
		).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "http://admin.example.test")
		request.Header.Set(adminAPICSRFHeader, csrf)
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		env.router.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("set auto creation enabled=%t while deletion waits = %d %s", enabled, response.Code, response.Body.String())
		}
		var payload struct {
			Data adminAPIAutoCreationDTO `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		schedule, err := env.store.GetAliasCreationSchedule(context.Background(), account.ID)
		if err != nil {
			t.Fatal(err)
		}
		if schedule.Enabled != enabled || payload.Data.Enabled != enabled {
			t.Fatalf("auto creation enabled=%t was not persisted and returned: schedule=%#v response=%#v", enabled, schedule, payload.Data)
		}
		if enabled {
			if len(schedule.PlannedAt) != autocreate.CreationsPerCycle || schedule.NextRunAt == nil ||
				len(payload.Data.PlannedTimes) != autocreate.CreationsPerCycle || payload.Data.NextRunAt == nil ||
				!schedule.NextRunAt.Equal(schedule.PlannedAt[0]) || !schedule.NextRunAt.After(clockBase) {
				t.Fatalf("enabling did not seed a future creation plan: schedule=%#v response=%#v", schedule, payload.Data)
			}
		} else if len(schedule.PlannedAt) != 0 || schedule.NextRunAt != nil || len(payload.Data.PlannedTimes) != 0 || payload.Data.NextRunAt != nil {
			t.Fatalf("disabling left scheduled creation work: schedule=%#v response=%#v", schedule, payload.Data)
		}
		if schedule.LastAttemptedAt != nil || schedule.LastCreatedAt != nil {
			t.Fatalf("toggling unexpectedly executed automatic creation: %#v", schedule)
		}
	}
	setEnabled(true)
	if requests.Load() != 0 {
		t.Fatalf("initial enable sent %d Apple requests", requests.Load())
	}

	// This helper cancels and joins all workers during cleanup, including when
	// a toggle assertion fails before the frozen cooldown is released.
	stop, stopped := startTestAliasDeletionJobs(t, env)
	admission := env.request(t, http.MethodDelete, "/admin/api/v1/aliases/batch?async=1", adminAPITestJSON(t, map[string]any{
		"alias_ids": []int64{first.ID, second.ID}, "operation_id": testAliasDeletionJobID,
	}), "application/json", []*http.Cookie{cookie}, csrf)
	if admission.Code != http.StatusAccepted {
		t.Fatalf("deletion admission = %d %s", admission.Code, admission.Body.String())
	}
	waitTestAliasDeletionSignal(t, waiting)
	readProgress := func() adminAPIAliasDeletionJobDTO {
		t.Helper()
		response := env.request(t, http.MethodGet, "/admin/api/v1/aliases/batch/jobs/"+testAliasDeletionJobID, nil, "", []*http.Cookie{cookie}, "")
		if response.Code != http.StatusOK {
			t.Fatalf("deletion progress = %d %s", response.Code, response.Body.String())
		}
		return decodeTestAliasDeletionJob(t, response)
	}
	before := readProgress()
	if before.Status != domain.AliasDeletionJobRunning || before.Processed != 0 || before.Deleted != 0 || before.Failed != 0 || len(before.Results) != 0 || len(before.Waits) != 1 {
		t.Fatalf("expected an unprocessed job waiting for Apple: %#v", before)
	}
	if wait := before.Waits[0]; wait.AccountID != account.ID || wait.AliasID != first.ID || wait.Operation != "validate" || wait.HTTPStatus != 429 || wait.Attempt != 1 {
		t.Fatalf("unexpected Apple cooldown: %#v", wait)
	}
	for _, enabled := range []bool{false, true, false} {
		setEnabled(enabled)
		after := readProgress()
		if after.Status != before.Status || after.Processed != before.Processed || after.Deleted != before.Deleted || after.Failed != before.Failed ||
			len(after.Results) != 0 || len(after.Waits) != 1 || after.Waits[0] != before.Waits[0] {
			t.Fatalf("toggle enabled=%t changed the frozen deletion job: before=%#v after=%#v", enabled, before, after)
		}
		if requests.Load() != 1 {
			t.Fatalf("toggle enabled=%t bypassed Apple cooldown: requests=%d, want 1", enabled, requests.Load())
		}
	}

	close(resume)
	job := waitTestAliasDeletionJob(t, env, admin.ID, testAliasDeletionJobID)
	final := env.server.adminAPIAliasDeletionJobSnapshot(job)
	stop()
	waitTestAliasDeletionSignal(t, stopped)
	if final.Status != domain.AliasDeletionJobCompleted || final.Processed != 2 || final.Deleted != 2 || final.Failed != 0 || len(final.Waits) != 0 {
		t.Fatalf("deletion did not resume after toggles: %#v", final)
	}
	if requests.Load() != 7 {
		t.Fatalf("Apple requests after completion = %d, want 7", requests.Load())
	}
	schedule, err := env.store.GetAliasCreationSchedule(context.Background(), account.ID)
	if err != nil || schedule.Enabled || len(schedule.PlannedAt) != 0 || schedule.NextRunAt != nil {
		t.Fatalf("resumed deletion changed the final disabled schedule: %#v err=%v", schedule, err)
	}
}
