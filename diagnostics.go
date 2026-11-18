package konfluxretest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

var reportedURL = regexp.MustCompile(`(?i)\bhttps?://[^\s<>"\x60]+`)

type (
	identityFailure struct {
		category string
		cause    error
	}
	diagnosticCause struct {
		message string
		cause   error
	}
)

// Error exposes the cause independently of its summary category.
func (e *identityFailure) Error() string { return e.cause.Error() }

// Unwrap preserves typed identity failure causes.
func (e *identityFailure) Unwrap() error { return e.cause }

// Error exposes the diagnostic with request credentials removed.
func (e *diagnosticCause) Error() string { return e.message }

// Unwrap retains cancellation and network error inspection.
func (e *diagnosticCause) Unwrap() error { return e.cause }

// diagnosticURL retains the failing service and resource without URL credentials.
func diagnosticURL(address string) string {
	u, err := url.Parse(address)
	if err != nil {
		return "<invalid URL>"
	}
	if u.Opaque != "" {
		return u.Scheme + ":<opaque URL>"
	}
	u.User, u.RawQuery, u.Fragment, u.RawFragment, u.ForceQuery = nil, "", "", "", false
	return u.String()
}

// preserveCause keeps typed failures inspectable while omitting request credentials.
func preserveCause(err error, token string) error {
	message := err.Error()
	visible := err
	for {
		failure, ok := errors.AsType[*url.Error](visible)
		if !ok {
			break
		}
		visible = failure.Err
		message = visible.Error()
	}
	if strings.HasPrefix(message, "failed to parse Location header ") {
		// A malformed Location can contain credentials even when it is relative.
		message = "invalid HTTP redirect Location header"
	}
	message = redactDiagnosticMessage(message, token)
	if strings.ContainsAny(message, "\r\n\t\x1b") {
		message = fmt.Sprintf("%q", message)
	}
	return &diagnosticCause{message: message, cause: err}
}

func redactDiagnosticMessage(message, token string) string {
	message = reportedURL.ReplaceAllStringFunc(message, diagnosticURL)
	if token != "" {
		message = strings.ReplaceAll(message, token, "[redacted]")
	}
	return message
}

func konfluxHTTPFailure(response *http.Response, address, token string) error {
	const maxErrorBody = 4096
	data, readErr := io.ReadAll(io.LimitReader(response.Body, maxErrorBody+1))
	truncated := len(data) > maxErrorBody
	data = data[:min(len(data), maxErrorBody)]
	var body struct {
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	message := ""
	if json.Unmarshal(data, &body) == nil {
		message = body.Message
		if message == "" && len(body.Error) > 0 {
			if json.Unmarshal(body.Error, &message) != nil {
				var nested struct {
					Message string `json:"message"`
				}
				if json.Unmarshal(body.Error, &nested) == nil {
					message = nested.Message
				}
			}
		}
	} else if !strings.HasPrefix(strings.TrimSpace(string(data)), "<") {
		message = strings.TrimSpace(string(data))
	}
	message = redactDiagnosticMessage(message, token)
	problem := fmt.Sprintf("GET %s returned HTTP %d (%s)", diagnosticURL(address), response.StatusCode, http.StatusText(response.StatusCode))
	if message != "" {
		problem += fmt.Sprintf(": %q", message)
	}
	if truncated {
		problem += " (error response truncated)"
	}
	if readErr != nil {
		return fmt.Errorf("%s; read error response: %w", problem, preserveCause(readErr, token))
	}
	return errors.New(problem)
}

func summarizeIdentityErrors(issues []error) error {
	if len(issues) == 0 {
		return nil
	}
	counts := map[string]int{}
	for _, issue := range issues {
		category := "identity lookup failures"
		if failure, ok := errors.AsType[*identityFailure](issue); ok {
			category = failure.category
		}
		counts[category]++
	}
	var categories []string
	for category := range counts {
		categories = append(categories, category)
	}
	slices.Sort(categories)
	for i, category := range categories {
		count := counts[category]
		if count == 1 {
			category = strings.TrimSuffix(category, "s")
		}
		categories[i] = fmt.Sprintf("%d %s", count, category)
	}
	checks := "checks"
	if len(issues) == 1 {
		checks = "check"
	}
	return fmt.Errorf("PipelineRun identity verification failed for %d %s (%s):\n%w", len(issues), checks, strings.Join(categories, "; "), errors.Join(issues...))
}
