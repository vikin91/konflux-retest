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
	"strconv"
	"strings"
	"testing"
)

func TestCheckPRRetriesTemporaryAdmissionDenial(t *testing.T) {
	const (
		head        = "c7b9d50a891118421d64c6e5e41e15ae50265305"
		branch      = "alkmim/rox-37740-release-4.11-v1.6.2-bundle-v4"
		association = `{"id":5147254021,"number":23854,"head":{"sha":"` + head + `","ref":"` + branch + `"}}`
		denial      = "ValidatingAdmissionPolicy 'deny-pipelinerun-create' with binding 'deny-pipelinerun-create' denied request: PipelineRun creation is temporarily disabled"
	)
	for name, tt := range map[string]struct {
		associations, denial, newerStatus, newerConclusion string
		wantJobs                                           []string
	}{
		"should retry checks and the first milestone": {
			associations: association, denial: denial, wantJobs: []string{"checks", "main-on-push"},
		},
		"should withhold when GitHub lists no PR": {denial: denial},
		"should withhold when GitHub lists a different PR": {
			associations: strings.Replace(association, "5147254021", "5147254022", 1), denial: denial,
		},
		"should withhold when GitHub lists a stale head": {
			associations: strings.Replace(association, head, "old-head", 1), denial: denial,
		},
		"should withhold when GitHub lists multiple PRs": {associations: association + `,{"number":23855}`, denial: denial},
		"should withhold a different admission denial": {
			associations: association, denial: "ValidatingAdmissionPolicy 'require-labels' denied request: missing labels",
		},
		"should withhold once a newer build is queued": {
			associations: association, denial: denial, newerStatus: "queued", wantJobs: []string{"checks"},
		},
		"should withhold once a newer build is running": {
			associations: association, denial: denial, newerStatus: "in_progress", wantJobs: []string{"checks"},
		},
		"should withhold once a newer build succeeds": {
			associations: association, denial: denial, newerStatus: "completed", newerConclusion: "success", wantJobs: []string{"checks"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			check := func(id int64, job string) string {
				return fmt.Sprintf(`{"id":%d,"app":{"id":296509},"name":"Red Hat Konflux / %s",
					"head_sha":"%s","status":"completed","conclusion":"failure","pull_requests":[%s],
					"output":{"title":"pipelinerun start failure","text":%q}}`, id, job, head, tt.associations, tt.denial)
			}
			const mainCheckID = 135671709681
			checks := []string{check(135671710608, "checks"), check(mainCheckID, "main-on-push"), check(135671708354, "operator-bundle-on-push")}
			if tt.newerStatus != "" {
				checks = append(checks, fmt.Sprintf(`{"id":%d,"app":{"id":296509},"name":"Red Hat Konflux / main-on-push","status":%q,"conclusion":%q}`,
					mainCheckID+10, tt.newerStatus, tt.newerConclusion))
			}
			config := Config{
				VerifyRunIdentity: true,
				HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					var body string
					switch r.URL.Path {
					case "/repos/stackrox/stackrox/pulls/23854":
						body = `{"id":5147254021,"state":"open","head":{"sha":"` + head + `","ref":"` + branch + `"}}`
					case "/repos/stackrox/stackrox/commits/" + head + "/check-runs":
						body = `{"check_runs":[` + strings.Join(checks, ",") + `]}`
					default:
						t.Fatalf("unexpected request: %s", r.URL)
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
				})},
			}
			client, checker := config.clients()
			client.identity = func(_ context.Context, c checkRun, _, _ string, _ string, _ int) (runObservation, error) {
				if c.ID != mainCheckID+10 || tt.newerStatus == "" {
					t.Errorf("unexpected PipelineRun lookup for check %d", c.ID)
				}
				return runObservation{matches: true}, nil
			}
			result, err := checkPR(t.Context(), client, checker, "stackrox/stackrox", 23854)
			wantAction := ActionWait
			if len(tt.wantJobs) > 0 {
				wantAction = ActionRetest
			}
			if err != nil || result.Action != wantAction || !slices.Equal(result.Jobs, tt.wantJobs) {
				t.Fatalf("result=%+v error=%v, want action=%v jobs=%v", result, err, wantAction, tt.wantJobs)
			}
			for _, job := range result.Jobs {
				if !slices.ContainsFunc(result.Runs, func(run Run) bool {
					return run.Job == job && run.Eligible && !run.IdentityKnown && run.ImageURL == "" &&
						slices.ContainsFunc(result.StartFailures, func(failure StartFailure) bool {
							return failure.CheckID == run.ID && failure.PRNumber == 23854
						})
				}) {
					t.Fatalf("expected GitHub check evidence for job %s: %+v", job, result)
				}
			}
		})
	}
}

