package konfluxretest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestCheckPRMissingTriggers(t *testing.T) {
	const (
		base = "/repos/stackrox/stackrox"
		fork = "/repos/contributor/stackrox"
		sha  = "current-head"
	)
	tests := map[string]struct {
		disabled, unavailable, malformed, retagMissing, identityUnknown bool
		want                                                            []string
	}{
		"should verify missing builds at the fork head":          {want: []string{"main-on-push", "scanner-v4-on-push", "retag-collector"}},
		"should withhold a missing retag with unavailable input": {retagMissing: true, want: []string{"main-on-push", "scanner-v4-on-push"}},
		"should withhold a check with unresolved identity":       {identityUnknown: true, want: []string{"scanner-v4-on-push", "retag-collector"}},
		"should withhold hints after a failed check lookup":      {unavailable: true},
		"should withhold hints without a real check inventory":   {malformed: true},
		"should preserve library defaults":                       {disabled: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			checks := slices.DeleteFunc(successfulChecks(), func(check checkRun) bool {
				return check.Name == checkPrefix+"scanner-v4-on-push" || check.Name == checkPrefix+"retag-collector" ||
					check.Name == checkPrefix+"operator-bundle-on-push" || (!tt.identityUnknown && check.Name == checkPrefix+"main-on-push")
			})
			inventory, err := json.Marshal(map[string]any{"check_runs": checks})
			if err != nil {
				t.Fatal(err)
			}
			files := requirementFiles(t)
			for _, component := range []string{"main", "scanner-v4", "operator-bundle"} {
				job := component + "-on-push"
				files[".tekton/"+component+"-build.yaml"] = fmt.Appendf(nil, "apiVersion: tekton.dev/v1\nkind: PipelineRun\nmetadata:\n  name: %s\n  annotations:\n    pipelinesascode.tekton.dev/on-comment: '/konflux-retest %s'\n", job, job)
			}
			client := githubClient{
				baseURL: "https://api.github.com", verifyIdentity: true, verifyMissingPRTriggers: !tt.disabled,
				identity: func(_ context.Context, check checkRun, _ string, _ string, _ string, _ int) (runObservation, error) {
					if tt.identityUnknown && check.Name == checkPrefix+"main-on-push" {
						return runObservation{}, errors.New("run lookup failed")
					}
					return runObservation{matches: true}, nil
				},
				http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.Method != http.MethodGet {
						t.Fatalf("analyzer sent %s", r.Method)
					}
					status := http.StatusOK
					var body string
					switch r.URL.Path {
					case base + "/pulls/42":
						body = `{"state":"open","head":{"sha":"` + sha + `","repo":{"full_name":"contributor/stackrox"}}}`
					case base + "/commits/" + sha + "/check-runs":
						body = string(inventory)
						if tt.unavailable {
							status = http.StatusForbidden
						}
						if tt.malformed {
							body = `{}`
						}
					default:
						file, inFork := strings.CutPrefix(r.URL.Path, fork+"/contents/")
						if !inFork || r.URL.Query().Get("ref") != sha || tt.disabled || tt.unavailable || tt.malformed || file == ".tekton/operator-bundle-build.yaml" ||
							(tt.identityUnknown && file == ".tekton/main-build.yaml") {
							t.Fatalf("unexpected manifest request: %s", r.URL)
						}
						data, ok := files[file]
						if !ok {
							t.Fatalf("unexpected file request: %s", file)
						}
						body = fmt.Sprintf(`{"encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString(data))
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
				})},
			}
			checker := inputChecker{
				pull: func(_ context.Context, image string) (string, error) {
					if image != "quay.io/rhacs-eng/release-collector:3.24.17-fast" {
						t.Fatalf("unexpected input image: %s", image)
					}
					if tt.retagMissing {
						return "image is missing", nil
					}
					return "", nil
				},
				registry: registryClient{baseURL: "https://quay.io", http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(""))}, nil
				})}},
			}
			result, err := checkPR(t.Context(), client, checker, "stackrox/stackrox", 42)
			if (err != nil) != (tt.unavailable || tt.malformed || tt.identityUnknown) || !slices.Equal(result.MissingPRJobs, tt.want) || len(result.Jobs) != 0 || result.Action != ActionWait {
				t.Fatalf("result=%+v err=%v; want missing %v", result, err, tt.want)
			}
			if tt.retagMissing && !strings.Contains(strings.Join(result.Warnings, "\n"), "cannot start missing build retag-collector") {
				t.Fatalf("missing retag input diagnostic: %v", result.Warnings)
			}
		})
	}
}

func TestMatchesPRTrigger(t *testing.T) {
	const header = "apiVersion: tekton.dev/v1\nkind: PipelineRun\nmetadata:\n  name: main-on-push\n  annotations:\n"
	const configured = "    pipelinesascode.tekton.dev/on-comment: '^/konflux-retest main-on-push$'\n"
	tests := map[string]struct {
		manifest      string
		want, wantErr bool
	}{
		"should accept a matching comment":           {header + configured + "    pipelinesascode.tekton.dev/on-label: '[]'\n", true, false},
		"should accept array-valued pipeline params": {header + configured + "spec:\n  params:\n  - name: tags\n    value: [latest, dev]\n", true, false},
		"should reject a different job name":         {strings.Replace(header, "main-on-push", "different-job", 1) + configured, false, false},
		"should reject a different command":          {header + strings.Replace(configured, "main-on-push", "different-job", 1), false, false},
		"should withhold path-filtered triggers":     {header + configured + "    pipelinesascode.tekton.dev/on-path-changed: '[docs/**]'\n", false, false},
		"should withhold label-filtered triggers":    {header + configured + "    pipelinesascode.tekton.dev/on-label: '[build]'\n", false, false},
		"should reject an invalid trigger pattern":   {header + "    pipelinesascode.tekton.dev/on-comment: '['\n", false, true},
		"should reject a broken manifest":            {header + configured + "---\n[", false, true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := matchesPRTrigger([]byte(tt.manifest), "main-on-push")
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Fatalf("got %t, %v; want match=%t error=%t", got, err, tt.want, tt.wantErr)
			}
		})
	}
}
