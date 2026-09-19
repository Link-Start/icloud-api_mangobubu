package autocreate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

type serviceRejectionTransport func(*http.Request) (*http.Response, error)

func (f serviceRejectionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestAppleBusinessRejectionFlowsThroughCreationLogsAndOriginalSchedule(t *testing.T) {
	for _, operation := range []string{"generate", "reserve"} {
		for _, rawCode := range []string{`-27577`, `"-27577"`} {
			t.Run(operation+"/"+rawCode, func(t *testing.T) {
				const candidate = "private-candidate@icloud.com"
				const responseDetail = "Apple rejected the requested alias"
				var responseBytes int
				generateRequests, reserveRequests, creatorCalls := 0, 0, 0
				failOperation := true
				client, err := apple.NewClient(apple.Config{Transport: serviceRejectionTransport(func(request *http.Request) (*http.Response, error) {
					var body string
					switch request.URL.Path {
					case "/v1/hme/generate":
						generateRequests++
						body = `{"success":true,"result":{"hme":"` + candidate + `"}}`
					case "/v1/hme/reserve":
						reserveRequests++
						body = `{"success":true,"result":{"hme":{"hme":"` + candidate + `"}}}`
					default:
						t.Fatalf("unexpected Apple request: %s", request.URL.Path)
					}
					if failOperation && request.URL.Path == "/v1/hme/"+operation {
						body = fmt.Sprintf(`{"success":false,"error":{"errorCode":%s,"errorMessage":%q},"sessionToken":"response-session-secret","nested":{"authorization":"Bearer response-auth-secret","email":%q}}`, rawCode, responseDetail, candidate)
						responseBytes = len(body)
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(body)),
						Request:    request,
					}, nil
				})})
				if err != nil {
					t.Fatal(err)
				}
				clock := newTestClock(time.Date(2026, 9, 19, 8, 41, 4, 0, time.UTC))
				repo := newFakeRepository()
				manager, logs := newFlowLogManager(t, repo, clock, func(ctx context.Context, accountID int64) (domain.Alias, error) {
					creatorCalls++
					domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseReserving, 65, 0)
					alias, _, createErr := client.CreateAlias(ctx, apple.Session{
						Region:                 apple.RegionGlobal,
						DSID:                   "42",
						ClientID:               "test-client",
						PremiumMailSettingsURL: "https://p01-maildomainws.icloud.com",
					}, "test label", "test note")
					if createErr != nil {
						var upstream *apple.Error
						if !errors.As(createErr, &upstream) || !upstream.ServiceRejected || upstream.Retryable || upstream.ServiceCode != "-27577" {
							t.Fatalf("business rejection = %#v", upstream)
						}
						if alias != (apple.Alias{}) {
							t.Fatalf("business rejection returned a candidate: %#v", alias)
						}
						return domain.Alias{}, createErr
					}
					return domain.Alias{ID: 101, AccountID: accountID, Address: alias.HME}, nil
				})
				schedule := enableForTest(t, manager, 101)
				if len(schedule.PlannedAt) < 2 {
					t.Fatal("test requires at least two scheduled runs")
				}
				wantNext := schedule.PlannedAt[1]
				clock.Set(*schedule.NextRunAt)
				manager.runDue(context.Background())
				manager.runDue(context.Background())
				wantReserves := 0
				if operation == "reserve" {
					wantReserves = 1
				}
				if creatorCalls != 1 || generateRequests != 1 || reserveRequests != wantReserves {
					t.Fatalf("business rejection replayed: creator=%d generate=%d reserve=%d", creatorCalls, generateRequests, reserveRequests)
				}
				failed := requireAutoCreateEvent(t, logs, "run_failed")
				for field, want := range map[string]string{
					"error_code":                  "APPLE_UPSTREAM_ERROR",
					"error_class":                 "apple_upstream",
					"cause_category":              "apple_upstream",
					"failed_stage":                string(domain.AliasCreationPhaseReserving),
					"failed_operation":            "reserve_alias",
					"operation":                   operation + " Hide My Email alias",
					"http_status":                 "200",
					"retryable":                   "false",
					"upstream_retryable":          "false",
					"service_code_fingerprint":    "d4d28aec6671015f",
					"remote_side_effect_possible": "false",
					"pending_confirmation":        "false",
					"failure_state_recorded":      "true",
					"auto_creation_disabled":      "false",
					"schedule_action":             "continue",
					"next_run_at":                 wantNext.Format(time.RFC3339Nano),
					"apple_response_operation":    operation + " Hide My Email alias",
					"apple_response_http_status":  "200",
					"apple_response_format":       "json",
					"apple_response_service_code": "-27577",
					"apple_response_bytes":        strconv.Itoa(responseBytes),
					"apple_response_truncated":    "false",
				} {
					if failed.Fields[field] != want {
						t.Fatalf("business rejection %s = %q, want %q; fields=%#v", field, failed.Fields[field], want, failed.Fields)
					}
				}
				if !strings.Contains(failed.Fields["error_context"], "明确返回业务失败") ||
					strings.Contains(failed.Fields["error_context"], "限流") {
					t.Fatalf("business failure reason is ambiguous: %q", failed.Fields["error_context"])
				}
				current, err := manager.GetSchedule(context.Background(), 101)
				if err != nil || !current.Enabled || current.NextRunAt == nil || !current.NextRunAt.Equal(wantNext) ||
					len(repo.failures) != 1 || len(repo.successes) != 0 || current.LastError != failed.Fields["error_context"] {
					t.Fatalf("business failure changed original schedule or state: %#v err=%v", current, err)
				}
				var original struct {
					Success bool `json:"success"`
					Error   struct {
						Code    json.RawMessage `json:"errorCode"`
						Message string          `json:"errorMessage"`
					} `json:"error"`
				}
				if err := json.Unmarshal([]byte(failed.Fields["apple_response_excerpt"]), &original); err != nil ||
					original.Success || string(original.Error.Code) != rawCode || original.Error.Message != responseDetail {
					t.Fatalf("original rejection missing from log: %#v err=%v", failed.Fields, err)
				}
				assertFlowLogsDoNotContain(t, logs, candidate, "response-session-secret", "response-auth-secret")

				failOperation = false
				clock.Set(wantNext)
				manager.runDue(context.Background())
				if creatorCalls != 2 || generateRequests != 2 || reserveRequests != wantReserves+1 ||
					len(repo.failures) != 1 || len(repo.successes) != 1 {
					t.Fatalf("next original slot did not recover: creator=%d generate=%d reserve=%d failures=%d successes=%d",
						creatorCalls, generateRequests, reserveRequests, len(repo.failures), len(repo.successes))
				}
				requireAutoCreateEvent(t, logs, "run_completed")
			})
		}
	}
}

