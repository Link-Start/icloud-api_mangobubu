package autocreate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

func TestOriginalAppleResponseRetainsItsOperationInJoinedFailure(t *testing.T) {
	client, err := apple.NewClient(apple.Config{Transport: serviceRejectionTransport(func(request *http.Request) (*http.Response, error) {
		body := `{"success":true,"result":{"hme":"candidate@icloud.com"}}`
		if request.URL.Path == "/v1/hme/reserve" {
			body = `{"success":false,"error":{"errorCode":-27577,"errorMessage":"Alias rejected"}}`
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(body)), Request: request,
		}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, _, reserveErr := client.CreateAlias(context.Background(), apple.Session{
		Region: apple.RegionGlobal, DSID: "42", ClientID: "test-client",
		PremiumMailSettingsURL: "https://p01-maildomainws.icloud.com",
	}, "test", "test")
	if reserveErr == nil {
		t.Fatal("expected fixture reserve failure")
	}
	clock := newTestClock(time.Date(2026, 9, 19, 13, 20, 40, 0, time.UTC))
	manager, logs := newFlowLogManager(t, newFakeRepository(), clock, func(ctx context.Context, _ int64) (domain.Alias, error) {
		domain.ReportAliasCreationProgress(ctx, domain.AliasCreationPhaseReconciling, 85, 1)
		return domain.Alias{}, errors.Join(
			&apple.Error{Op: "list Hide My Email aliases", Kind: apple.ErrService, StatusCode: http.StatusServiceUnavailable},
			fmt.Errorf("sensitive local context: %w", reserveErr),
		)
	})
	schedule := enableForTest(t, manager, 71)
	clock.Set(*schedule.NextRunAt)
	manager.runDue(context.Background())
	failed := requireAutoCreateEvent(t, logs, "run_failed")
	if failed.Fields["operation"] != "list Hide My Email aliases" || failed.Fields["http_status"] != "503" ||
		failed.Fields["apple_response_operation"] != "reserve Hide My Email alias" || failed.Fields["apple_response_http_status"] != "200" ||
		!strings.Contains(failed.Fields["apple_response_excerpt"], "Alias rejected") {
		t.Fatalf("original response was attributed to the wrong request: %#v", failed.Fields)
	}
	assertFlowLogsDoNotContain(t, logs, "sensitive local context", "candidate@icloud.com")
}

func TestOriginalAppleResponseDoesNotFallBackToArbitraryErrorText(t *testing.T) {
	err := &apple.Error{
		Op: "reserve Hide My Email alias", Kind: apple.ErrService,
		ServiceCode: "sensitive-unknown-code", Err: errors.New("sensitive response text"),
	}
	if attrs := aliasCreationResponseAttrs(err); len(attrs) != 0 {
		t.Fatalf("missing snapshot fell back to raw error text: %#v", attrs)
	}
}
