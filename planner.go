package konfluxretest

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const checkPrefix = "Red Hat Konflux / "

var (
	// ignoredKonfluxChecks is an arbitrary, tool-specific list of Konflux checks
	// deliberately excluded from build identity verification and retry planning.
	ignoredKonfluxChecks = []*regexp.Regexp{
		regexp.MustCompile(`^Red Hat Konflux / acs-conforma-dev(-[0-9]+-[0-9]+)?(?:$| / )`),
	}
	// scannerRetagJobs is a branch-specific group of Scanner V2 bundle prerequisites.
	// Observing any member requires the whole group; branches without it omit all four.
	scannerRetagJobs = []string{"retag-scanner", "retag-scanner-db", "retag-scanner-slim", "retag-scanner-db-slim"}
	// milestones lists image producers before their consumers. checks is independent.
	milestones = [][]string{
		slices.Concat([]string{"central-db-on-push", "main-on-push", "operator-on-push", "scanner-v4-on-push", "scanner-v4-db-on-push", "roxctl-on-push", "retag-collector", "retag-fact"}, scannerRetagJobs),
		{"operator-bundle-on-push"},
		{"operator-index-on-push", "create-custom-snapshot"},
	}
	// scannerMilestones contains independent image builds from the Scanner repository.
	scannerMilestones = [][]string{
		{"scanner-on-push", "scanner-db-on-push", "scanner-slim-on-push", "scanner-db-slim-on-push"},
	}
	// collectorMilestones contains the Collector build, including its architecture matrix.
	collectorMilestones = [][]string{
		{"collector-on-push"},
	}
	// factMilestones contains the independent FACT image build.
	factMilestones = [][]string{
		{"fact-on-push"},
	}
	// policyCheck recognizes supported scenarios and components across release streams.
	policyCheck = regexp.MustCompile(`^acs-conforma-(dev|prod|prod-like)(-[0-9]+-[0-9]+)?(?: / (central-db|main|operator|scanner-v4|scanner-v4-db|scanner|scanner-db|scanner-slim|scanner-db-slim|operator-bundle|roxctl|collector|fact)(-[0-9]+-[0-9]+)?)?$`)
)

type retryPlan struct {
	jobs           []string
	missingJobs    []string
	messages       []string
	waits          []string
	noRetryAction  string
	hasScopedBuild bool
}

func stagesForRepo(repo string) [][]string {
	switch {
	case strings.EqualFold(repo, "stackrox/scanner"):
		return scannerMilestones
	case strings.EqualFold(repo, "stackrox/collector"):
		return collectorMilestones
	case strings.EqualFold(repo, "stackrox/fact"):
		return factMilestones
	}
	return milestones
}

