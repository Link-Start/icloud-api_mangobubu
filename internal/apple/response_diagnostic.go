package apple

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxResponseDiagnosticBytes = 1500
	maxDiagnosticInputBytes    = 64 << 10
	diagnosticRedacted         = "[REDACTED]"
)

// ResponseDiagnostic is an explicitly requested, sanitized view of an HME
// failure response. OriginalBytes counts bytes received before sanitization;
// for interrupted or limited reads it is only the number received so far.
// Format describes the original body (json/text), or omitted when its content
// could not be safely retained. Truncated JSON may be an incomplete snippet.
type ResponseDiagnostic struct {
	Body          string
	Format        string
	ServiceCode   string
	OriginalBytes int
	Truncated     bool
}

// ResponseDiagnostic returns a value copy, without exposing the raw response.
// Authentication and session operations never retain a response diagnostic.
func (e *Error) ResponseDiagnostic() (ResponseDiagnostic, bool) {
	if e == nil || !e.hasResponseDiagnostic {
		return ResponseDiagnostic{}, false
	}
	return e.responseDiagnostic, true
}

func allowsResponseDiagnostic(operation string) bool {
	switch operation {
	case "list Hide My Email aliases", "decode Hide My Email list",
		"generate Hide My Email alias", "decode generate Hide My Email alias", "decode generated Hide My Email alias",
		"reserve Hide My Email alias", "decode reserve Hide My Email alias", "decode reserved Hide My Email alias",
		"update Hide My Email forwarding target", "decode update Hide My Email forwarding target",
		"deactivate Hide My Email alias", "decode deactivate Hide My Email alias",
		"delete Hide My Email alias", "decode delete Hide My Email alias":
		return true
	default:
		return false
	}
}

func makeResponseDiagnostic(body []byte, contentType string, incomplete bool) ResponseDiagnostic {
	diagnostic := ResponseDiagnostic{OriginalBytes: len(body), Truncated: incomplete}
	omit := func(reason string) ResponseDiagnostic {
		diagnostic.Body = reason
		diagnostic.Format = "omitted"
		return diagnostic
	}
	if incomplete {
		return omit("Response body omitted: the response was not read completely.")
	}
	if code := responseServiceCode(body); diagnosticServiceCode.MatchString(code) {
		diagnostic.ServiceCode = code
	}
	if len(body) > maxDiagnosticInputBytes {
		diagnostic.Truncated = true
		return omit("Response body omitted: it exceeds the diagnostic inspection limit.")
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return omit("No response body was received.")
	}
	if !utf8.Valid(trimmed) {
		return omit("Response body omitted: it is not valid UTF-8 text.")
	}

	var value any
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err == nil {
		var trailing any
		if decoder.Decode(&trailing) != io.EOF {
			return omit("Response body omitted: it is not a complete JSON document.")
		}
		orderedSecrets := orderedDiagnosticSecrets(value, nil)
		for _, secret := range orderedSecrets {
			if diagnostic.ServiceCode == secret {
				diagnostic.ServiceCode = ""
				break
			}
		}
		value = sanitizeDiagnosticValue(value, orderedSecrets, 0, &diagnostic.Truncated)
		encoded, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return omit("Response body omitted: JSON sanitization did not complete.")
		}
		diagnostic.Body = string(encoded)
		diagnostic.Format = "json"
	} else {
		if strings.Contains(strings.ToLower(contentType), "json") || strings.ContainsRune("{[\"", rune(trimmed[0])) {
			return omit("Response body omitted: malformed JSON cannot be safely sanitized.")
		}
		if strings.Contains(strings.ToLower(contentType), "html") || diagnosticMarkup.Match(trimmed) {
			return omit("Response body omitted: HTML or XML may contain embedded credentials.")
		}
		for _, char := range string(trimmed) {
			if unicode.IsControl(char) && char != '\n' && char != '\r' && char != '\t' {
				return omit("Response body omitted: it contains non-text control characters.")
			}
		}
		diagnostic.Body = sanitizeDiagnosticText(string(trimmed), nil)
		diagnostic.Format = "text"
	}
	if len(diagnostic.Body) > maxResponseDiagnosticBytes {
		end := maxResponseDiagnosticBytes
		for !utf8.RuneStart(diagnostic.Body[end]) {
			end--
		}
		diagnostic.Body = diagnostic.Body[:end]
		diagnostic.Truncated = true
	}
	return diagnostic
}

