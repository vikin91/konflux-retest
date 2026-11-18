package konfluxretest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestStrictIdentityPreservesMatchingRunsAndPartialResults(t *testing.T) {
	for name, tc := range map[string]struct {
		newStatus, newOutcome string
		unknown               bool
		startFailure          bool
		want                  []string
	}{
		"later branch does not hide tag": {"completed", "success", false, false, []string{"fact-on-push"}},
		"unknown later run withholds":    {"completed", "failure", true, false, nil},
		"later failed start withholds":   {"completed", "failure", false, true, nil},
	} {
		t.Run(name, func(t *testing.T) {
			checks := []checkRun{{ID: 10, Name: checkPrefix + "fact-on-push", Status: "completed", Conclusion: "failure"}, {ID: 11, Name: checkPrefix + "fact-on-push", Status: tc.newStatus, Conclusion: tc.newOutcome}}
			if tc.startFailure {
				checks[1].Output.Title = "pipelinerun start failure"
			}
			c := githubClient{baseURL: "https://api.github.com", verifyIdentity: true, http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Get("filter") != "all" {
					t.Fatal("strict reads must include all check runs")
				}
				data, _ := json.Marshal(map[string]any{"check_runs": checks})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
			})}, identity: func(_ context.Context, c checkRun, _, _, _ string, _ int) (runObservation, error) {
				if c.ID == 11 {
					if tc.startFailure {
						t.Fatal("creation failure reached identity lookup")
					}
					if tc.unknown {
						return runObservation{}, errors.New("archive unavailable")
					}
					return runObservation{}, nil
				}
				return runObservation{matches: true}, nil
			}}
			result, err := checkCommit(t.Context(), c, inputChecker{}, "stackrox/fact", "stackrox/fact", Result{SHA: "head", Tag: "v0.3.77"})
			if !slices.Equal(result.Jobs, tc.want) || (err != nil) != tc.unknown {
				t.Fatal(result, err)
			}
			if !tc.unknown && !tc.startFailure && (len(result.Runs) != 1 || result.Runs[0].ID != 10 || !result.Runs[0].IdentityKnown || !result.Runs[0].Eligible) {
				t.Fatal(result.Runs)
			}
			if tc.startFailure && (result.Action != ActionWait || len(result.StartFailures) != 1 || result.StartFailures[0].CheckID != 11) {
				t.Fatal(result)
			}
		})
	}
}

