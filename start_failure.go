package konfluxretest

import (
	"cmp"
	"net/url"
	"regexp"
	"strings"
	"unicode"
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
		// Reason is the reported diagnostic with credentials redacted.
		Reason string
	}
)

const (
	// StartFailureAdmissionPolicy indicates that check output reports an admission policy rejection.
	StartFailureAdmissionPolicy StartFailureCategory = "admission_policy"
	// StartFailureOther indicates an explicit creation failure without evidence of a policy rejection.
	StartFailureOther StartFailureCategory = "other"
)

var (
	admissionPolicyRejection = regexp.MustCompile(`(?i)\b(?:validating)?admission[ -]?policy\b[^\r\n]*\b(denied|rejected|forbids?|forbidden)\b|\b(denied|rejected|forbidden)\b[^\r\n]*\b(?:by|because of)\s+(?:an?\s+)?(?:validating)?admission[ -]?policy\b`)
	namedCredential          = regexp.MustCompile(`(?i)\b(authorization|password|[\w-]*token|client[_-]?secret|api[_-]?key)["']?\s*[:=]\s*(?:"(?:\\.|[^"\\])*"|'[^']*'|(?:(?:bearer|basic)\s+)?[^\s,;<>"'()\x60]+)`)
	bearerCredential         = regexp.MustCompile(`(?i)\b(bearer)[ \t]+[a-z0-9._~+/=-]+`)
	githubCredential         = regexp.MustCompile(`\b(?:gh[pousr]_|github_pat_)[A-Za-z0-9_]+\b`)
)

func pipelineRunStartFailure(check checkRun, secrets ...string) *StartFailure {
	if !isKonflux(check) || check.Status != "completed" ||
		(check.Conclusion != "failure" && check.Conclusion != "startup_failure") ||
		!strings.EqualFold(strings.TrimSpace(check.Output.Title), "pipelinerun start failure") {
		return nil
	}
	text, summary := strings.TrimSpace(check.Output.Text), strings.TrimSpace(check.Output.Summary)
	reason := cmp.Or(text, summary, strings.TrimSpace(check.Output.Title))
	category := StartFailureOther
	switch {
	case admissionPolicyRejection.MatchString(text):
		category = StartFailureAdmissionPolicy
	case admissionPolicyRejection.MatchString(summary):
		category, reason = StartFailureAdmissionPolicy, summary
	}
	return &StartFailure{
		CheckID: check.ID, Job: strings.TrimPrefix(check.Name, checkPrefix),
		Cluster: checkCluster(check.DetailsURL), Category: category,
		Reason: redactStartFailureReason(reason, secrets),
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

func redactStartFailureReason(reason string, secrets []string) string {
	reason = reportedURL.ReplaceAllStringFunc(reason, diagnosticURL)
	reason = namedCredential.ReplaceAllString(reason, "${1}=[redacted]")
	reason = bearerCredential.ReplaceAllString(reason, "${1} [redacted]")
	reason = githubCredential.ReplaceAllString(reason, "[redacted]")
	for _, secret := range secrets {
		if secret != "" {
			reason = strings.ReplaceAll(reason, secret, "[redacted]")
		}
	}
	reason = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return ' '
		}
		return r
	}, reason)
	return strings.Join(strings.Fields(reason), " ")
}
