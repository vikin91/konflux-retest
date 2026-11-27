package konfluxretest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"syscall"
)

type (
	// CheckErrorKind classifies errors from CheckPR, CheckTag, and CheckDiagnostic.
	// Use errors.Is to test for each kind independently, including in joined errors.
	CheckErrorKind string

	// DiagnosticSeverity describes whether a diagnostic accompanies a returned error.
	DiagnosticSeverity string

	// CheckDiagnostic describes a check or input finding in a complete or partial result.
	CheckDiagnostic struct {
		// Job and CheckRunID are set when the finding relates to a specific job or run.
		Job        string
		CheckRunID int64
		Severity   DiagnosticSeverity
		// Err carries the finding's message, classification, and safe cause.
		Err error
	}

	classifiedError struct {
		kind  CheckErrorKind
		cause error
	}

	identityFailure struct {
		category string
		cause    error
	}
)

const (
	// ErrCheckCredential indicates a missing, unavailable, or rejected credential.
	ErrCheckCredential CheckErrorKind = "check credential failure"
	// ErrCheckPermission indicates an access denial.
	ErrCheckPermission CheckErrorKind = "check permission failure"
	// ErrCheckRateLimit indicates a rate limit or quota response.
	ErrCheckRateLimit CheckErrorKind = "check rate limit"
	// ErrCheckNetwork indicates a network or timeout failure.
	ErrCheckNetwork CheckErrorKind = "check network failure"
	// ErrCheckIdentity indicates that a PipelineRun's identity could not be verified.
	ErrCheckIdentity CheckErrorKind = "check PipelineRun identity failure"
	// ErrCheckInput indicates a failure to resolve or verify a required input.
	ErrCheckInput CheckErrorKind = "check input failure"
	// ErrCheckRegistry indicates that the registry could not complete image verification.
	ErrCheckRegistry CheckErrorKind = "check registry failure"
	// ErrCheckImageMissing indicates confirmed absence of the requested image.
	ErrCheckImageMissing CheckErrorKind = "check image missing"

	// DiagnosticWarning identifies a finding that does not contribute to the returned error.
	DiagnosticWarning DiagnosticSeverity = "warning"
	// DiagnosticError identifies a finding that contributes to the returned error.
	DiagnosticError DiagnosticSeverity = "error"
)

// Error returns the category name. Returned check errors carry the detailed message.
func (kind CheckErrorKind) Error() string { return string(kind) }

func (e *classifiedError) Error() string { return e.cause.Error() }

func (e *classifiedError) Is(target error) bool { return e.kind == target }

func (e *classifiedError) Unwrap() error { return e.cause }

func classify(kind CheckErrorKind, cause error) error {
	if kind == "" {
		return cause
	}
	return &classifiedError{kind: kind, cause: cause}
}

func checkStatusKind(status int) CheckErrorKind {
	switch status {
	case http.StatusUnauthorized:
		return ErrCheckCredential
	case http.StatusForbidden:
		return ErrCheckPermission
	case http.StatusTooManyRequests:
		return ErrCheckRateLimit
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return ErrCheckNetwork
	default:
		return ""
	}
}

// Error exposes the cause independently of its summary category.
func (e *identityFailure) Error() string { return e.cause.Error() }

// Unwrap preserves the classified causes of the lookup failure.
func (e *identityFailure) Unwrap() error { return e.cause }

// reportURL keeps ordinary HTTPS resource links usable in reports.
func reportURL(address string) string {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Opaque != "" {
		return ""
	}
	u.User, u.RawQuery, u.Fragment, u.RawFragment, u.ForceQuery = nil, "", "", "", false
	return u.String()
}

// diagnosticError keeps context sentinels inspectable without retaining upstream error data.
func diagnosticError(message string, err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s: %w", message, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%s: %w", message, context.DeadlineExceeded)
	default:
		return errors.New(message)
	}
}

// networkDiagnostic preserves safe causes without retaining request URLs or transport messages.
func networkDiagnostic(message string, err error) error {
	cause := diagnosticError(message, err)
	return classifyNetwork(cause.Error(), err)
}

func classifyNetwork(message string, err error) error {
	var cause error = errors.New(message)
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF, syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ETIMEDOUT} {
		if errors.Is(err, known) {
			cause = &diagnosticCause{message: message, cause: known}
			break
		}
	}
	if errors.Is(cause, context.Canceled) {
		return cause
	}
	return classify(ErrCheckNetwork, cause)
}

type diagnosticCause struct {
	message string
	cause   error
}

func (e *diagnosticCause) Error() string { return e.message }

func (e *diagnosticCause) Unwrap() error { return e.cause }

func isNetworkError(err error) bool {
	_, ok := errors.AsType[net.Error](err)
	return ok || errors.Is(err, io.ErrUnexpectedEOF)
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
