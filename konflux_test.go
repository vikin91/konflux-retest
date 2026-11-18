package konfluxretest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func invalidArchivePageSize(r *http.Request) *http.Response {
	size, err := strconv.Atoi(r.URL.Query().Get("page_size"))
	if err == nil && size > 5 && size < 10000 {
		return nil
	}
	body := fmt.Sprintf(`{"message":"invalid page size (%d): value must be greater than 5 and less than 10000"}`, size)
	return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(body))}
}

func TestInterpretImageLog(t *testing.T) {
	image := "quay.io/rhacs-eng/release-scanner:2.39.27-fast"
	missing := "INFO: Image not found yet: " + image + ".\n"
	tests := map[string]struct {
		log, reason string
		accepted    bool
	}{
		"should use exact missing-image lines":               {missing + missing, "Konflux reported image not found", false},
		"should not infer absence from timeout":              {"TaskRunTimeout after 5m", "", false},
		"should ignore another image":                        {strings.ReplaceAll(missing, "2.39.27", "2.39.26"), "", false},
		"should discard missing evidence when image appears": {missing + "Image " + image + " found.\n", "", false},
		"should recognize a missing attestation":             {"Attestation for " + image + "@sha256:abc not (yet) found, sleeping and retrying...", "Konflux was waiting for the build attestation", false},
		"should recognize a source mismatch":                 {"WARNING: The found image " + image + "@sha256:abc seems to be built for a different commit (old)", "Konflux reported a source revision mismatch", false},
		"should recognize failed build tasks":                {"ERROR: At least one task has failed during build of " + image + "@sha256:abc.", "Konflux reported an unsuccessful attested build", false},
		"should accept a subsequently usable image":          {missing + "Image " + image + " found.\nAccepting this image.\n", "", true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := interpretImageLog(tt.log, image)
			if got.reason != tt.reason || got.accepted != tt.accepted {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestKonfluxEvidenceBindsLatestCheckAndImage(t *testing.T) {
	tests := map[string]struct{ mismatch, want string }{
		"should read archived evidence":         {},
		"should reject an older revision":       {"revision", "latest check and PR head"},
		"should reject an older check run":      {"check", "latest check and PR head"},
		"should reject another image":           {"image", "required image"},
		"should reject another namespace":       {"namespace", "latest check and PR head"},
		"should reject another PipelineRun":     {"pipeline", "latest check and PR head"},
		"should reject redirected record paths": {"record", "record path"},
		"should reject multiple records":        {"multiple", "records=2, more_pages=false"},
		"should reject a continuation token":    {"paginated", "records=1, more_pages=true"},
		"should reject missing records":         {"missing", "records=0, more_pages=false"},
		"should reject malformed task data":     {"malformed", "invalid archived Konflux task"},
		"should reject a malformed response":    {"bad envelope", "invalid Konflux archive response"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			requirement := imageRequirement{repository: "quay.io/rhacs-eng/release-scanner", tag: "2.39.27-fast", revision: "head"}
			task := konfluxTask{}
			task.Metadata.Namespace = "rh-acs-tenant"
			task.Metadata.Labels = map[string]string{"tekton.dev/pipelineRun": "retag-scanner-run", "tekton.dev/pipelineTask": "wait-for-image", "pipelinesascode.tekton.dev/sha": "head", "pipelinesascode.tekton.dev/check-run-id": "123"}
			task.Spec.Params = []tektonParam{{Name: "IMAGE", Value: new(requirement.image())}}
			recordName := "rh-acs-tenant/results/result-id/records/task-id"
			switch tt.mismatch {
			case "revision":
				task.Metadata.Labels["pipelinesascode.tekton.dev/sha"] = "old"
			case "check":
				task.Metadata.Labels["pipelinesascode.tekton.dev/check-run-id"] = "122"
			case "namespace":
				task.Metadata.Namespace = "another"
			case "pipeline":
				task.Metadata.Labels["tekton.dev/pipelineRun"] = "another-run"
			case "image":
				task.Spec.Params[0].Value = new("quay.io/rhacs-eng/release-scanner:other")
			case "record":
				recordName = "other/results/result-id/records/task-id"
			}
			raw, err := json.Marshal(task)
			if err != nil {
				t.Fatal(err)
			}
			if tt.mismatch == "malformed" {
				raw = []byte(`{`)
			}
			record := map[string]any{"name": recordName, "data": map[string]any{"value": raw}}
			archive := map[string]any{"records": []any{record}}
			switch tt.mismatch {
			case "multiple":
				archive["records"] = []any{record, record}
			case "missing":
				archive["records"] = []any{}
			case "paginated":
				archive["next_page_token"] = "another-page"
			case "bad envelope":
				archive["records"] = "not a list"
			}
			records, err := json.Marshal(archive)
			if err != nil {
				t.Fatal(err)
			}
			logCalls := 0
			client := &konfluxClient{
				token: func(_ context.Context, host string) (string, error) {
					if host != "api.test.openshiftapps.com" {
						t.Errorf("unexpected credential target %s", host)
					}
					return "konflux-token", nil
				},
				http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.Method != "GET" || r.URL.Host != "tekton-results-tekton-results.apps.test.openshiftapps.com" || r.Header.Get("Authorization") != "Bearer konflux-token" {
						t.Errorf("unexpected request")
					}
					body := string(records)
					if strings.HasSuffix(r.URL.Path, "/logs/task-id") {
						logCalls++
						body = "INFO: Image not found yet: " + requirement.image() + ".\n"
					} else {
						if rejection := invalidArchivePageSize(r); rejection != nil {
							return rejection, nil
						}
						if !strings.Contains(r.URL.Query().Get("filter"), `data.metadata.creationTimestamp > "2026-11-13T11:34:20Z"`) || r.URL.Query().Has("page_token") {
							t.Error("unexpected archive query", r.URL.RawQuery)
						}
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
				})},
			}
			check := checkRun{ID: 123, DetailsURL: "https://konflux-ui.apps.test.openshiftapps.com/ns/rh-acs-tenant/pipelinerun/retag-scanner-run", StartedAt: time.Date(2026, 11, 13, 11, 35, 20, 0, time.UTC)}
			evidence, err := client.evidence(t.Context(), check, requirement)
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) || evidence != (logEvidence{}) || logCalls != 0 {
					t.Fatalf("evidence=%+v err=%v log calls=%d", evidence, err, logCalls)
				}
			} else if err != nil || evidence.reason != "Konflux reported image not found" || logCalls != 1 {
				t.Fatalf("evidence=%+v err=%v", evidence, err)
			}
		})
	}
}

