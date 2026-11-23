package konfluxretest

import (
	"context"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

type (
	// StartFailureCategory classifies a check's reported PipelineRun creation failure.
	StartFailureCategory string

	// StartFailure records a GitHub check's report about one creation attempt.
	StartFailure struct {
		CheckID int64
		Job     string
		// PRNumber identifies the exact PR associated with the failed attempt.
		PRNumber int
		// ExternalID is supplied by the GitHub App and can identify a job without identifying the triggering ref.
		ExternalID string
		// CheckStartedAt is GitHub's check timestamp.
		CheckStartedAt time.Time

		// Cluster is the cluster domain from the Konflux UI URL, or empty if unknown.
		Cluster  string
		Category StartFailureCategory
		// Reason summarizes the creation failure for the report.
		Reason string
	}
)

const (
	// StartFailureAdmissionPolicy indicates that check output reports an admission policy rejection.
	StartFailureAdmissionPolicy StartFailureCategory = "admission_policy"
	// StartFailureGitHubCheckUpdate indicates a GitHub 404 while marking a check in progress.
	StartFailureGitHubCheckUpdate StartFailureCategory = "github_check_update"
	// StartFailureOther indicates a creation failure outside the recognized categories.
	StartFailureOther StartFailureCategory = "other"
)

var (
	admissionPolicyRejection  = regexp.MustCompile(`(?i)\b(?:validating)?admission[ -]?policy\b[^\r\n]*\b(denied|rejected|forbids?|forbidden)\b|\b(denied|rejected|forbidden)\b[^\r\n]*\b(?:by|because of)\s+(?:an?\s+)?(?:validating)?admission[ -]?policy\b`)
	githubCheckUpdateNotFound = regexp.MustCompile(`cannot use the API on the provider platform to create a in_progress status:\s+PATCH https://api\.github\.com/repos/[^/\s]+/[^/\s]+/check-runs/[1-9][0-9]*: 404 Not Found\b`)
)

func (c githubClient) startFailures(ctx context.Context, checks []checkRun, repo, sourceRepo string, result Result) ([]StartFailure, []string) {
	var failedChecks []*checkRun
	jobs := make(map[string]struct{})
	stages := stagesForRepo(repo)
	for i := range checks {
		check := &checks[i]
		failure := pipelineRunStartFailure(*check)
		if failure == nil {
			continue
		}
		check.startFailure = failure
		failedChecks = append(failedChecks, check)
		if !check.matchesPRStartFailure(result) {
			continue
		}
		failure.PRNumber = result.prNumber
		if failure.Category == StartFailureAdmissionPolicy {
			check.retryPRStartFailure = true
		} else if strings.HasPrefix(check.Name, checkPrefix) && knownJob(failure.Job, stages) {
			jobs[failure.Job] = struct{}{}
		}
	}

	verified, warnings := c.verifyPRTriggers(ctx, sourceRepo, result.SHA, slices.Sorted(maps.Keys(jobs)))
	var failures []StartFailure
	for _, check := range failedChecks {
		failure := check.startFailure
		if failure.Category == StartFailureGitHubCheckUpdate && failure.PRNumber > 0 {
			check.retryPRStartFailure = slices.Contains(verified, failure.Job)
		}
		failures = append(failures, *failure)
	}
	return failures, warnings
}

func pipelineRunStartFailure(check checkRun) *StartFailure {
	if !isKonflux(check) || check.Status != "completed" ||
		(check.Conclusion != "failure" && check.Conclusion != "startup_failure") ||
		!strings.EqualFold(strings.TrimSpace(check.Output.Title), "pipelinerun start failure") {
		return nil
	}
	reason := "See the GitHub check for the PipelineRun creation failure."
	category := StartFailureOther
	if admissionPolicyRejection.MatchString(check.Output.Text) || admissionPolicyRejection.MatchString(check.Output.Summary) {
		category = StartFailureAdmissionPolicy
		reason = "An admission policy denied PipelineRun creation."
	} else if githubCheckUpdateNotFound.MatchString(check.Output.Text) || githubCheckUpdateNotFound.MatchString(check.Output.Summary) {
		category = StartFailureGitHubCheckUpdate
		reason = "GitHub returned 404 while Konflux marked the check in progress."
	}
	return &StartFailure{
		CheckID: check.ID, Job: strings.TrimPrefix(check.Name, checkPrefix),
		ExternalID: check.ExternalID, CheckStartedAt: check.StartedAt,
		Cluster: checkCluster(check.DetailsURL), Category: category,
		Reason: reason,
	}
}

func (check checkRun) matchesPRStartFailure(result Result) bool {
	if check.ID <= 0 ||
		result.prID <= 0 || result.prHeadRef == "" || check.HeadSHA != result.SHA || len(check.PullRequests) != 1 {
		return false
	}
	pr := check.PullRequests[0]
	if pr.ID != result.prID || pr.Number != result.prNumber || pr.Head.SHA != result.SHA || pr.Head.Ref != result.prHeadRef {
		return false
	}
	switch check.startFailure.Category {
	case StartFailureGitHubCheckUpdate:
		return true
	case StartFailureAdmissionPolicy:
	default:
		return false
	}
	for _, output := range []string{check.Output.Text, check.Output.Summary} {
		if strings.Contains(output, "ValidatingAdmissionPolicy 'deny-pipelinerun-create'") &&
			strings.Contains(output, "PipelineRun creation is temporarily disabled") {
			return true
		}
	}
	return false
}

func (check checkRun) isPRStartFailure() bool {
	return check.retryPRStartFailure && check.startFailure != nil && check.startFailure.CheckID == check.ID
}

func checkCluster(address string) string {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Port() != "" {
		return ""
	}
	cluster, ok := strings.CutPrefix(strings.ToLower(u.Hostname()), "konflux-ui.apps.")
	label, hasDomain := strings.CutSuffix(cluster, ".openshiftapps.com")
	if !ok || !hasDomain {
		return ""
	}
	for part := range strings.SplitSeq(label, ".") {
		if !konfluxName.MatchString(part) {
			return ""
		}
	}
	return cluster
}
