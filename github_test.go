package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReportPRPaginatesAndUsesCurrentHead(t *testing.T) {
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
	var out strings.Builder
	if err := reportPR(t.Context(), client, inputChecker{}, "stackrox/stackrox", 23956, &out); err != nil {
		t.Fatal(err)
	}
	want := "---\nhttps://github.com/stackrox/stackrox/pull/23956\n\nNext action: Retest\n/konflux-retest roxctl-on-push\n\nWarnings:\n"
	if !strings.HasPrefix(out.String(), want) {
		t.Fatalf("stdout=%q; want %q", out.String(), want)
	}
	if pages != 2 {
		t.Errorf("read %d pages; want 2", pages)
	}
	if !strings.Contains(out.String(), "policy check") {
		t.Fatalf("missing diagnostics: %s", out.String())
	}
	if strings.Contains(out.String(), "test-token") {
		t.Fatal("token exposed")
	}
}

func TestReportPRHandlesClosedPRAndAPIErrors(t *testing.T) {
	tests := map[string]struct {
		status    int
		body      string
		wantError bool
		message   string
	}{
		"should skip closed PRs": {status: 200, body: `{"state":"closed","head":{"sha":"old-head"}}`, message: "PR is not open"},
		"should report authentication failures without response contents": {status: 401, body: "sensitive-response", wantError: true},
		"should report rate limiting without response contents":           {status: 429, body: "sensitive-response", wantError: true},
		"should reject invalid JSON":                                      {status: 200, body: "invalid", wantError: true},
		"should reject empty head SHAs":                                   {status: 200, body: `{"state":"open"}`, wantError: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tt.status); fmt.Fprint(w, tt.body) }))
			defer server.Close()
			var out strings.Builder
			err := reportPR(t.Context(), githubClient{baseURL: server.URL, http: server.Client()}, inputChecker{}, "stackrox/stackrox", 1, &out)
			if (err != nil) != tt.wantError {
				t.Fatalf("unexpected error: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "sensitive-response") {
				t.Fatal("response body exposed")
			}
			if strings.Count(out.String(), "Next action:") != 1 || strings.Count(out.String(), "Warnings:") != 1 || !strings.HasPrefix(out.String(), "---\n") {
				t.Fatalf("invalid report format: %s", out.String())
			}
			if strings.Contains(out.String(), "/konflux-retest") {
				t.Fatal("unexpected retry command")
			}
			if !strings.Contains(out.String()+out.String(), tt.message) {
				t.Fatalf("diagnostics=%q", out.String())
			}
		})
	}
}

func TestReportPRPrintsActionBlockBeforeDiagnostics(t *testing.T) {
	tests := map[string]struct {
		state      string
		conclusion string
		wantBlock  string
	}{
		"should print retry comments before policy diagnostics": {
			state: "completed", conclusion: "failure",
			wantBlock: "Next action: Retest\n/konflux-retest roxctl-on-push\n\nWarnings:\n",
		},
		"should print wait advice before policy diagnostics": {
			state:     "in_progress",
			wantBlock: "Next action: Wait\nroxctl-on-push (in_progress).\n\nWarnings:\n",
		},
		"should print a status when build jobs passed": {
			state: "completed", conclusion: "success",
			wantBlock: "Next action: None\nNo build retests needed.\n\nWarnings:\n",
		},
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
			// Both streams share a writer to verify the order seen in a terminal.
			var terminal strings.Builder
			client := githubClient{baseURL: server.URL, http: server.Client()}
			if err := reportPR(t.Context(), client, inputChecker{}, "stackrox/stackrox", 23956, &terminal); err != nil {
				t.Fatal(err)
			}
			wantPrefix := "---\nhttps://github.com/stackrox/stackrox/pull/23956\n\n" + tt.wantBlock
			if !strings.HasPrefix(terminal.String(), wantPrefix) {
				t.Fatalf("terminal output must start with %q; got %q", wantPrefix, terminal.String())
			}
			if !strings.Contains(strings.TrimPrefix(terminal.String(), wantPrefix), "policy check") {
				t.Fatal("expected policy diagnostics after the action block")
			}
		})
	}
}

func TestReportTag(t *testing.T) {
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
			var out strings.Builder
			err := reportTag(t.Context(), githubClient{baseURL: server.URL, http: server.Client()}, inputChecker{}, "stackrox/scanner", tt.tag, &out)
			if (err != nil) != tt.wantError {
				t.Fatalf("error=%v; wantError=%v", err, tt.wantError)
			}
			if strings.Count(out.String(), "Next action:") != 1 || strings.Count(out.String(), "Warnings:") != 1 {
				t.Fatalf("invalid report: %s", out.String())
			}
			if tt.wantError {
				if strings.Contains(out.String(), "/konflux-retest") || strings.Contains(out.String(), "sensitive-response") || !strings.Contains(out.String(), "Next action: Wait") {
					t.Fatalf("invalid error report: %s", out.String())
				}
				return
			}
			want := "---\nhttps://github.com/stackrox/scanner/commit/" + sha + "\nTag: " + tt.tag + "\n\n" +
				"Next action: Retest\n/konflux-retest scanner-on-push\n/konflux-retest scanner-db-on-push\n" +
				"/konflux-retest scanner-slim-on-push\n/konflux-retest scanner-db-slim-on-push\n\nWarnings:\nNone.\n"
			if out.String() != want {
				t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
			}
		})
	}
}

func TestReportTagChecksRetagInputsAtResolvedCommit(t *testing.T) {
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
	var out strings.Builder
	err := reportTag(t.Context(), githubClient{baseURL: server.URL, http: server.Client()}, checker, "stackrox/stackrox", "4.10.9", &out)
	if err != nil {
		t.Fatal(err)
	}
	if !checked || !strings.Contains(out.String(), "Konflux reported image not found") || strings.Contains(out.String(), "/konflux-retest") {
		t.Fatalf("missing inputs must withhold retag and downstream retries: %s", out.String())
	}
}