func TestKonfluxDoesNotSendCredentialsToUnsupportedHosts(t *testing.T) {
	client := &konfluxClient{token: func(context.Context, string) (string, error) { t.Fatal("unexpected credential access"); return "", nil }}
	for _, host := range []string{"https://example.com", "http://konflux-ui.apps.test.openshiftapps.com", "https://konflux-ui.apps.test.openshiftapps.com.evil.com", "https://user@konflux-ui.apps.test.openshiftapps.com"} {
		if _, err := client.evidence(t.Context(), checkRun{DetailsURL: host + "/ns/rh-acs-tenant/pipelinerun/test"}, imageRequirement{}); err == nil {
			t.Errorf("accepted %s", host)
		}
	}
}

func TestInputCheckerUsesAvailableEvidence(t *testing.T) {
	requirement := imageRequirement{repository: "quay.io/rhacs-eng/release-scanner", tag: "2.39.27-fast"}
	tests := map[string]struct {
		evidence      logEvidence
		pull          bool
		pullReason    string
		pullError     bool
		registryReady bool
		want          string
		wantError     bool
		wantRegistry  bool
	}{
		"should wait based on consumer log evidence":                                      {evidence: logEvidence{reason: "Konflux reported image not found"}, want: "Konflux reported image not found"},
		"should permit retry after consumer accepted input":                               {evidence: logEvidence{accepted: true}},
		"should retain evidence when Docker and Quay are unavailable":                     {evidence: logEvidence{reason: "Konflux reported image not found"}, pull: true, pullError: true, want: "Konflux reported image not found", wantRegistry: true},
		"should retain confirmed local absence when Quay is unavailable":                  {pull: true, pullReason: "image is missing (docker pull)", want: "image is missing (docker pull)", wantRegistry: true},
		"should allow retries after a successful pull despite stale missing-image logs":   {evidence: logEvidence{reason: "Konflux reported image not found"}, pull: true},
		"should confirm existence without registry or log access after a successful pull": {pull: true},
		"should try Quay after Docker access fails":                                       {pull: true, pullError: true, registryReady: true, wantRegistry: true},
		"should try Quay after Docker reports a missing image":                            {pull: true, pullReason: "image is missing (docker pull)", registryReady: true, wantRegistry: true},
		"should report registry errors when Docker fails without log evidence":            {pull: true, pullError: true, want: "availability unknown", wantError: true, wantRegistry: true},
		"should remain uncertain without evidence":                                        {want: "availability unknown", wantError: true, wantRegistry: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			registryCalls, logCalls := 0, 0
			files := registryFixture(t, "https://github.com/stackrox/scanner", "scanner-sha")
			checker := inputChecker{
				logs: func(context.Context, checkRun, imageRequirement) (logEvidence, error) {
					logCalls++
					return tt.evidence, nil
				},
				registry: registryClient{baseURL: "https://quay.io", http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					registryCalls++
					status, body := http.StatusForbidden, "secret"
					if tt.registryReady {
						var ok bool
						body, ok = files[r.URL.Path]
						if !ok {
							t.Fatalf("unexpected registry path: %s", r.URL.Path)
						}
						status = http.StatusOK
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}},
			}
			if tt.pull {
				checker.pull = func(context.Context, string) (string, error) {
					if tt.pullError {
						return "", fmt.Errorf("Docker unavailable")
					}
					return tt.pullReason, nil
				}
			}
			reason, warnings, err := checker.check(t.Context(), checkRun{}, requirement)
			if (err != nil) != tt.wantError || reason != tt.want {
				t.Fatalf("reason=%q warnings=%v err=%v", reason, warnings, err)
			}
			if (registryCalls > 0) != tt.wantRegistry {
				t.Fatalf("registry calls=%d; want access=%v", registryCalls, tt.wantRegistry)
			}
			if tt.pull && !tt.pullError && tt.pullReason == "" {
				want := []string{requirement.image() + ": image exists (docker pull)."}
				if logCalls != 0 || !slices.Equal(warnings, want) {
					t.Fatalf("log calls=%d warnings=%v; want %v", logCalls, warnings, want)
				}
			}
			registryWarning := strings.Contains(strings.Join(warnings, "\n"), "cannot access Quay.io")
			if registryWarning != (tt.wantRegistry && !tt.registryReady) {
				t.Fatalf("unexpected registry warnings: %v", warnings)
			}
		})
	}
}

