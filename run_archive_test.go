package konfluxretest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestCheckPRDisambiguatesArchivedPipelineRuns(t *testing.T) {
	const (
		sha         = "1c511896804e8256bb06f2fa8e9ac1541109281e"
		scannerID   = 131657651084
		namespace   = "rh-acs-tenant"
		scannerName = "scanner-v4-on-push-cqhl7"
		details     = "https://konflux-ui.apps.example.openshiftapps.com/ns/" + namespace + "/pipelinerun/"
	)
	data, err := os.ReadFile("testdata/archive-reused-pipelinerun-name.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture []json.RawMessage
	if err := json.Unmarshal(data, &fixture); err != nil || len(fixture) != 2 {
		t.Fatalf("expected two archived fixture runs, got %d: %v", len(fixture), err)
	}
	old, current := string(fixture[0]), string(fixture[1])
	replace := func(value, from, to string) string {
		t.Helper()
		if !strings.Contains(value, from) {
			t.Fatalf("fixture lacks %q", from)
		}
		return strings.Replace(value, from, to, 1)
	}
	for name, tt := range map[string]struct {
		records     []string
		issue       string
		warningOnly bool
	}{
		"reused name":                    {records: []string{old, current}},
		"reused name in API order":       {records: []string{current, old}},
		"two claims on the check":        {records: []string{replace(old, "32501188263", "131657651084"), current}, issue: "multiple archived PipelineRuns claim the check ID"},
		"repeated UID":                   {records: []string{replace(old, "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"), current}, issue: "missing or repeated archived PipelineRun UID"},
		"unknown check ID":               {records: []string{replace(old, "32501188263", ""), current}, issue: "missing or invalid archived check-run-id annotation"},
		"unknown UID":                    {records: []string{replace(old, "11111111-1111-4111-8111-111111111111", ""), current}, issue: "missing or repeated archived PipelineRun UID"},
		"no record claims the check":     {records: []string{old, replace(current, "131657651084", "131657651085")}, issue: "no archived PipelineRun matches the check ID"},
		"matching check on wrong SHA":    {records: []string{old, replace(current, sha, "different-sha")}, issue: "SHA annotation does not match"},
		"matching check on wrong repo":   {records: []string{old, replace(current, "stackrox/stackrox", "example/fork")}, issue: "repository annotation does not match"},
		"matching check with unknown PR": {records: []string{old, replace(current, "23856", "")}, issue: "PR event identity unavailable", warningOnly: true},
	} {
		t.Run(name, func(t *testing.T) {
			scanner := buildCheck("scanner-v4-on-push", "completed", "failure")
			scanner.ID, scanner.DetailsURL = scannerID, details+scannerName
			checks := buildCheck("checks", "completed", "failure")
			checks.DetailsURL = details + "check-fixture"
			records := make([]archivedRun, len(tt.records))
			for i, record := range tt.records {
				records[i].Data.Value = []byte(record)
			}
			config := Config{
				VerifyRunIdentity: true,
				KonfluxToken:      func(context.Context, string) (string, error) { return "fixture", nil },
				HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					status := http.StatusOK
					var body any
					switch {
					case strings.HasSuffix(r.URL.Path, "/pulls/23856"):
						body = json.RawMessage(`{"state":"open","head":{"sha":"` + sha + `"}}`)
					case strings.HasSuffix(r.URL.Path, "/check-runs"):
						body = map[string]any{"check_runs": []checkRun{scanner, checks}}
					case strings.HasSuffix(r.URL.Path, "/pipelineruns/"+scannerName):
						status = http.StatusServiceUnavailable
					case strings.HasSuffix(r.URL.Path, "/pipelineruns/check-fixture"):
						body = json.RawMessage(`{"metadata":{"name":"check-fixture","namespace":"rh-acs-tenant","annotations":{
							"pipelinesascode.tekton.dev/check-run-id":"1","pipelinesascode.tekton.dev/sha":"` + sha + `",
							"pipelinesascode.tekton.dev/repo-url":"https://github.com/stackrox/stackrox","pipelinesascode.tekton.dev/event-type":"pull_request",
							"pipelinesascode.tekton.dev/branch":"release-4.11","pipelinesascode.tekton.dev/pull-request":"23856"}}}`)
					case strings.HasSuffix(r.URL.Path, "/results/-/records"):
						body = map[string]any{"records": records}
					default:
						t.Errorf("unexpected request: %s", r.URL)
						status = http.StatusNotFound
					}
					response, err := json.Marshal(body)
					if err != nil {
						return nil, err
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(response)))}, nil
				})},
			}
			result, err := CheckPR(t.Context(), config, "stackrox/stackrox", 23856)
			wantJobs := []string{"checks"}
			if tt.issue == "" {
				wantJobs = append(wantJobs, "scanner-v4-on-push")
			}
			wantError := tt.issue != "" && !tt.warningOnly
			if (err != nil) != wantError || !slices.Equal(result.Jobs, wantJobs) || len(result.Runs) != 2 {
				t.Fatalf("result=%+v error=%v, want jobs %v", result, err, wantJobs)
			}
			if tt.issue != "" && !strings.Contains(strings.Join(result.Warnings, "\n"), tt.issue) {
				t.Fatalf("warnings=%v, want %q", result.Warnings, tt.issue)
			}
			run := result.Runs[1]
			if run.ID != scannerID || run.IdentityKnown != (tt.issue == "") || run.Eligible != run.IdentityKnown ||
				run.IdentityKnown && run.ImageURL != "quay.io/example/scanner-v4:fixture" ||
				!run.IdentityKnown && run.ImageURL != "" {
				t.Fatalf("unexpected archive evidence: %+v", run)
			}
		})
	}
}
