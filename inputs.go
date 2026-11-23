package konfluxretest

import (
	"context"
	"fmt"
)

type inputChecker struct {
	registry registryClient
	logs     func(context.Context, checkRun, imageRequirement) (logEvidence, error)
	pull     func(context.Context, string) (string, error)
}

// check accepts a successful optional pull as sufficient evidence for a retry.
// Failed pulls fall back to registry and log evidence; without a pull, logs take precedence.
func (c inputChecker) check(ctx context.Context, check checkRun, requirement imageRequirement) (string, []string, error) {
	var warnings []string
	var pullReason string
	if c.pull != nil {
		var err error
		pullReason, err = c.pull(ctx, requirement.image())
		if err == nil && pullReason == "" {
			return "", []string{requirement.image() + ": image exists (docker pull)."}, nil
		}
		if err != nil {
			warnings = append(warnings, diagnosticError("Local image verification unavailable", err).Error()+".")
		}
	}

	var evidence logEvidence
	var logErr error
	if c.logs != nil && !check.isPRStartFailure() {
		evidence, logErr = c.logs(ctx, check, requirement)
	}
	var registryErr error
	if c.pull != nil || !evidence.accepted && evidence.reason == "" {
		var reason string
		reason, registryErr = c.registry.readiness(ctx, requirement)
		if registryErr == nil {
			return reason, warnings, nil
		}
		warnings = append(warnings, fmt.Sprintf("error: cannot access Quay.io to check %s: %s", requirement.image(), registryErr))
	}
	if logErr != nil {
		warnings = append(warnings, "Konflux logs unavailable: "+logErr.Error()+".")
	}
	if pullReason != "" {
		if pullReason == "image is missing" || pullReason == "image is missing (docker pull)" {
			return pullReason, warnings, nil
		}
		return "local verification reported an unusable image", warnings, nil
	}
	if evidence.accepted {
		warnings = append(warnings, "Input readiness inferred from the latest Konflux logs; not reverified now.")
		return "", warnings, nil
	}
	if evidence.reason != "" {
		warnings = append(warnings, "Input availability inferred from the latest Konflux logs; not verified now.")
		return evidence.reason, warnings, nil
	}
	return "availability unknown", warnings, registryErr
}
