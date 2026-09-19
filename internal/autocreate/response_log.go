package autocreate

import (
	"log/slog"

	"icloud-api/internal/apple"
)

// Only the Apple client's bounded, redacted snapshot can enter these fields.
// The summary and error chain may contain request data and are not a fallback.
func aliasCreationResponseAttrs(err error) []slog.Attr {
	upstream, diagnostic, ok := findAliasCreationResponse(err)
	if !ok {
		return nil
	}
	attrs := []slog.Attr{
		slog.String("apple_response_excerpt", diagnostic.Body),
		slog.String("apple_response_format", diagnostic.Format),
		slog.String("apple_response_service_code", diagnostic.ServiceCode),
		slog.Int("apple_response_bytes", diagnostic.OriginalBytes),
		slog.Bool("apple_response_truncated", diagnostic.Truncated),
		slog.String("apple_response_operation", safeAppleOperation(upstream.Op)),
	}
	if upstream.StatusCode > 0 {
		attrs = append(attrs, slog.Int("apple_response_http_status", upstream.StatusCode))
	}
	return attrs
}

// A directory or persistence failure may wrap an earlier reserve error. Keep
// the snapshot's own operation and HTTP status so that response is identifiable.
func findAliasCreationResponse(err error) (*apple.Error, apple.ResponseDiagnostic, bool) {
	if upstream, ok := err.(*apple.Error); ok && upstream != nil {
		if diagnostic, present := upstream.ResponseDiagnostic(); present {
			return upstream, diagnostic, true
		}
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range wrapped.Unwrap() {
			if upstream, diagnostic, ok := findAliasCreationResponse(child); ok {
				return upstream, diagnostic, true
			}
		}
	case interface{ Unwrap() error }:
		return findAliasCreationResponse(wrapped.Unwrap())
	}
	return nil, apple.ResponseDiagnostic{}, false
}
