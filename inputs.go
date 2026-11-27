package konfluxretest

import (
	"context"
	"errors"
	"fmt"
)

const (
	missingImageReason     = "image is missing"
	missingImagePullReason = "image is missing (docker pull)"
)

type inputChecker struct {
	registry registryClient
	logs     func(context.Context, checkRun, imageRequirement) (logEvidence, error)
	pull     func(context.Context, string) (string, error)
}

// check accepts a successful optional pull as sufficient evidence for a retry.
// Failed pulls fall back to registry and log evidence; without a pull, logs take precedence.
func (c inputChecker) check(ctx context.Context, check checkRun, requirement imageRequirement) (string, []string, []CheckDiagnostic, error) {
	var warnings []string
	var diagnostics []CheckDiagnostic
	addWarning := func(err error) {
		diagnostics = append(diagnostics, CheckDiagnostic{Severity: DiagnosticWarning, Err: err})
	}
	reportUnusable := func(reason string) {
		if reason == "" {
			return
		}
		cause := errors.New(reason)
		if reason == missingImageReason || reason == missingImagePullReason {
			addWarning(classify(ErrCheckImageMissing, cause))
			return
		}
		addWarning(classify(ErrCheckInput, cause))
	}
	var pullReason string
	if c.pull != nil {
		var err error
		pullReason, err = c.pull(ctx, requirement.image())
		if err == nil && pullReason == "" {
			return "", []string{requirement.image() + ": image exists (docker pull)."}, nil, nil
		}
		if err != nil {
			cause := diagnosticError("Local image verification unavailable", err)
			if isNetworkError(err) {
				cause = networkDiagnostic("Local image verification unavailable", err)
			}
			warnings = append(warnings, cause.Error()+".")
			addWarning(classify(ErrCheckInput, cause))
		}
	}

	var evidence logEvidence
	var logErr error
	if c.logs != nil && !check.isPRStartFailure() {
		evidence, logErr = c.logs(ctx, check, requirement)
	}
	var registryErr error
	registryDiagnostic := len(diagnostics)
	if c.pull != nil || !evidence.accepted && evidence.reason == "" {
		var reason string
		reason, registryErr = c.registry.readiness(ctx, requirement)
		if registryErr == nil {
			reportUnusable(reason)
			return reason, warnings, diagnostics, nil
		}
		registryErr = classify(ErrCheckRegistry, registryErr)
		warnings = append(warnings, fmt.Sprintf("error: cannot access Quay.io to check %s: %s", requirement.image(), registryErr))
		addWarning(registryErr)
	}
	if logErr != nil {
		warnings = append(warnings, "Konflux logs unavailable: "+logErr.Error()+".")
		addWarning(classify(ErrCheckInput, logErr))
	}
	if pullReason != "" {
		if pullReason == missingImageReason || pullReason == missingImagePullReason {
			reportUnusable(pullReason)
			return pullReason, warnings, diagnostics, nil
		}
		const reason = "local verification reported an unusable image"
		reportUnusable(reason)
		return reason, warnings, diagnostics, nil
	}
	if evidence.accepted {
		warnings = append(warnings, "Input readiness inferred from the latest Konflux logs; not reverified now.")
		return "", warnings, diagnostics, nil
	}
	if evidence.reason != "" {
		warnings = append(warnings, "Input availability inferred from the latest Konflux logs; not verified now.")
		reportUnusable(evidence.reason)
		return evidence.reason, warnings, diagnostics, nil
	}
	// Without fallback evidence, the registry failure is part of the returned error.
	diagnostics[registryDiagnostic].Severity = DiagnosticError
	return "availability unknown", warnings, diagnostics, registryErr
}
