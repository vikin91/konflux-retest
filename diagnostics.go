package konfluxretest

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

type identityFailure struct {
	category string
	cause    error
}

// Error exposes the cause independently of its summary category.
func (e *identityFailure) Error() string { return e.cause.Error() }

// Unwrap preserves cancellation and deadline errors.
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
