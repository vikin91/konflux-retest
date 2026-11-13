package konfluxretest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestCheckPRPaginatesAndUsesCurrentHead(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("unexpected mutation: %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing authentication")
		}
		switch r.URL.Path {
		case "/repos/stackrox/stackrox/pulls/23956":
			fmt.Fprint(w, `{"state":"open","head":{"sha":"current-head"}}`)
		case "/repos/stackrox/stackrox/commits/current-head/check-runs":
			pages++
			if r.URL.Query().Get("filter") != "latest" || r.URL.Query().Get("per_page") != "100" {
				t.Error("missing pagination parameters")
			}
			checks := []checkRun{}
			if r.URL.Query().Get("page") == "1" {
				for i := range 100 {
					checks = append(checks, checkRun{ID: int64(i + 1), Name: fmt.Sprintf("unrelated %d", i), Status: "completed", Conclusion: "success"})
				}
			} else if r.URL.Query().Get("page") == "2" {
				checks = successfulChecks()
				for i := range checks {
					if checks[i].Name == checkPrefix+"roxctl-on-push" || checks[i].Name == checkPrefix+"operator-bundle-on-push" {
						checks[i].Conclusion = "failure"
					}
				}
				checks = append(checks, buildCheck("acs-conforma-prod-like-5-0 / main-5-0", "completed", "failure"))
			} else {
				t.Errorf("unexpected page: %s", r.URL.Query().Get("page"))
			}
			json.NewEncoder(w).Encode(map[string]any{"check_runs": checks})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := githubClient{baseURL: server.URL, token: "test-token", http: server.Client()}
	result, err := checkPR(t.Context(), client, inputChecker{}, "stackrox/stackrox", 23956)
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://github.com/stackrox/stackrox/pull/23956" || result.SHA != "current-head" || result.Action != ActionRetest || !reflect.DeepEqual(result.Jobs, []string{"roxctl-on-push"}) {
		t.Fatalf("unexpected result: %+v", result)
	}
	warnings := strings.Join(result.Warnings, "\n")
	if pages != 2 {
		t.Errorf("read %d pages; want 2", pages)
	}
	if !strings.Contains(warnings, "policy check") {
		t.Fatalf("missing diagnostics: %s", warnings)
	}
	if strings.Contains(warnings, "test-token") {
		t.Fatal("token exposed")
	}
}

func TestCheckPRHandlesClosedPRAndAPIErrors(t *testing.T) {
	tests := map[string]struct {
		status    int
		body      string
		wantError bool
	}{
		"should skip closed PRs": {status: 200, body: `{"state":"closed","head":{"sha":"old-head"}}`},
		"should report authentication failures without response contents": {status: 401, body: "sensitive-response", wantError: true},
		"should report rate limiting without response contents":           {status: 429, body: "sensitive-response", wantError: true},
		"should reject invalid JSON":                                      {status: 200, body: "invalid", wantError: true},
		"should reject empty head SHAs":                                   {status: 200, body: `{"state":"open"}`, wantError: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tt.status); fmt.Fprint(w, tt.body) }))
			defer server.Close()
			result, err := checkPR(t.Context(), githubClient{baseURL: server.URL, http: server.Client()}, inputChecker{}, "stackrox/stackrox", 1)
			if (err != nil) != tt.wantError {
				t.Fatalf("unexpected error: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "sensitive-response") {
				t.Fatal("response body exposed")
			}
			if len(result.Jobs) != 0 || result.URL != "https://github.com/stackrox/stackrox/pull/1" {
				t.Fatalf("unexpected result: %+v", result)
			}
			if tt.wantError && (result.Action != ActionWait || len(result.Warnings) != 1) {
				t.Fatalf("invalid error result: %+v", result)
			}
			if !tt.wantError && (result.Action != ActionNone || result.Message != "PR is not open.") {
				t.Fatalf("unexpected closed PR result: %+v", result)
			}
		})
	}
}

func TestCheckPRReturnsActionAndPolicyWarnings(t *testing.T) {
	tests := map[string]struct {
		state, conclusion string
		action            Action
		message           string
	}{
		"should return retries alongside policy warnings": {state: "completed", conclusion: "failure", action: ActionRetest},
		"should return wait advice":                       {state: "in_progress", action: ActionWait, message: "roxctl-on-push (in_progress)."},
		"should report successful builds":                 {state: "completed", conclusion: "success", action: ActionNone, message: "No build retests needed."},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			checks := successfulChecks()
			for i := range checks {
				if checks[i].Name == checkPrefix+"roxctl-on-push" {
					checks[i].Status = tt.state
					checks[i].Conclusion = tt.conclusion
				}
			}
			checks = append(checks, buildCheck("acs-conforma-prod-like-5-0 / main-5-0", "completed", "failure"))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/pulls/") {
					fmt.Fprint(w, `{"state":"open","head":{"sha":"head"}}`)
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"check_runs": checks})
			}))
			defer server.Close()
			result, err := checkPR(t.Context(), githubClient{baseURL: server.URL, http: server.Client()}, inputChecker{}, "stackrox/stackrox", 23956)
			if err != nil {
				t.Fatal(err)
			}
			if result.Action != tt.action || result.Message != tt.message || !strings.Contains(strings.Join(result.Warnings, "\n"), "policy check") {
				t.Fatalf("unexpected result: %+v", result)
			}
		})
	}
}