func TestCheckPRUsesOptionalIntegrations(t *testing.T) {
	for name, tt := range map[string]struct {
		pullReason  string
		wantLogs    bool
		failedStart bool
		action      Action
		jobs        []string
	}{
		"should investigate a missing image after fallback fails":          {pullReason: "image is missing (docker pull)", wantLogs: true, action: ActionInvestigateMissingImages},
		"should suggest a retest after a successful pull without fallback": {action: ActionRetest, jobs: []string{"retag-scanner"}},
		"should suggest a failed start when the image is available":        {failedStart: true, action: ActionRetest, jobs: []string{"retag-scanner"}},
		"should withhold a failed start when the image is missing":         {failedStart: true, pullReason: "image is missing (docker pull)", action: ActionInvestigateMissingImages},
	} {
		t.Run(name, func(t *testing.T) {

			files := requirementFiles(t)
			checks := successfulChecks()
			pr := pullRequest{ID: 42, Number: 1, State: "open"}
			pr.Head.SHA, pr.Head.Ref = "head", "topic"
			for i := range checks {
				if checks[i].Name == checkPrefix+"retag-scanner" {
					checks[i].Conclusion = "failure"
					checks[i].DetailsURL = "https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/run"
					if tt.failedStart {
						checks[i].HeadSHA = pr.Head.SHA
						checks[i].PullRequests = []pullRequest{pr}
						checks[i].Output.Title = "pipelinerun start failure"
						checks[i].Output.Text = "ValidatingAdmissionPolicy 'deny-pipelinerun-create' denied request: PipelineRun creation is temporarily disabled"
					}
				}
			}
			var tokenCalled, pullCalled bool
			config := Config{
				KonfluxToken: func(_ context.Context, host string) (string, error) {
					tokenCalled = true
					if host != "api.example.openshiftapps.com" {
						t.Errorf("unexpected host: %s", host)
					}
					return "", errors.New("no cluster login")
				},
				PullImage: func(_ context.Context, image string) (string, error) {
					pullCalled = true
					if image != "quay.io/rhacs-eng/release-scanner:2.39.27-fast" {
						t.Errorf("unexpected image: %s", image)
					}
					return tt.pullReason, nil
				},
				HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					var body []byte
					status := http.StatusOK
					switch {
					case r.URL.Host == "quay.io":
						status = http.StatusForbidden
					case r.URL.Path == "/repos/stackrox/stackrox/pulls/1":
						body, _ = json.Marshal(pr)
					case r.URL.Path == "/repos/stackrox/stackrox/commits/head/check-runs":
						body, _ = json.Marshal(map[string]any{"check_runs": checks})
					case strings.HasPrefix(r.URL.Path, "/repos/stackrox/stackrox/contents/"):
						path := strings.TrimPrefix(r.URL.Path, "/repos/stackrox/stackrox/contents/")
						if r.URL.Query().Get("ref") != "head" {
							t.Fatal("requirements are not pinned")
						}
						body, _ = json.Marshal(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString(files[path])})
					default:
						t.Fatalf("unexpected request: %s", r.URL)
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
				})},
			}
			result, err := CheckPR(t.Context(), config, "stackrox/stackrox", 1)
			if err != nil || tokenCalled != tt.wantLogs || !pullCalled || result.Action != tt.action || !slices.Equal(result.Jobs, tt.jobs) {
				t.Fatalf("result=%+v error=%v tokenCalled=%v pullCalled=%v", result, err, tokenCalled, pullCalled)
			}
		})
	}
}

