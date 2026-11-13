package konfluxretest

import (
	"slices"
	"strings"
	"testing"
)

func buildCheck(name, status, conclusion string) checkRun {
	return checkRun{ID: 1, Name: checkPrefix + name, Status: status, Conclusion: conclusion}
}

func successfulChecks() []checkRun {
	var checks []checkRun
	for _, names := range milestones {
		for _, name := range names {
			checks = append(checks, buildCheck(name, "completed", "success"))
		}
	}
	return checks
}

func TestPlanRetries(t *testing.T) {
	tests := map[string]struct {
		changes map[string]checkRun
		remove  string
		extra   []checkRun
		want    []string
		message string
	}{
		"should restart only the earliest failed milestone": {
			changes: map[string]checkRun{
				"roxctl-on-push":          buildCheck("roxctl-on-push", "completed", "timed_out"),
				"operator-bundle-on-push": buildCheck("operator-bundle-on-push", "completed", "failure"),
				"create-custom-snapshot":  buildCheck("create-custom-snapshot", "completed", "failure"),
			}, want: []string{"roxctl-on-push"},
		},
		"should restart independent checks alongside the current milestone": {
			changes: map[string]checkRun{"operator-bundle-on-push": buildCheck("operator-bundle-on-push", "completed", "failure")},
			extra:   []checkRun{buildCheck("checks", "completed", "failure")},
			want:    []string{"checks", "operator-bundle-on-push"},
		},
		"should allow both consumers when the bundle passed": {
			changes: map[string]checkRun{
				"operator-index-on-push": buildCheck("operator-index-on-push", "completed", "failure"),
				"create-custom-snapshot": buildCheck("create-custom-snapshot", "completed", "cancelled"),
			}, want: []string{"operator-index-on-push", "create-custom-snapshot"},
		},
		"should wait for running producers": {
			changes: map[string]checkRun{
				"main-on-push":            buildCheck("main-on-push", "in_progress", ""),
				"operator-bundle-on-push": buildCheck("operator-bundle-on-push", "completed", "failure"),
			}, message: "main-on-push (in_progress)",
		},
		"should still retry failed peers while another producer is queued": {
			changes: map[string]checkRun{
				"main-on-push":   buildCheck("main-on-push", "queued", ""),
				"roxctl-on-push": buildCheck("roxctl-on-push", "completed", "failure"),
			}, want: []string{"roxctl-on-push"}, message: "main-on-push (queued)",
		},
		"should not assume missing prerequisites passed": {
			remove:  "retag-fact",
			changes: map[string]checkRun{"operator-bundle-on-push": buildCheck("operator-bundle-on-push", "completed", "failure")},
			message: "retag-fact (missing)",
		},
		"should not restart skipped jobs or bypass them": {
			changes: map[string]checkRun{
				"main-on-push":            buildCheck("main-on-push", "completed", "skipped"),
				"operator-bundle-on-push": buildCheck("operator-bundle-on-push", "completed", "failure"),
			}, message: "main-on-push (skipped)",
		},
		"should not treat neutral as a successful image build": {
			changes: map[string]checkRun{"main-on-push": buildCheck("main-on-push", "completed", "neutral")},
			message: "main-on-push (neutral)",
		},
		"should report checks requiring action without retrying": {
			extra:   []checkRun{buildCheck("checks", "completed", "action_required")},
			message: "independent checks: action_required",
		},
		"should recognize versioned policy checks without blocking the bundle": {
			changes: map[string]checkRun{"operator-bundle-on-push": buildCheck("operator-bundle-on-push", "completed", "failure")},
			extra:   []checkRun{buildCheck("acs-conforma-prod-like-5-0 / main-5-0", "completed", "failure")},
			want:    []string{"operator-bundle-on-push"}, message: "no policy retry emitted",
		},
		"should warn on new policy scenarios": {
			extra:   []checkRun{buildCheck("acs-conforma-new-policy-5-0 / main-5-0", "completed", "failure")},
			message: "WARNING: unknown Konflux check",
		},
		"should warn on new policy components": {
			extra:   []checkRun{buildCheck("acs-conforma-prod-like-5-0 / new-component-5-0", "completed", "failure")},
			message: "WARNING: unknown Konflux check",
		},
		"should withhold consumers when an unknown producer fails": {
			changes: map[string]checkRun{"operator-bundle-on-push": buildCheck("operator-bundle-on-push", "completed", "failure")},
			extra:   []checkRun{buildCheck("new-image-on-push", "completed", "failure")},
			message: "later milestones withheld",
		},
		"should still warn when an unknown check succeeds": {
			extra:   []checkRun{buildCheck("new-image-on-push", "completed", "success")},
			message: "WARNING: unknown Konflux check",
		},
		"should ignore obsolete failures when a newer run passes": {
			extra: []checkRun{{ID: 0, Name: checkPrefix + "roxctl-on-push", Status: "completed", Conclusion: "failure"}},
		},
		"should use newer failures instead of older successes": {
			extra: []checkRun{{ID: 2, Name: checkPrefix + "roxctl-on-push", Status: "completed", Conclusion: "failure"}},
			want:  []string{"roxctl-on-push"},
		},
		"should ignore GitHub Actions checks mentioning Konflux": {
			extra: []checkRun{{ID: 3, Name: "wait-for-konflux-images", Status: "completed", Conclusion: "failure"}},
		},
		"should leave successful jobs alone": {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var checks []checkRun
			for _, check := range successfulChecks() {
				job := strings.TrimPrefix(check.Name, checkPrefix)
				if job == tt.remove {
					continue
				}
				if replacement, ok := tt.changes[job]; ok {
					check = replacement
				}
				checks = append(checks, check)
			}
			checks = append(checks, tt.extra...)
			got := planRetries("stackrox/stackrox", checks, nil)
			if !slices.Equal(got.jobs, tt.want) {
				t.Fatalf("jobs = %v; want %v", got.jobs, tt.want)
			}
			if tt.message != "" && !strings.Contains(strings.Join(got.messages, "\n"), tt.message) {
				t.Errorf("messages = %v; want substring %q", got.messages, tt.message)
			}
		})
	}
}