func sensitiveDiagnosticKey(key string) bool {
	key = strings.Map(func(char rune) rune {
		if unicode.IsLetter(char) || unicode.IsDigit(char) {
			return unicode.ToLower(char)
		}
		return -1
	}, key)
	for _, marker := range []string{
		"credential", "password", "passwd", "passphrase", "secret", "token", "session", "cookie",
		"authorization", "authentication", "authattribute", "dsid", "appleid", "accountname", "accountid",
		"clientid", "anonymousid", "deviceid", "email", "privatekey", "accesskey", "apikey", "signature",
	} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	switch key {
	case "hme", "scnt", "idms", "salt", "srp", "proof", "key", "label", "note", "username", "userid":
		return true
	default:
		return false
	}
}

// Remember sensitive leaf values as well, so echoed credentials in an error
// message are removed even when the message does not name their field.
func collectDiagnosticSecrets(value any, secrets map[string]struct{}, depth int) {
	if depth > 32 {
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if sensitiveDiagnosticKey(key) {
				collectDiagnosticSecretValues(child, secrets, depth+1)
			} else {
				collectDiagnosticSecrets(child, secrets, depth+1)
			}
		}
	case []any:
		for _, child := range typed {
			collectDiagnosticSecrets(child, secrets, depth+1)
		}
	}
}

func collectDiagnosticSecretValues(value any, secrets map[string]struct{}, depth int) {
	if depth > 32 {
		return
	}
	switch typed := value.(type) {
	case string:
		if len(typed) >= 4 {
			secrets[typed] = struct{}{}
		}
	case json.Number:
		if len(typed) >= 4 {
			secrets[string(typed)] = struct{}{}
		}
	case map[string]any:
		for _, child := range typed {
			collectDiagnosticSecretValues(child, secrets, depth+1)
		}
	case []any:
		for _, child := range typed {
			collectDiagnosticSecretValues(child, secrets, depth+1)
		}
	}
}

func orderedDiagnosticSecrets(value any, inherited []string) []string {
	secrets := make(map[string]struct{}, len(inherited))
	for _, secret := range inherited {
		secrets[secret] = struct{}{}
	}
	collectDiagnosticSecrets(value, secrets, 0)
	ordered := make([]string, 0, len(secrets))
	for secret := range secrets {
		ordered = append(ordered, secret)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if len(ordered[i]) == len(ordered[j]) {
			return ordered[i] < ordered[j]
		}
		return len(ordered[i]) > len(ordered[j])
	})
	return ordered
}