func TestCheckPRRetainsUnaffectedJobsOnInputError(t *testing.T) {
	for name, contractError := range map[string]bool{"read error": false, "contract error": true} {
		t.Run(name, func(t *testing.T) {
			files := requirementFiles(t)
			pipeline := string(files[".tekton/retag-pipeline.yaml"])
			files[".tekton/retag-pipeline.yaml"] = []byte(strings.ReplaceAll(pipeline, "cc0e455bf2f74a2f5b3dac0a08a6db9dd1586848b6d949c350164aaf6821faa3", strings.Repeat("0", 64)))
			checks := successfulChecks()
			ids := map[string]int64{}
			for i := range checks {
				job := strings.TrimPrefix(checks[i].Name, checkPrefix)
				checks[i].ID = int64(i + 1)
				checks[i].DetailsURL = "https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/" + job
				ids[job] = checks[i].ID
				if job == "retag-scanner" || job == "roxctl-on-push" {
					checks[i].Conclusion = "failure"
				}
			}
			unknown := buildCheck("checks", "completed", "failure")
			unknown.ID = int64(len(checks) + 1)
			unknown.DetailsURL = "https://example.test/unsupported"
			checks = append(checks, unknown)
			config := Config{
				VerifyRunIdentity: true,
				KonfluxToken:      func(context.Context, string) (string, error) { return "fixture", nil },
				HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					body := `{"state":"open","head":{"sha":"head"}}`
					status := http.StatusOK
					switch {
					case strings.HasSuffix(r.URL.Path, "/check-runs"):
						data, _ := json.Marshal(map[string]any{"check_runs": checks})
						body = string(data)
					case strings.Contains(r.URL.Path, "/contents/"):
						if contractError {
							_, path, _ := strings.Cut(r.URL.Path, "/contents/")
							data, _ := json.Marshal(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString(files[path])})
							body = string(data)
						} else {
							status = http.StatusServiceUnavailable
						}
					case strings.Contains(r.URL.Path, "/pipelineruns/"):
						job := strings.TrimPrefix(r.URL.Path, "/apis/tekton.dev/v1/namespaces/builds/pipelineruns/")
						body = `{"metadata":{"name":"` + job + `","namespace":"builds","annotations":{
							"pipelinesascode.tekton.dev/check-run-id":"` + strconv.FormatInt(ids[job], 10) + `",
							"pipelinesascode.tekton.dev/repo-url":"https://github.com/stackrox/stackrox",
							"pipelinesascode.tekton.dev/sha":"head","pipelinesascode.tekton.dev/event-type":"pull_request",
							"pipelinesascode.tekton.dev/branch":"release-4.11","pipelinesascode.tekton.dev/pull-request":"1"}}}`
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
				})},
			}
			result, err := CheckPR(t.Context(), config, "stackrox/stackrox", 1)
			if !errors.Is(err, ErrCheckInput) || !errors.Is(err, ErrCheckIdentity) ||
				errors.Is(err, ErrUnsupportedRetagContract) != contractError ||
				result.Action != ActionRetest || !slices.Equal(result.Jobs, []string{"roxctl-on-push"}) {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			wantWarning := "HTTP 503"
			if contractError {
				wantWarning = "unsupported pinned task"
			}
			if !strings.Contains(strings.Join(result.Warnings, "\n"), wantWarning) {
				t.Fatalf("warnings=%v, want %q", result.Warnings, wantWarning)
			}
			if !slices.ContainsFunc(result.Runs, func(run Run) bool { return run.Job == "roxctl-on-push" && run.IdentityKnown && run.Eligible }) {
				t.Fatalf("lost verified retry evidence: %+v", result.Runs)
			}
		})
	}
}

func TestConfigProtectsRedirectCredentials(t *testing.T) {
	client, checker := (Config{}).clients()
	req, err := http.NewRequest(http.MethodGet, "https://storage.example/image", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.http.CheckRedirect(req, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("GitHub redirect: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	if err := checker.registry.http.CheckRedirect(req, nil); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatal("registry credential retained on redirect")
	}
	req.URL.Scheme = "http"
	if checker.registry.http.CheckRedirect(req, nil) == nil {
		t.Fatal("insecure redirect accepted")
	}
}

func TestRegistryRedirectPreservesDownloadURLs(t *testing.T) {
	requests := 0
	config := Config{HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		response := &http.Response{StatusCode: http.StatusFound, Header: make(http.Header), Body: http.NoBody}
		if requests > 1 && (r.Header.Get("Authorization") != "" || r.Header.Get("Referer") != "") {
			t.Fatal("unexpected headers on a registry redirect")
		}
		switch r.URL.Host {
		case "quay.io":
			if r.Header.Get("Authorization") != "Bearer fixture" {
				t.Fatal("initial Quay request lacks authorization")
			}
			response.Header.Set("Location", "https://storage.example/blob?signature=first")
		case "storage.example":
			if r.URL.Query().Get("signature") != "first" {
				t.Fatal("storage request lost its signature")
			}
			response.Header.Set("Location", "https://download.example/blob?signature=second")
		case "download.example":
			if r.URL.Query().Get("signature") != "second" {
				t.Fatal("download request lost its signature")
			}
			response.StatusCode = http.StatusOK
		default:
			t.Fatalf("unexpected host %q", r.URL.Host)
		}
		return response, nil
	})}}
	_, checker := config.clients()
	resp, err := checker.registry.request(t.Context(), "https://quay.io/v2/example/private/blobs/sha256:fixture", "Bearer fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if requests != 3 || resp.StatusCode != http.StatusOK {
		t.Fatalf("requests=%d, status=%d", requests, resp.StatusCode)
	}
}
