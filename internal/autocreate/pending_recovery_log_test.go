package autocreate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

func TestDiscardedAliasCandidateLogsRecoveryAndKeepsNextCreationScheduled(t *testing.T) {
	const code = "APPLE_ALIAS_CANDIDATE_DISCARDED"
	const reason = "Apple 最新目录未找到候选地址，已清理本地待确认记录；下次计划将重新创建"
	const sensitive = "discarded-candidate-secret@icloud.com"
	clock := newTestClock(time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC))
	repo := newFakeRepository()
	calls := 0
	manager, logs := newFlowLogManager(t, repo, clock, func(ctx context.Context, accountID int64) (domain.Alias, error) {
		calls++
		if calls == 1 {
			domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseReconciling, 85, 1)
			return domain.Alias{}, fmt.Errorf("candidate %s: %w", sensitive, testDiagnosticError{
				code: code, detail: "private discarded candidate detail",
			})
		}
		domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseReserving, 65, 0)
		return domain.Alias{ID: 97, AccountID: accountID, Address: "new-alias@icloud.com"}, nil
	})
	schedule := enableForTest(t, manager, 97)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(context.Background())

	failed := requireAutoCreateEvent(t, logs, "run_failed")
	for field, want := range map[string]string{
		"error_code":                  code,
		"error_context":               reason,
		"error_class":                 "account_state",
		"cause_category":              "account_state",
		"failed_stage":                string(domain.AliasCreationPhaseReconciling),
		"failed_operation":            "reconcile_alias_directory",
		"failure_state_recorded":      "true",
		"pending_confirmation":        "false",
		"remote_side_effect_possible": "false",
		"auto_creation_disabled":      "false",
		"schedule_action":             "continue",
	} {
		if failed.Fields[field] != want {
			t.Fatalf("discarded candidate %s = %q, want %q; fields=%#v", field, failed.Fields[field], want, failed.Fields)
		}
	}
	assertAutoCreateEventsAbsent(t, logs, "run_completed", "run_completed_with_warning")
	assertFlowLogsDoNotContain(t, logs, sensitive, "private discarded candidate detail")
	if len(repo.successes) != 0 || len(repo.failures) != 1 || repo.failures[0].message != reason {
		t.Fatalf("candidate cleanup recorded as creation: successes=%#v failures=%#v", repo.successes, repo.failures)
	}
	current, err := manager.GetSchedule(context.Background(), 97)
	if err != nil || !current.Enabled || current.NextRunAt == nil || !current.NextRunAt.After(clock.Now()) || current.LastCreatedAt != nil {
		t.Fatalf("schedule after local candidate cleanup = %#v err=%v", current, err)
	}
	clock.Set(*current.NextRunAt)
	manager.runDue(context.Background())
	if calls != 2 || len(repo.successes) != 1 || len(repo.failures) != 1 {
		t.Fatalf("next scheduled creation did not recover: calls=%d successes=%#v failures=%#v", calls, repo.successes, repo.failures)
	}
	requireAutoCreateEvent(t, logs, "run_completed")
	current, err = manager.GetSchedule(context.Background(), 97)
	if err != nil || !current.Enabled || current.LastCreatedAt == nil || current.LastError != "" {
		t.Fatalf("schedule after subsequent creation = %#v err=%v", current, err)
	}
}

func TestDiscardedCandidateRetainsEarlierForwardingMutationDiagnostic(t *testing.T) {
	clock := newTestClock(time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC))
	manager, logs := newFlowLogManager(t, newFakeRepository(), clock, func(ctx context.Context, _ int64) (domain.Alias, error) {
		domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseInitializingForwarding, 50, 0)
		domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseReconciling, 85, 1)
		return domain.Alias{}, testDiagnosticError{code: "APPLE_ALIAS_CANDIDATE_DISCARDED"}
	})
	schedule := enableForTest(t, manager, 98)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(context.Background())
	failed := requireAutoCreateEvent(t, logs, "run_failed")
	if failed.Fields["remote_side_effect_possible"] != "true" || failed.Fields["pending_confirmation"] != "false" {
		t.Fatalf("discard erased earlier forwarding mutation: %#v", failed.Fields)
	}
}