func TestResultFromPlan(t *testing.T) {
	tests := map[string]struct {
		plan    retryPlan
		action  Action
		message string
	}{
		"should suggest eligible retries": {plan: retryPlan{jobs: []string{"retag-scanner"}}, action: ActionRetest},
		"should wait for running checks":  {plan: retryPlan{noRetryAction: "Wait: main-on-push (in_progress)."}, action: ActionWait, message: "main-on-push (in_progress)."},
	}
	for reason, action := range map[string]Action{
		"image is missing":                 ActionInvestigateMissingImages,
		"image is missing (docker pull)":   ActionInvestigateMissingImages,
		"Konflux reported image not found": ActionInvestigateMissingImages,
		"availability unknown":             ActionWait,
		"build attestation is missing":     ActionWait,
	} {
		message := "retag-scanner: quay.io/rhacs-eng/release-scanner:2.39.27-fast: " + reason + "."
		summary := "Waiting for usable input images."
		if action == ActionInvestigateMissingImages {
			summary = "Check the missing input images listed below."
		}
		tests["should choose action for "+reason] = struct {
			plan    retryPlan
			action  Action
			message string
		}{
			plan: retryPlan{waits: []string{message}}, action: action, message: summary,
		}
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			result := resultFromPlan(Result{}, tt.plan, []string{"additional warning"})
			if result.Action != tt.action || result.Message != tt.message || !slices.Equal(result.Jobs, tt.plan.jobs) || !slices.Equal(result.Warnings, slices.Concat(tt.plan.messages, tt.plan.waits, []string{"additional warning"})) {
				t.Fatalf("unexpected result: %+v", result)
			}
		})
	}
}
