package apple

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

type countingIsError struct {
	count *int
	err   error
}

func (e *countingIsError) Error() string { return "counting error" }

func (e *countingIsError) Is(target error) bool {
	(*e.count)++
	return target == e.err
}

func (e *countingIsError) Unwrap() error { return e.err }

func TestErrorIsTraversesCauseOnce(t *testing.T) {
	count := 0
	target := errors.New("target")
	cause := &countingIsError{count: &count, err: target}
	err := &Error{Kind: ErrService, Err: cause}

	if !errors.Is(err, target) {
		t.Fatal("errors.Is should find the wrapped target")
	}
	if count != 1 {
		t.Fatalf("wrapped Is calls = %d, want 1", count)
	}
}

func TestIsRateLimitedRecognizesHTTPAndHMEThrottleCodes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil"},
		{
			name: "HTTP 429",
			err:  &Error{Kind: ErrService, StatusCode: http.StatusTooManyRequests},
			want: true,
		},
		{
			name: "HME batch limit",
			err:  &Error{Kind: ErrService, StatusCode: http.StatusOK, ServiceCode: "  " + hmeRateLimitCodeBatch + "  "},
			want: true,
		},
		{
			name: "expired HME candidate is not a throttle",
			err:  &Error{Kind: ErrService, StatusCode: http.StatusOK, ServiceCode: "-41003"},
			want: false,
		},
		{
			name: "unrelated business error",
			err:  &Error{Kind: ErrService, StatusCode: http.StatusOK, ServiceCode: "-41099"},
		},
		{
			name: "rate limit in a later joined cause",
			err: errors.Join(
				&Error{Kind: ErrService, StatusCode: http.StatusOK, ServiceCode: "-41099"},
				fmt.Errorf("wrapped: %w", &Error{Kind: ErrService, StatusCode: http.StatusOK, ServiceCode: hmeRateLimitCodeBatch}),
			),
			want: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsRateLimited(test.err); got != test.want {
				t.Fatalf("IsRateLimited() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 250_000_000, time.UTC)
	future := now.Add(2 * time.Minute).Truncate(time.Second)
	const maxDuration = time.Duration(1<<63 - 1)
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "empty"},
		{name: "whitespace", value: " \t "},
		{name: "zero", value: "0"},
		{name: "seconds", value: "120", want: 2 * time.Minute},
		{name: "OWS", value: " \t120\t ", want: 2 * time.Minute},
		{name: "leading zeroes", value: strings.Repeat("0", 100) + "120", want: 2 * time.Minute},
		{name: "negative", value: "-1"},
		{name: "negative zero", value: "-0"},
		{name: "plus sign", value: "+120"},
		{name: "fraction", value: "1.5"},
		{name: "exponent", value: "1e3"},
		{name: "hex", value: "0x10"},
		{name: "units", value: "120s"},
		{name: "embedded space", value: "1 20"},
		{name: "list", value: "120, 240"},
		{name: "CRLF", value: "120\r\n"},
		{name: "unicode space", value: "\u00a0120"},
		{name: "non ASCII digits", value: "１２０"},
		{name: "invalid", value: "not-a-date"},
		{name: "large uncapped wait", value: "315360000", want: 315360000 * time.Second},
		{name: "largest whole seconds", value: "9223372036", want: 9223372036 * time.Second},
		{name: "duration overflow", value: "9223372037", want: maxDuration},
		{name: "int64 overflow", value: "9223372036854775808", want: maxDuration},
		{name: "uint64 limit", value: "18446744073709551615", want: maxDuration},
		{name: "uint64 overflow", value: "18446744073709551616", want: maxDuration},
		{name: "arbitrarily large", value: strings.Repeat("9", 1024), want: maxDuration},
		{name: "overflow with invalid suffix", value: strings.Repeat("9", 1024) + "x"},
		{name: "overflow with negative sign", value: "-" + strings.Repeat("9", 1024)},
		{name: "HTTP date", value: future.Format(http.TimeFormat), want: future.Sub(now)},
		{name: "HTTP date OWS", value: " \t" + future.Format(http.TimeFormat) + "\t", want: future.Sub(now)},
		{name: "RFC850 date", value: future.Format("Monday, 02-Jan-06 15:04:05 GMT"), want: future.Sub(now)},
		{name: "asctime date", value: future.Format(time.ANSIC), want: future.Sub(now)},
		{name: "past date", value: now.Add(-time.Second).Format(http.TimeFormat)},
		{name: "current second", value: now.Format(http.TimeFormat)},
		{name: "invalid day", value: "Tue, 32 Sep 2026 12:00:00 GMT"},
		{name: "non HTTP date", value: future.Format(time.RFC3339)},
		{name: "date overflow", value: "Fri, 31 Dec 9999 23:59:59 GMT", want: maxDuration},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parseRetryAfter(test.value, now); got != test.want {
				t.Fatalf("parseRetryAfter(%q) = %v, want %v", test.value, got, test.want)
			}
		})
	}
}

