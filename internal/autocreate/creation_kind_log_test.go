package autocreate

import (
	"context"
	"testing"
	"time"

	"icloud-api/internal/domain"
)

func TestAliasCreationLogsKeepSelectedKindThroughConfirmation(t *testing.T) {
	for _, testCase := range []struct {
		name string
		kind domain.AliasCreationKind
		code string
		want string
	}{
		{"new candidate awaiting directory", domain.AliasCreationKindNew, "APPLE_ALIAS_CONFIRMATION_PENDING", "new"},
		{"old candidate discarded", domain.AliasCreationKindReconcile, "APPLE_ALIAS_CANDIDATE_DISCARDED", "reconcile"},
		{"unrecognized kind", "private-kind-secret", "AUTO_CREATE_FAILED", "undetermined"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			clock := newTestClock(time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC))
			manager, logs := newFlowLogManager(t, newFakeRepository(), clock, func(ctx context.Context, _ int64) (domain.Alias, error) {
				domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseCheckingCapacity, 15, 0)
				selected := domain.WithAliasCreationKind(ctx, testCase.kind)
				domain.ReportAliasCreationProgress(selected, domain.AliasCreationPhaseLoadingSession, 25, 0)
				if testCase.kind == domain.AliasCreationKindNew {
					domain.ReportAliasCreationProgress(selected, domain.AliasCreationPhaseReserving, 65, 0)
				}
				// A producer using the original context must not erase a known kind.
				domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseReconciling, 85, 1)
				return domain.Alias{}, testDiagnosticError{code: testCase.code}
			})
			schedule := enableForTest(t, manager, 71)
			clock.Set(*schedule.NextRunAt)
			manager.runDue(context.Background())

			started := requireAutoCreateEvent(t, logs, "run_started")
			if started.Fields["auto_create_kind"] != "undetermined" {
				t.Fatalf("run kind was guessed before reading pending state: %#v", started.Fields)
			}
			entry := requireAutoCreateEvent(t, logs, "run_failed")
			if entry.Fields["auto_create_kind"] != testCase.want || entry.Fields["failed_stage"] != "reconciling" {
				t.Fatalf("terminal creation kind or stage = %#v", entry.Fields)
			}
			confirmation := requireAutoCreateStage(t, logs, domain.AliasCreationPhaseReconciling)
			if confirmation.Fields["auto_create_kind"] != testCase.want {
				t.Fatalf("directory confirmation changed creation kind: %#v", confirmation.Fields)
			}
			assertFlowLogsDoNotContain(t, logs, "private-kind-secret")
		})
	}
}

func TestAliasCreationTerminalProgressCarriesKindWithoutAnotherStage(t *testing.T) {
	clock := newTestClock(time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC))
	manager, logs := newFlowLogManager(t, newFakeRepository(), clock, func(ctx context.Context, _ int64) (domain.Alias, error) {
		domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseCheckingCapacity, 15, 0)
		ctx = domain.WithAliasCreationKind(ctx, domain.AliasCreationKindNew)
		domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseFailed, 15, 0)
		return domain.Alias{}, testDiagnosticError{code: "ALIAS_LIMIT_REACHED"}
	})
	schedule := enableForTest(t, manager, 71)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(context.Background())
	failed := requireAutoCreateEvent(t, logs, "run_failed")
	if failed.Fields["auto_create_kind"] != "new" || failed.Fields["failed_stage"] != "checking_capacity" {
		t.Fatalf("terminal metadata overwrote stage or lost kind: %#v", failed.Fields)
	}
}