func TestCheckTag(t *testing.T) {
	const sha = "6938eeab2b7ba5235bc4c710d56e742aab6ac52f"
	tests := map[string]struct {
		tag        string
		status     int
		body       string
		checksFail bool
		wantError  bool
	}{
		"should resolve the Scanner tag to its commit":           {tag: "2.39.27", status: 200, body: `{"sha":"` + sha + `"}`},
		"should resolve namespaced tags without branch fallback": {tag: "release/2.39.27", status: 200, body: `{"sha":"` + sha + `"}`},
		"should escape valid URL-sensitive tag characters":       {tag: "release/v1#build%2F", status: 200, body: `{"sha":"` + sha + `"}`},
		"should report missing tags":                             {tag: "missing", status: 404, body: "sensitive-response", wantError: true},
		"should report denied access":                            {tag: "2.39.27", status: 403, body: "sensitive-response", wantError: true},
		"should reject empty commit SHAs":                        {tag: "2.39.27", status: 200, body: `{}`, wantError: true},
		"should reject malformed tag responses":                  {tag: "2.39.27", status: 200, body: `invalid`, wantError: true},
		"should report check lookup errors once":                 {tag: "2.39.27", status: 200, body: `{"sha":"` + sha + `"}`, checksFail: true, wantError: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected mutation: %s", r.Method)
				}
				switch r.URL.Path {
				case "/repos/stackrox/scanner/commits/refs/tags/" + tt.tag:
					if r.URL.RawQuery != "" {
						t.Errorf("tag changed the query: %s", r.URL.RawQuery)
					}
					w.WriteHeader(tt.status)
					fmt.Fprint(w, tt.body)
				case "/repos/stackrox/scanner/commits/" + sha + "/check-runs":
					if tt.checksFail {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					checks := []checkRun{
						buildCheck("scanner-on-push", "completed", "failure"),
						buildCheck("scanner-db-on-push", "completed", "failure"),
						buildCheck("scanner-slim-on-push", "completed", "failure"),
						buildCheck("scanner-db-slim-on-push", "completed", "failure"),
						buildCheck("acs-conforma-dev-4-10 / scanner-slim-4-10", "completed", "failure"),
					}
					json.NewEncoder(w).Encode(map[string]any{"check_runs": checks})
				default:
					t.Errorf("unexpected path: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			result, err := checkTag(t.Context(), githubClient{baseURL: server.URL, http: server.Client()}, inputChecker{}, "stackrox/scanner", tt.tag)
			if (err != nil) != tt.wantError {
				t.Fatalf("error=%v; wantError=%v", err, tt.wantError)
			}
			if tt.wantError {
				if len(result.Jobs) != 0 || result.Action != ActionWait || len(result.Warnings) != 1 || strings.Contains(result.Warnings[0], "sensitive-response") {
					t.Fatalf("invalid error result: %+v", result)
				}
				return
			}
			wantJobs := []string{"scanner-on-push", "scanner-db-on-push", "scanner-slim-on-push", "scanner-db-slim-on-push"}
			if result.URL != "https://github.com/stackrox/scanner/commit/"+sha || result.Tag != tt.tag || result.SHA != sha || result.Action != ActionRetest || !slices.Equal(result.Jobs, wantJobs) || len(result.Warnings) != 0 {
				t.Fatalf("unexpected result: %+v", result)
			}
		})
	}
}

func TestCheckTagChecksRetagInputsAtResolvedCommit(t *testing.T) {
	files := requirementFiles(t)
	checks := successfulChecks()
	for i := range checks {
		if checks[i].Name == checkPrefix+"retag-scanner" || checks[i].Name == checkPrefix+"operator-bundle-on-push" {
			checks[i].Conclusion = "failure"
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected mutation: %s", r.Method)
		}
		switch {
		case r.URL.Path == "/repos/stackrox/stackrox/commits/refs/tags/4.10.9":
			fmt.Fprint(w, `{"sha":"tag-commit"}`)
		case r.URL.Path == "/repos/stackrox/stackrox/commits/tag-commit/check-runs":
			json.NewEncoder(w).Encode(map[string]any{"check_runs": checks})
		case strings.HasPrefix(r.URL.Path, "/repos/stackrox/stackrox/contents/"):
			if r.URL.Query().Get("ref") != "tag-commit" {
				t.Error("input requirements must use the resolved commit")
			}
			path := strings.TrimPrefix(r.URL.Path, "/repos/stackrox/stackrox/contents/")
			data, ok := files[path]
			if !ok {
				t.Errorf("unexpected file: %s", path)
			}
			json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString(data)})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	checked := false
	checker := inputChecker{logs: func(_ context.Context, check checkRun, requirement imageRequirement) (logEvidence, error) {
		checked = true
		if check.Name != checkPrefix+"retag-scanner" || requirement.revision != "tag-commit" || requirement.sourceURL != "https://github.com/stackrox/stackrox" {
			t.Errorf("wrong input context: check=%+v requirement=%+v", check, requirement)
		}
		return logEvidence{reason: "Konflux reported image not found"}, nil
	}}
	result, err := checkTag(t.Context(), githubClient{baseURL: server.URL, http: server.Client()}, checker, "stackrox/stackrox", "4.10.9")
	if err != nil {
		t.Fatal(err)
	}
	if !checked || !strings.Contains(strings.Join(result.Warnings, "\n"), "Konflux reported image not found") || len(result.Jobs) != 0 {
		t.Fatalf("missing inputs must withhold retag and downstream retries: %+v", result)
	}
}