func sanitizeDiagnosticValue(value any, secrets []string, depth int, truncated *bool) any {
	if depth > 32 {
		*truncated = true
		return "[OMITTED: nesting limit]"
	}
	switch typed := value.(type) {
	case map[string]any:
		cleaned := make(map[string]any, len(typed))
		for key, child := range typed {
			cleanedKey := sanitizeDiagnosticText(key, secrets)
			if sensitiveDiagnosticKey(key) {
				cleaned[cleanedKey] = diagnosticRedacted
			} else {
				cleaned[cleanedKey] = sanitizeDiagnosticValue(child, secrets, depth+1, truncated)
			}
		}
		return cleaned
	case []any:
		for index, child := range typed {
			typed[index] = sanitizeDiagnosticValue(child, secrets, depth+1, truncated)
		}
		return typed
	case string:
		trimmed := strings.TrimSpace(typed)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			// Some gateways encode another JSON document inside a message.
			// Decode it before inspecting keys, including escaped key names.
			var nested any
			decoder := json.NewDecoder(strings.NewReader(trimmed))
			decoder.UseNumber()
			if decoder.Decode(&nested) != nil {
				return "[OMITTED: embedded malformed JSON]"
			}
			var trailing any
			if decoder.Decode(&trailing) != io.EOF {
				return "[OMITTED: embedded malformed JSON]"
			}
			encoded, err := json.Marshal(sanitizeDiagnosticValue(nested, orderedDiagnosticSecrets(nested, secrets), depth+1, truncated))
			if err != nil {
				return "[OMITTED: embedded JSON sanitization failed]"
			}
			return string(encoded)
		}
		return sanitizeDiagnosticText(typed, secrets)
	default:
		return value
	}
}

var (
	diagnosticServiceCode = regexp.MustCompile(`^(?:-?[0-9]{1,12}|[A-Z][A-Z0-9_]{0,63})$`)
	diagnosticEmail       = regexp.MustCompile("(?i)[\\p{L}\\p{N}.!#$%&'*+/=?^_`{|}~-]+(?:@|%40)[\\p{L}\\p{N}](?:[\\p{L}\\p{N}.-]*[\\p{L}\\p{N}])?")
	diagnosticBearer      = regexp.MustCompile(`(?i)\b(bearer|basic)[ \t]+[^\s"'<>;,}]+`)
	diagnosticAssignment  = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9_. \t-]*)["']?[ \t]*[:=][ \t]*`)
	diagnosticIdentity    = regexp.MustCompile(`(?i)\b(dsid|apple[-_ ]?id|account[-_ ]?id|anonymous[-_ ]?id|client[-_ ]?id)[ \t]+(?:is[ \t]+)?[A-Za-z0-9_.@%+-]+`)
	diagnosticJWT         = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`)
	diagnosticMarkup      = regexp.MustCompile(`(?i)<[!?/]?[a-z][^>]*>`)
)

func sanitizeDiagnosticText(text string, secrets []string) string {
	if diagnosticMarkup.MatchString(text) || strings.Contains(text, "-----BEGIN ") {
		return "[OMITTED: embedded markup or key material]"
	}
	for _, secret := range secrets {
		text = strings.ReplaceAll(text, secret, diagnosticRedacted)
	}
	text = diagnosticEmail.ReplaceAllString(text, diagnosticRedacted)
	text = diagnosticBearer.ReplaceAllString(text, "$1 "+diagnosticRedacted)
	text = diagnosticJWT.ReplaceAllString(text, diagnosticRedacted)
	text = sanitizeDiagnosticAssignments(text)
	text = diagnosticIdentity.ReplaceAllString(text, "$1 "+diagnosticRedacted)
	return strings.Map(func(char rune) rune {
		if unicode.IsControl(char) && char != '\n' && char != '\r' && char != '\t' {
			return utf8.RuneError
		}
		return char
	}, text)
}

func sanitizeDiagnosticAssignments(text string) string {
	var cleaned strings.Builder
	start := 0
	for _, match := range diagnosticAssignment.FindAllStringSubmatchIndex(text, -1) {
		if match[0] < start || !sensitiveDiagnosticKey(text[match[2]:match[3]]) {
			continue
		}
		// Unquoted credentials can contain spaces and cookie lists can contain
		// semicolons. Omit the remainder of their line rather than a suffix.
		end := len(text)
		if newline := strings.IndexAny(text[match[1]:], "\r\n"); newline >= 0 {
			end = match[1] + newline
		}
		cleaned.WriteString(text[start:match[1]])
		cleaned.WriteString(diagnosticRedacted)
		start = end
	}
	if start == 0 {
		return text
	}
	cleaned.WriteString(text[start:])
	return cleaned.String()
}
