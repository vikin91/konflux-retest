package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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
		"should reject redirected record paths": {"record", "record path"},
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
			case "image":
				task.Spec.Params[0].Value = new("quay.io/rhacs-eng/release-scanner:other")
			case "record":
				recordName = "other/results/result-id/records/task-id"
			}
			raw, err := json.Marshal(task)
			if err != nil {
				t.Fatal(err)
			}
			records, err := json.Marshal(map[string]any{"records": []any{map[string]any{"name": recordName, "data": map[string]any{"value": raw}}}})
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
					} else if r.URL.Query().Get("page_size") != "5" || !strings.Contains(r.URL.Query().Get("filter"), `data.metadata.creationTimestamp > "2026-11-13T11:34:20Z"`) {
						t.Error("archive query is not bounded by check time")
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
				})},
			}
			check := checkRun{ID: 123, DetailsURL: "https://konflux-ui.apps.test.openshiftapps.com/ns/rh-acs-tenant/pipelinerun/retag-scanner-run", StartedAt: time.Date(2026, 11, 13, 11, 35, 20, 0, time.UTC)}
			evidence, err := client.evidence(t.Context(), check, requirement)
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) || logCalls != 0 {
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
		evidence   logEvidence
		pull       bool
		pullReason string
		pullError  bool
		want       string
		wantError  bool
	}{
		"should wait based on consumer log evidence":                     {evidence: logEvidence{reason: "Konflux reported image not found"}, want: "Konflux reported image not found"},
		"should permit retry after consumer accepted input":              {evidence: logEvidence{accepted: true}},
		"should retain evidence when Docker is unavailable":              {evidence: logEvidence{reason: "Konflux reported image not found"}, pull: true, pullError: true, want: "Konflux reported image not found"},
		"should use a confirmed local absence":                           {pull: true, pullReason: "image is missing (docker pull)", want: "image is missing (docker pull)"},
		"should not equate a successful pull with attestation readiness": {evidence: logEvidence{reason: "Konflux reported image not found"}, pull: true, want: "image pulls successfully", wantError: true},
		"should remain uncertain without evidence":                       {want: "availability unknown", wantError: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			registryCalls := 0
			checker := inputChecker{logs: func(context.Context, checkRun, imageRequirement) (logEvidence, error) { return tt.evidence, nil }, registry: registryClient{baseURL: "https://quay.io", http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				registryCalls++
				return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("secret"))}, nil
			})}}}
			if tt.pull {
				checker.pull = func(context.Context, string) (string, error) {
					if tt.pullError {
						return "", fmt.Errorf("Docker unavailable")
					}
					return tt.pullReason, nil
				}
			}
			reason, warnings, err := checker.check(t.Context(), checkRun{}, requirement)
			if (err != nil) != tt.wantError || (tt.want == "" && reason != "") || !strings.Contains(reason, tt.want) {
				t.Fatalf("reason=%q warnings=%v err=%v", reason, warnings, err)
			}
			if !tt.wantError && registryCalls != 0 {
				t.Error("conclusive evidence should avoid registry access")
			}
		})
	}
}

func TestDockerPullFailureDistinguishesMissingFromUnknown(t *testing.T) {
	tests := map[string]struct {
		output  string
		missing bool
	}{
		"should recognize a missing manifest":                  {"Error response from daemon: manifest unknown", true},
		"should not treat denied access as missing":            {"pull access denied, repository does not exist or may require docker login", false},
		"should prioritize authentication over not-found text": {"unauthorized: manifest unknown", false},
		"should not infer absence from a daemon error":         {"Cannot connect to the Docker daemon", false},
		"should not infer absence from network failure":        {"TLS handshake timeout", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			reason, err := dockerPullFailure([]byte(tt.output))
			if (reason != "") != tt.missing || (err == nil) != tt.missing {
				t.Fatalf("reason=%q error=%v", reason, err)
			}
		})
	}
}

func TestPrintPlanFormat(t *testing.T) {
	tests := map[string]struct {
		plan retryPlan
		want string
	}{
		"should print a compact retest block": {retryPlan{jobs: []string{"retag-scanner"}}, "Next action: Retest\n/konflux-retest retag-scanner\n\nWarnings:\nNone.\n"},
		"should print a compact wait block":   {retryPlan{noRetryAction: "Wait: main-on-push (in_progress).", messages: []string{"Milestone 1 is not ready: main-on-push (in_progress)."}}, "Next action: Wait\nmain-on-push (in_progress).\n\nWarnings:\nMilestone 1 is not ready: main-on-push (in_progress).\n"},
	}
	for reason, action := range map[string]string{
		"image is missing":                 "Investigate missing images",
		"image is missing (docker pull)":   "Investigate missing images",
		"Konflux reported image not found": "Investigate missing images",
		"availability unknown":             "Wait",
		"build attestation is missing":     "Wait",
	} {
		message := "retag-scanner: quay.io/rhacs-eng/release-scanner:2.39.27-fast: " + reason + "."
		summary := "Waiting for usable input images."
		if action == "Investigate missing images" {
			summary = "Check the missing input images listed below."
		}
		tests["should choose action for "+reason] = struct {
			plan retryPlan
			want string
		}{
			retryPlan{waits: []string{message}},
			"Next action: " + action + "\n" + summary + "\n\nWarnings:\n" + message + "\n",
		}
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var out strings.Builder
			printPlan(&out, tt.plan, nil)
			if out.String() != tt.want {
				t.Fatalf("got %q; want %q", out.String(), tt.want)
			}
		})
	}
}