func TestPlanRetriesNoKonfluxChecks(t *testing.T) {
	plan := planRetries("stackrox/stackrox", nil, nil)
	if len(plan.jobs) != 0 || !strings.Contains(strings.Join(plan.messages, ""), "no Konflux checks") {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestPlanRetriesRecognizesKonfluxApp(t *testing.T) {
	check := checkRun{Name: "new unprefixed job", Status: "queued"}
	check.App.Slug = "red-hat-konflux"
	plan := planRetries("stackrox/stackrox", []checkRun{check}, nil)
	if len(plan.jobs) != 0 || !strings.Contains(strings.Join(plan.messages, ""), "WARNING: unknown Konflux check") {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestPlanRetriesIgnoresConformaDev(t *testing.T) {
	for _, name := range []string{
		"acs-conforma-dev / main",
		"acs-conforma-dev-5-0 / main-5-0",
		"acs-conforma-dev-4-10 / unknown-component-4-10",
	} {
		t.Run(name, func(t *testing.T) {
			checks := successfulChecks()
			for i := range checks {
				if checks[i].Name == checkPrefix+"operator-bundle-on-push" {
					checks[i].Conclusion = "failure"
				}
			}
			checks = append(checks, buildCheck(name, "completed", "failure"))
			plan := planRetries("stackrox/stackrox", checks, nil)
			if !slices.Equal(plan.jobs, []string{"operator-bundle-on-push"}) {
				t.Fatalf("ignored dev check must not block bundle retries: %v", plan.jobs)
			}
			if strings.Contains(strings.Join(plan.messages, "\n"), "conforma-dev") {
				t.Fatalf("ignored check leaked into diagnostics: %v", plan.messages)
			}
		})
	}
}

func TestPlanRetriesDoesNotIgnoreSimilarScenarioNames(t *testing.T) {
	plan := planRetries("stackrox/stackrox", append(successfulChecks(), buildCheck("acs-conforma-development / main", "completed", "failure")), nil)
	if !strings.Contains(strings.Join(plan.messages, "\n"), "WARNING: unknown Konflux check") {
		t.Fatalf("unexpectedly ignored a different scenario: %v", plan.messages)
	}
}

func TestPlanRetriesScannerRetags(t *testing.T) {
	tests := map[string]struct {
		scannerChecks []checkRun
		want          []string
		message       string
	}{
		"should retry failed scanner retags before the bundle": {
			scannerChecks: []checkRun{
				buildCheck("retag-scanner", "completed", "failure"),
				buildCheck("retag-scanner-db", "completed", "failure"),
				buildCheck("retag-scanner-slim", "completed", "failure"),
				buildCheck("retag-scanner-db-slim", "completed", "success"),
			},
			want: []string{"retag-scanner", "retag-scanner-db", "retag-scanner-slim"},
		},
		"should retry the bundle after all scanner retags pass": {
			scannerChecks: []checkRun{
				buildCheck("retag-scanner", "completed", "success"),
				buildCheck("retag-scanner-db", "completed", "success"),
				buildCheck("retag-scanner-slim", "completed", "success"),
				buildCheck("retag-scanner-db-slim", "completed", "success"),
			}, want: []string{"operator-bundle-on-push"},
		},
		"should not require scanner retags on branches without them": {
			want: []string{"operator-bundle-on-push"},
		},
		"should wait for missing group members when one is observed": {
			scannerChecks: []checkRun{buildCheck("retag-scanner", "completed", "success")},
			message:       "retag-scanner-db (missing)",
		},
		"should wait for running scanner retags": {
			scannerChecks: []checkRun{
				buildCheck("retag-scanner", "in_progress", ""),
				buildCheck("retag-scanner-db", "completed", "success"),
				buildCheck("retag-scanner-slim", "completed", "success"),
				buildCheck("retag-scanner-db-slim", "completed", "success"),
			}, message: "retag-scanner (in_progress)",
		},
		"should not bypass skipped scanner prerequisites": {
			scannerChecks: []checkRun{
				buildCheck("retag-scanner", "completed", "success"),
				buildCheck("retag-scanner-db", "completed", "success"),
				buildCheck("retag-scanner-slim", "completed", "success"),
				buildCheck("retag-scanner-db-slim", "completed", "skipped"),
			}, message: "retag-scanner-db-slim (skipped)",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var checks []checkRun
			for _, check := range successfulChecks() {
				if slices.Contains(scannerRetagJobs, strings.TrimPrefix(check.Name, checkPrefix)) {
					continue
				}
				if check.Name == checkPrefix+"operator-bundle-on-push" {
					check.Conclusion = "failure"
				}
				checks = append(checks, check)
			}
			plan := planRetries("stackrox/stackrox", append(checks, tt.scannerChecks...), nil)
			if !slices.Equal(plan.jobs, tt.want) {
				t.Fatalf("jobs = %v; want %v", plan.jobs, tt.want)
			}
			messages := strings.Join(plan.messages, "\n")
			if !strings.Contains(messages, tt.message) {
				t.Fatalf("messages = %q; want %q", messages, tt.message)
			}
			if strings.Contains(messages, "unknown Konflux") {
				t.Fatalf("known scanner jobs must not warn: %s", messages)
			}
		})
	}
}

func TestPlanScannerRetries(t *testing.T) {
	tests := map[string]struct {
		repo    string
		checks  []checkRun
		want    []string
		message string
	}{
		"should retry failed peers while preserving successful and running builds": {
			repo: "stackrox/scanner",
			checks: []checkRun{
				buildCheck("scanner-on-push", "completed", "success"),
				buildCheck("scanner-db-on-push", "completed", "failure"),
				buildCheck("scanner-slim-on-push", "in_progress", ""),
				buildCheck("scanner-db-slim-on-push", "completed", "timed_out"),
			},
			want: []string{"scanner-db-on-push", "scanner-db-slim-on-push"}, message: "scanner-slim-on-push (in_progress)",
		},
		"should use the latest rerun and report missing Scanner checks": {
			repo: "StackRox/Scanner",
			checks: []checkRun{
				buildCheck("scanner-on-push", "completed", "failure"),
				{ID: 2, Name: checkPrefix + "scanner-on-push", Status: "completed", Conclusion: "success"},
			}, message: "scanner-db-on-push (missing)",
		},
		"should report Scanner policy failures without retrying them": {
			repo:    "stackrox/scanner",
			checks:  []checkRun{buildCheck("acs-conforma-prod-4-10 / scanner-db-slim-4-10", "completed", "failure")},
			message: "policy check",
		},
		"should not accept unknown Scanner jobs": {
			repo: "stackrox/scanner", checks: []checkRun{buildCheck("scanner-new-on-push", "completed", "failure")},
			message: "unknown Konflux check",
		},
		"should keep Scanner builds out of the StackRox catalog": {
			repo: "stackrox/stackrox", checks: []checkRun{buildCheck("scanner-on-push", "completed", "failure")},
			message: "unknown Konflux check",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			plan := planRetries(tt.repo, tt.checks, nil)
			if !slices.Equal(plan.jobs, tt.want) {
				t.Fatalf("jobs=%v; want %v", plan.jobs, tt.want)
			}
			messages := strings.Join(plan.messages, "\n")
			if !strings.Contains(messages, tt.message) {
				t.Fatalf("messages=%q; want %q", messages, tt.message)
			}
			if strings.EqualFold(tt.repo, "stackrox/scanner") && strings.Contains(messages, "operator") {
				t.Fatalf("Scanner builds should not require StackRox jobs: %s", messages)
			}
		})
	}
}
