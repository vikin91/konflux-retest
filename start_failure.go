package konfluxretest

import (
	"net/url"
	"regexp"
	"strings"
)

type (
	// StartFailureCategory classifies a check's reported PipelineRun creation failure.
	StartFailureCategory string

	// StartFailure records a GitHub check's report about one creation attempt.
	StartFailure struct {
		CheckID int64
		Job     string
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
		Cluster: checkCluster(check.DetailsURL), Category: category,
		Reason: reason,
	}
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