func TestParseRetryAfterRFC850RollingCentury(t *testing.T) {
	for _, year := range []int{1900, 1994, 2026, 2090} {
		now := time.Date(year, time.September, 8, 12, 0, 0, 0, time.UTC)
		for _, offset := range []int{1, 43, 50, 51} {
			future := now.AddDate(offset, 0, 0)
			value := future.Format("Monday, 02-Jan-06 15:04:05 GMT")
			want := future.Sub(now)
			if offset > 50 {
				want = 0
			}
			if got := parseRetryAfter(value, now); got != want {
				t.Fatalf("parseRetryAfter(%q, %v) = %v, want %v", value, now, got, want)
			}
		}
		value := now.AddDate(50, 0, 0).Add(time.Second).Format("Monday, 02-Jan-06 15:04:05 GMT")
		if got := parseRetryAfter(value, now); got != 0 {
			t.Fatalf("date over 50-year boundary = %v, want 0", got)
		}
	}
}

func TestRetryDelayTraversesWrappedAndJoinedErrors(t *testing.T) {
	var nilApple *Error
	tests := []struct {
		name string
		err  error
		want time.Duration
	}{
		{name: "nil"},
		{name: "typed nil", err: nilApple},
		{name: "unrelated", err: errors.New("unrelated")},
		{name: "zero", err: &Error{Retryable: true}},
		{name: "negative", err: &Error{RetryAfter: -time.Second}},
		{name: "non-retryable hint", err: &Error{RetryAfter: time.Minute}, want: time.Minute},
		{name: "wrapped", err: fmt.Errorf("wrapped: %w", &Error{RetryAfter: time.Minute}), want: time.Minute},
		{name: "outer maximum", err: &Error{RetryAfter: 3 * time.Minute, Err: &Error{RetryAfter: time.Minute}}, want: 3 * time.Minute},
		{name: "inner maximum", err: &Error{RetryAfter: time.Minute, Err: &Error{RetryAfter: 3 * time.Minute}}, want: 3 * time.Minute},
		{name: "negative outer", err: &Error{RetryAfter: -time.Minute, Err: &Error{RetryAfter: time.Minute}}, want: time.Minute},
		{
			name: "later nested joined maximum",
			err: fmt.Errorf("batch: %w", errors.Join(
				&Error{RetryAfter: time.Minute},
				fmt.Errorf("nested: %w", errors.Join(nilApple, errors.New("unrelated"),
					&Error{RetryAfter: -time.Second}, &Error{RetryAfter: 5 * time.Minute})),
				&Error{RetryAfter: 2 * time.Minute},
			)),
			want: 5 * time.Minute,
		},
		{name: "multiple wraps", err: fmt.Errorf("%w; %w", &Error{RetryAfter: time.Second}, &Error{RetryAfter: time.Minute}), want: time.Minute},
		{name: "saturated", err: &Error{RetryAfter: time.Duration(1<<63 - 1)}, want: time.Duration(1<<63 - 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := RetryDelay(test.err); got != test.want {
				t.Fatalf("RetryDelay() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestNonRetryableAppleErrorPreservesRetryAfter(t *testing.T) {
	cause := errors.New("connection reset")
	original := &Error{
		Op: "delete", Kind: ErrService, StatusCode: http.StatusTooManyRequests,
		ServiceCode: hmeRateLimitCodeBatch, Retryable: true, RetryAfter: 2 * time.Minute, Err: cause,
	}
	want := *original
	want.Retryable = false
	copied, ok := nonRetryableAppleError(fmt.Errorf("wrapped: %w", original)).(*Error)
	if !ok || copied == original || *copied != want {
		t.Fatalf("copied error = %#v, want %#v", copied, want)
	}
	if !original.Retryable || RetryDelay(copied) != 2*time.Minute || !errors.Is(copied, cause) || !IsRateLimited(copied) {
		t.Fatal("copy changed original retry policy or lost error metadata")
	}
}
