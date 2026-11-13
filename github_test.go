package main

import (
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