func planRetries(repo string, checks []checkRun, unavailable map[string]string) retryPlan {
	stages := stagesForRepo(repo)
	plan := retryPlan{noRetryAction: "No build retests needed."}
	latest := make(map[string]checkRun)
	for _, check := range checks {
		if !isKonflux(check) || ignoredKonfluxCheck(check.Name) {
			continue
		}
		old, exists := latest[check.Name]
		if !exists || check.ID > old.ID {
			latest[check.Name] = check
		}
	}
	names := make([]string, 0, len(latest))
	for name := range latest {
		names = append(names, name)
	}
	slices.Sort(names)
	jobs := make(map[string]checkRun)
	unknownUnfinished := false
	for _, fullName := range names {
		check := latest[fullName]
		name, prefixed := strings.CutPrefix(fullName, checkPrefix)
		if failure := check.startFailure; failure != nil {
			switch {
			case failure.PRNumber == 0:
				plan.messages = append(plan.messages, fmt.Sprintf("check %q (ID %d) reported a PipelineRun creation failure on this commit. PR or tag unknown. %s", fullName, failure.CheckID, failure.Reason))
				plan.noRetryAction = "Wait: PipelineRun creation failed for a check with unknown PR or tag."
			case check.isPRStartFailure():
				plan.messages = append(plan.messages, fmt.Sprintf("check %q (ID %d) could not start a PipelineRun for PR #%d. %s", fullName, failure.CheckID, failure.PRNumber, failure.Reason))
			default:
				plan.messages = append(plan.messages, fmt.Sprintf("check %q (ID %d) could not start a PipelineRun for PR #%d. %s No matching PR comment trigger was verified.", fullName, failure.CheckID, failure.PRNumber, failure.Reason))
				plan.noRetryAction = "Wait: No matching PR comment trigger was verified."
			}
		}
		switch {
		case prefixed && knownJob(name, stages):
			jobs[name] = check
			plan.hasScopedBuild = plan.hasScopedBuild || check.identityKnown || check.isPRStartFailure()
		case prefixed && policyCheck.MatchString(name):
			if check.Status == "completed" && check.Conclusion != "success" && check.Conclusion != "skipped" {
				plan.messages = append(plan.messages, fmt.Sprintf("policy check %q is %s; inspect its findings or upstream build failure; no policy retry emitted.", name, check.Conclusion))
			}
		default:
			plan.messages = append(plan.messages, fmt.Sprintf("WARNING: unknown Konflux check %q (%s); update the embedded job catalog before relying on its dependency order.", fullName, checkState(check)))
			if !passed(check) {
				unknownUnfinished = true
			}
		}
	}
	if check, exists := jobs["checks"]; exists {
		if retryable(check) {
			plan.jobs = append(plan.jobs, "checks")
		} else if !passed(check) {
			plan.messages = append(plan.messages, "independent checks: "+checkState(check)+"; no retry emitted.")
		}
	}
	scannerRetagsPresent := slices.ContainsFunc(scannerRetagJobs, func(name string) bool {
		_, exists := jobs[name]
		return exists
	})
	for phase, names := range stages {
		if phase > 0 && unknownUnfinished {
			plan.noRetryAction = "Wait: an unknown Konflux check has not succeeded. Inspect the warning below before retrying downstream jobs."
			plan.messages = append(plan.messages, "later milestones withheld because an unknown Konflux check has not succeeded.")
			break
		}
		var retry, blocked []string
		for _, name := range names {
			check, exists := jobs[name]
			switch {
			case !exists && !scannerRetagsPresent && slices.Contains(scannerRetagJobs, name):
				continue
			case !exists:
				plan.missingJobs = append(plan.missingJobs, name)
				blocked = append(blocked, name+" (missing)")
			case passed(check):
			case retryable(check):
				if reason := unavailable[name]; reason != "" {
					blocked = append(blocked, name+" (input unavailable)")
					plan.waits = append(plan.waits, name+": "+reason+".")
				} else {
					retry = append(retry, name)
				}
			default:
				blocked = append(blocked, name+" ("+checkState(check)+")")
			}
		}
		if len(retry) == 0 && len(blocked) == 0 {
			continue
		}
		if len(retry) > 0 {
			plan.jobs = append(plan.jobs, retry...)
		}
		if len(blocked) > 0 {
			plan.noRetryAction = fmt.Sprintf("Wait: %s.", strings.Join(blocked, ", "))
			plan.messages = append(plan.messages, fmt.Sprintf("Milestone %d is not ready: %s.", phase+1, strings.Join(blocked, ", ")))
		}
		break
	}
	if len(latest) == 0 {
		plan.noRetryAction = "Wait for build checks to appear for this commit."
		plan.messages = []string{"no Konflux checks found on this commit."}
	}
	return plan
}

func knownJob(name string, stages [][]string) bool {
	if name == "checks" {
		return true
	}
	for _, names := range stages {
		if slices.Contains(names, name) {
			return true
		}
	}
	return false
}

func isKonflux(check checkRun) bool {
	return check.App.ID == redHatKonfluxAppID
}

func passed(check checkRun) bool {
	return check.startFailure == nil && check.Status == "completed" && check.Conclusion == "success"
}

func retryable(check checkRun) bool {
	return check.isPRStartFailure() || check.startFailure == nil && check.Status == "completed" &&
		slices.Contains([]string{"failure", "timed_out", "cancelled", "stale", "startup_failure"}, check.Conclusion)
}

func checkState(check checkRun) string {
	if check.startFailure != nil {
		if check.startFailure.PRNumber > 0 && !check.isPRStartFailure() {
			return "PR comment trigger unverified"
		}
		if check.startFailure.CheckID != check.ID {
			return "newer PipelineRun creation failure has unknown PR or tag"
		}
		return "PipelineRun creation failed"
	}
	if check.Status != "completed" {
		return check.Status
	}
	if check.Conclusion == "" {
		return "completed without a conclusion"
	}
	return check.Conclusion
}

func ignoredKonfluxCheck(name string) bool {
	for _, pattern := range ignoredKonfluxChecks {
		if pattern.MatchString(name) {
			return true
		}
	}
	return false
}
