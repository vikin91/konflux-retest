package konfluxretest

import (
	"net/url"
	"regexp"
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
		// PRNumber identifies the exact PR associated with a temporary admission denial.
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
	// StartFailureOther indicates an explicit creation failure without evidence of a policy rejection.
	StartFailureOther StartFailureCategory = "other"
)

var admissionPolicyRejection = regexp.MustCompile(`(?i)\b(?:validating)?admission[ -]?policy\b[^\r\n]*\b(denied|rejected|forbids?|forbidden)\b|\b(denied|rejected|forbidden)\b[^\r\n]*\b(?:by|because of)\s+(?:an?\s+)?(?:validating)?admission[ -]?policy\b`)

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
	}
	return &StartFailure{
		CheckID: check.ID, Job: strings.TrimPrefix(check.Name, checkPrefix),
		ExternalID: check.ExternalID, CheckStartedAt: check.StartedAt,
		Cluster: checkCluster(check.DetailsURL), Category: category,
		Reason: reason,
	}
}

func (check checkRun) matchesPRAdmissionDenial(result Result) bool {
	if check.startFailure == nil || check.startFailure.Category != StartFailureAdmissionPolicy || check.ID <= 0 ||
		result.prID <= 0 || result.prHeadRef == "" || check.HeadSHA != result.SHA || len(check.PullRequests) != 1 {
		return false
	}
	pr := check.PullRequests[0]
	if pr.ID != result.prID || pr.Number != result.prNumber || pr.Head.SHA != result.SHA || pr.Head.Ref != result.prHeadRef {
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
	return check.startFailure != nil && check.startFailure.CheckID == check.ID && check.startFailure.PRNumber > 0
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