func TestServiceRejectionPreservesIndependentRemoteEffectsAndDiagnostics(t *testing.T) {
	rejected := &apple.Error{
		Op: "reserve Hide My Email alias", Kind: apple.ErrService,
		StatusCode: http.StatusOK, ServiceCode: "-27577", ServiceRejected: true,
	}
	for _, test := range []struct {
		name            string
		err             error
		priorForwarding bool
		wantRemote      bool
		wantPending     bool
		wantDisabled    bool
		wantCode        string
	}{
		{
			name: "earlier forwarding update", err: rejected, priorForwarding: true,
			wantRemote: true,
		},
		{
			name:     "joined persistence failure",
			err:      errors.Join(testDiagnosticError{code: "AUTO_CREATION_PERSISTENCE_ERROR", detail: "private database detail"}, rejected),
			wantCode: "AUTO_CREATION_PERSISTENCE_ERROR",
		},
		{
			name: "pending candidate", err: testPendingConfirmationError{err: rejected},
			wantRemote: true, wantPending: true,
		},
		{
			name: "untracked remote mutation", err: &untrackedRemoteSideEffectTestError{cause: rejected},
			wantRemote: true, wantDisabled: true,
		},
		{
			name: "unknown 2xx result",
			err: &apple.Error{Op: "reserve Hide My Email alias", Kind: apple.ErrService,
				StatusCode: http.StatusOK, ServiceCode: "-27577"},
			wantRemote: true,
		},
		{
			name: "malformed 2xx result",
			err: &apple.Error{Op: "decode reserve Hide My Email alias", Kind: apple.ErrInvalidResponse,
				StatusCode: http.StatusOK, Err: errors.New("private malformed response")},
			wantRemote: true,
		},
		{
			name: "another operation rejected",
			err: &apple.Error{Op: "list Hide My Email aliases", Kind: apple.ErrService,
				StatusCode: http.StatusOK, ServiceCode: "-27577", ServiceRejected: true},
			wantRemote: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock := newTestClock(time.Date(2026, 9, 19, 8, 41, 4, 0, time.UTC))
			manager, logs := newFlowLogManager(t, newFakeRepository(), clock, func(ctx context.Context, _ int64) (domain.Alias, error) {
				if test.priorForwarding {
					domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseInitializingForwarding, 50, 0)
				}
				domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseReserving, 65, 0)
				return domain.Alias{}, test.err
			})
			schedule := enableForTest(t, manager, 102)
			clock.Set(*schedule.NextRunAt)
			manager.runDue(context.Background())
			failed := requireAutoCreateEvent(t, logs, "run_failed")
			wantCode := test.wantCode
			if wantCode == "" {
				wantCode = "APPLE_UPSTREAM_ERROR"
			}
			for field, want := range map[string]string{
				"error_code":                  wantCode,
				"remote_side_effect_possible": strconv.FormatBool(test.wantRemote),
				"pending_confirmation":        strconv.FormatBool(test.wantPending),
				"auto_creation_disabled":      strconv.FormatBool(test.wantDisabled),
			} {
				if failed.Fields[field] != want {
					t.Fatalf("%s = %q, want %q; fields=%#v", field, failed.Fields[field], want, failed.Fields)
				}
			}
			assertFlowLogsDoNotContain(t, logs, "-27577", "private database detail", "private malformed response")
		})
	}
}