func TestStrictTagAssessmentExcludesDeveloperConformaFromIdentity(t *testing.T) {
	const sha = "5c3cdb95be54e2b015f78e6cb5ff9198508373cd"
	const tag = "0.2.18"
	const repo = "stackrox/fact"
	dev := Advisory{ID: 133413328301, Name: "acs-conforma-dev-4-10 / fact-4-10", Conclusion: "failure", URL: "https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/conforma-dev"}
	legacyDev := Advisory{ID: 27, Name: "acs-conforma-dev / fact", Conclusion: "cancelled", URL: "https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/legacy-dev"}
	const missingID = `PipelineRun check-run-id annotation "" does not match 17`
	for name, tc := range map[string]struct {
		includeDev   bool
		job          string
		matches      bool
		missingID    bool
		githubStatus int
		wantAction   Action
		wantMessage  string
		wantWarning  string
		wantError    string
	}{
		"empty GitHub inventory":                      {wantAction: ActionWait, wantMessage: "GitHub returned no check runs for this commit."},
		"only excluded advisories":                    {includeDev: true, wantAction: ActionWait, wantMessage: "No verified build checks match the requested tag or PR."},
		"verified build beside excluded advisories":   {includeDev: true, job: "fact-on-push", matches: true, wantAction: ActionRetest},
		"build for a different ref":                   {includeDev: true, job: "fact-on-push", wantAction: ActionWait, wantMessage: "No verified build checks match the requested tag or PR."},
		"build identity failure":                      {includeDev: true, job: "fact-on-push", missingID: true, wantAction: ActionWait, wantWarning: missingID, wantError: missingID},
		"production Conforma still requires identity": {job: "acs-conforma-prod-4-10 / fact-4-10", missingID: true, wantAction: ActionWait, wantWarning: missingID, wantError: missingID},
		"verified production Conforma is not a build": {job: "acs-conforma-prod-4-10 / fact-4-10", matches: true, wantAction: ActionWait, wantMessage: "No verified build checks match the requested tag or PR.", wantWarning: "policy check"},
		"unknown Konflux job still requires identity": {job: "new-job", missingID: true, wantAction: ActionWait, wantWarning: "unknown Konflux check", wantError: missingID},
		"GitHub lookup failure":                       {githubStatus: http.StatusServiceUnavailable, wantAction: ActionWait, wantWarning: "HTTP 503", wantError: "HTTP 503"},
	} {
		t.Run(name, func(t *testing.T) {
			var checks []checkRun
			var wantAdvisories []Advisory
			if tc.includeDev {
				for _, a := range []Advisory{dev, legacyDev} {
					checks = append(checks, checkRun{ID: a.ID, Name: checkPrefix + a.Name, Status: "completed", Conclusion: a.Conclusion, DetailsURL: a.URL})
				}
				wantAdvisories = append(wantAdvisories, legacyDev, dev)
			}
			if tc.job != "" {
				check := checkRun{ID: 17, Name: checkPrefix + tc.job, Status: "completed", Conclusion: "failure", DetailsURL: "https://example.com/run"}
				checks = append(checks, check)
				if strings.HasPrefix(tc.job, "acs-conforma-") {
					wantAdvisories = append(wantAdvisories, Advisory{ID: check.ID, Name: tc.job, Conclusion: check.Conclusion, URL: check.DetailsURL})
				}
			}
			identityCalls := 0
			c := githubClient{baseURL: "https://api.github.com", verifyIdentity: true, http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				status := http.StatusOK
				var body []byte
				switch r.URL.Path {
				case "/repos/" + repo + "/commits/refs/tags/" + tag:
					body, _ = json.Marshal(map[string]string{"sha": sha})
				case "/repos/" + repo + "/commits/" + sha + "/check-runs":
					if tc.githubStatus != 0 {
						status = tc.githubStatus
					}
					body, _ = json.Marshal(map[string]any{"check_runs": checks})
				default:
					t.Fatalf("unexpected GitHub request: %s", r.URL)
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})}, identity: func(_ context.Context, check checkRun, gotRepo, gotSHA, gotTag string, pr int) (runObservation, error) {
				identityCalls++
				if check.ID != 17 || gotRepo != repo || gotSHA != sha || gotTag != tag || pr != 0 {
					t.Fatalf("unexpected identity lookup for %q (run %d) at %s %s tag %q PR %d", check.Name, check.ID, gotRepo, gotSHA, gotTag, pr)
				}
				const prefix = "pipelinesascode.tekton.dev/"
				annotations := map[string]string{
					prefix + "sha": sha, prefix + "repo-url": "https://github.com/" + repo,
					prefix + "event-type": "push", prefix + "branch": "refs/heads/main",
				}
				if !tc.missingID {
					annotations[prefix+"check-run-id"] = "17"
				}
				if tc.matches {
					annotations[prefix+"branch"] = "refs/tags/" + tag
				}
				data, _ := json.Marshal(map[string]any{"metadata": map[string]any{"name": "run", "namespace": "builds", "annotations": annotations}})
				matches, err := matchRunIdentity(data, "builds", "run", check.ID, gotRepo, gotSHA, gotTag, pr)
				return runObservation{matches: matches}, err
			}}
			result, err := checkTag(t.Context(), c, inputChecker{}, repo, tag)
			if (err != nil) != (tc.wantError != "") || err != nil && !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("assessment error = %v, want %q", err, tc.wantError)
			}
			if (identityCalls == 1) != (tc.job != "") || identityCalls > 1 {
				t.Fatalf("unexpected identity lookup count: %d", identityCalls)
			}
			if !slices.Equal(result.Advisories, wantAdvisories) {
				t.Fatalf("advisories = %+v, want %+v", result.Advisories, wantAdvisories)
			}
			if result.Action != tc.wantAction || tc.wantMessage != "" && result.Message != tc.wantMessage || !strings.Contains(strings.Join(result.Warnings, "\n"), tc.wantWarning) {
				t.Fatalf("unexpected assessment: %+v", result)
			}
			switch {
			case tc.wantAction == ActionRetest:
				if !slices.Equal(result.Jobs, []string{tc.job}) || len(result.Runs) != 1 || !result.Runs[0].IdentityKnown || !result.Runs[0].Eligible || len(result.Warnings) != 0 {
					t.Fatalf("verified retry should remain eligible: %+v", result)
				}
			case tc.missingID:
				if len(result.Jobs) != 0 || len(result.Runs) != 1 || result.Runs[0].IdentityKnown || result.Runs[0].Eligible || result.Runs[0].Status != "unknown" {
					t.Fatalf("failed verification must withhold retries: %+v", result)
				}
			case len(result.Jobs) != 0:
				t.Fatalf("unexpected retries: %v", result.Jobs)
			}
		})
	}
}
