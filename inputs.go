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

// check prefers evidence from the consumer's latest run, then checks the registry
// when logs are unavailable or inconclusive. Pulling proves existence only.
func (c inputChecker) check(ctx context.Context, check checkRun, requirement imageRequirement) (string, []string, error) {
	var evidence logEvidence
	var logErr error
	if c.logs != nil {
		evidence, logErr = c.logs(ctx, check, requirement)
	}
	if evidence.accepted {
		return "", []string{"Input readiness inferred from the latest Konflux logs; not reverified now."}, nil
	}
	var warnings []string
	pulled := false
	if c.pull != nil {
		reason, err := c.pull(ctx, requirement.image())
		switch {
		case err != nil:
			warnings = append(warnings, "Local image verification unavailable: "+err.Error()+".")
		case reason != "":
			return reason, nil, nil
		default:
			pulled = true
		}
	}
	if evidence.reason != "" && !pulled {
		warnings = append(warnings, "Input availability inferred from the latest Konflux logs; not verified now.")
		return evidence.reason, warnings, nil
	}
	reason, err := c.registry.readiness(ctx, requirement)
	if err == nil {
		return reason, nil, nil
	}
	if !pulled {
		warnings = append(warnings, fmt.Sprintf("error: cannot access Quay.io to check %s: %s", requirement.image(), err))
	}
	if logErr != nil {
		warnings = append(warnings, "Konflux logs unavailable: "+logErr.Error()+".")
	}
	if pulled {
		return "image pulls successfully; attestation and source readiness remain unverified", warnings, err
	}
	return "availability unknown", warnings, err
}