func TestPendingAliasRecoveryReasonsDoNotClaimCreation(t *testing.T) {
	pending := aliasCreationErrorReason("APPLE_ALIAS_CONFIRMATION_PENDING")
	if strings.Contains(pending, "已创建") || !strings.Contains(pending, "满 5 分钟") || !strings.Contains(pending, "完整目录") || !strings.Contains(pending, "自动清理") {
		t.Fatalf("pending candidate reason is not bounded or claims creation: %q", pending)
	}
	inactive := diagnoseAliasCreationError(testDiagnosticError{code: "APPLE_ALIAS_INACTIVE", detail: "private inactive detail"})
	if inactive.code != "APPLE_ALIAS_INACTIVE" || inactive.class != "account_state" ||
		!strings.Contains(inactive.reason, "已停用") || !strings.Contains(inactive.reason, "本地记录已保留") ||
		!strings.Contains(inactive.reason, "重新启用") || strings.Contains(inactive.reason, "等待目录") || strings.Contains(inactive.reason, "private") {
		t.Fatalf("inactive candidate diagnostic = %#v", inactive)
	}
}

func TestPendingCandidateFailuresRetainSpecificDiagnosticsAndSchedule(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		cause      error
		code       string
		class      string
		httpStatus string
		cooldown   bool
	}{
		{
			name: "inactive", cause: testDiagnosticError{code: "APPLE_ALIAS_INACTIVE", detail: "private inactive candidate"},
			code: "APPLE_ALIAS_INACTIVE", class: "account_state",
		},
		{
			name: "directory upstream error",
			cause: &apple.Error{Op: "list Hide My Email aliases", Kind: apple.ErrService, StatusCode: http.StatusServiceUnavailable,
				Retryable: true, Err: errors.New("private directory response")},
			code: "APPLE_UPSTREAM_ERROR", class: "apple_upstream", httpStatus: "503",
		},
		{
			name: "directory throttle",
			cause: &apple.Error{Op: "list Hide My Email aliases", Kind: apple.ErrService, StatusCode: http.StatusTooManyRequests,
				Retryable: true, Err: errors.New("private throttle response")},
			code: "APPLE_RATE_LIMITED", class: "apple_upstream", httpStatus: "429", cooldown: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			clock := newTestClock(time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC))
			repo := newFakeRepository()
			manager, logs := newFlowLogManager(t, repo, clock, func(ctx context.Context, _ int64) (domain.Alias, error) {
				domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseReconciling, 85, 1)
				return domain.Alias{}, testPendingConfirmationError{err: testCase.cause}
			})
			schedule := enableForTest(t, manager, 99)
			clock.Set(*schedule.NextRunAt)
			manager.runDue(context.Background())
			failed := requireAutoCreateEvent(t, logs, "run_failed")
			if failed.Fields["error_code"] != testCase.code || failed.Fields["error_class"] != testCase.class ||
				failed.Fields["cause_category"] != testCase.class || failed.Fields["pending_confirmation"] != "true" ||
				failed.Fields["http_status"] != testCase.httpStatus || failed.Fields["schedule_action"] != "continue" ||
				failed.Fields["auto_creation_disabled"] != "false" {
				t.Fatalf("retained candidate diagnostics = %#v", failed.Fields)
			}
			if len(repo.failures) != 1 || repo.failures[0].message != aliasCreationErrorReason(testCase.code) || len(repo.successes) != 0 {
				t.Fatalf("retained candidate result state: failures=%#v successes=%#v", repo.failures, repo.successes)
			}
			current, err := manager.GetSchedule(context.Background(), 99)
			if err != nil || !current.Enabled || current.NextRunAt == nil {
				t.Fatalf("retained candidate schedule = %#v err=%v", current, err)
			}
			if testCase.cooldown && !current.NextRunAt.Equal(clock.Now().Add(appleRateLimitCooldown)) {
				t.Fatalf("pending candidate throttle skipped cooldown: %#v", current)
			}
			assertAutoCreateEventsAbsent(t, logs, "run_completed")
			assertFlowLogsDoNotContain(t, logs, "private inactive candidate", "private directory response", "private throttle response")
		})
	}
}
