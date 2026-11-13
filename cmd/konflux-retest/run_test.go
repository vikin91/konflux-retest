package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRunContinuesAfterLookupFailure(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token")
	tests := map[string]struct {
		args                      []string
		first, second, body, want string
	}{
		"should continue with the next PR": {
			args:  []string{"--repo", "stackrox/scanner", "--prs", "1", "2"},
			first: "/repos/stackrox/scanner/pulls/1", second: "/repos/stackrox/scanner/pulls/2",
			body: `{"state":"closed"}`, want: "https://github.com/stackrox/scanner/pull/2\n\nNext action: None\nPR is not open.",
		},
		"should continue with the next tag": {
			args:  []string{"--repo", "stackrox/scanner", "--tags", "missing", "v1"},
			first: "/repos/stackrox/scanner/commits/refs/tags/missing", second: "/repos/stackrox/scanner/commits/refs/tags/v1",
			body: `{"sha":"head"}`, want: "https://github.com/stackrox/scanner/commit/head\nTag: v1\n\nNext action: Wait",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			transport := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = transport })
			var paths []string
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				paths = append(paths, r.URL.Path)
				status, body := http.StatusOK, tt.body
				switch r.URL.Path {
				case tt.first:
					status, body = http.StatusNotFound, "sensitive-response"
				case tt.second:
				case "/repos/stackrox/scanner/commits/head/check-runs":
					body = `{"check_runs":[]}`
				default:
					t.Fatalf("unexpected request: %s", r.URL)
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			var out, diagnostics strings.Builder
			code := run(t.Context(), tt.args, &out, &diagnostics)
			if code != 1 || diagnostics.Len() != 0 || len(paths) < 2 || paths[0] != tt.first || paths[1] != tt.second {
				t.Fatalf("code=%d stderr=%q paths=%v", code, diagnostics.String(), paths)
			}
			if strings.Count(out.String(), "Next action:") != 2 || strings.Count(out.String(), "Warnings:") != 2 || !strings.Contains(out.String(), "\n\n---\n"+tt.want) || strings.Contains(out.String(), "sensitive-response") {
				t.Fatalf("unexpected output: %s", out.String())
			}
		})
	}
}

func TestRunCollectorPR(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token")
	transport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = transport })
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/repos/stackrox/collector/pulls/4307":
			body = `{"state":"open","head":{"sha":"head"}}`
		case "/repos/stackrox/collector/commits/head/check-runs":
			body = `{"check_runs":[{"id":1,"name":"Red Hat Konflux / collector-on-push","status":"completed","conclusion":"failure"},{"id":2,"name":"Red Hat Konflux / acs-conforma-dev-5-0 / collector-5-0","status":"completed","conclusion":"cancelled"}]}`
		default:
			t.Fatalf("unexpected request: %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	var out, diagnostics strings.Builder
	code := run(t.Context(), []string{"--repo", "collector", "--prs", "4307"}, &out, &diagnostics)
	want := "---\nhttps://github.com/stackrox/collector/pull/4307\n\nNext action: Retest\n/konflux-retest collector-on-push\n\nWarnings:\nNone.\n"
	if code != 0 || diagnostics.Len() != 0 || out.String() != want {
		t.Fatalf("code=%d stderr=%q output=%q; want %q", code, diagnostics.String(), out.String(), want)
	}
}

func TestRunFactTag(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token")
	const sha = "7826bed24314e774deb43ae5db1e7b30b1eda796"
	tests := map[string]struct {
		repo, status, conclusion, want string
	}{
		"should wait only for the running FACT build": {
			repo: "stackrox/fact", status: "in_progress",
			want: "Next action: Wait\nfact-on-push (in_progress).\n\nWarnings:\nMilestone 1 is not ready: fact-on-push (in_progress).\n",
		},
		"should retry the failed FACT build using the short repository name": {
			repo: "fact", status: "completed", conclusion: "failure",
			want: "Next action: Retest\nPost each command below as a separate GitHub comment.\n/retest fact-on-push tag:0.3.12\n\nWarnings:\nNone.\n",
		},
		"should preserve a successful FACT build": {
			repo: "stackrox/fact", status: "completed", conclusion: "success",
			want: "Next action: None\nNo build retests needed.\n\nWarnings:\nNone.\n",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			transport := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = transport })
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				var body string
				switch r.URL.Path {
				case "/repos/stackrox/fact/commits/refs/tags/0.3.12":
					body = `{"sha":"` + sha + `"}`
				case "/repos/stackrox/fact/commits/" + sha + "/check-runs":
					body = `{"check_runs":[{"id":1,"name":"Red Hat Konflux / fact-on-push","status":"` + tt.status + `","conclusion":"` + tt.conclusion + `"},{"id":2,"name":"Red Hat Konflux / acs-conforma-dev-4-11 / fact-4-11","status":"completed","conclusion":"cancelled"}]}`
				default:
					t.Fatalf("unexpected request: %s", r.URL)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			var out, diagnostics strings.Builder
			code := run(t.Context(), []string{"--repo", tt.repo, "--tags", "0.3.12"}, &out, &diagnostics)
			want := "---\nhttps://github.com/stackrox/fact/commit/" + sha + "\nTag: 0.3.12\n\n" + tt.want
			if code != 0 || diagnostics.Len() != 0 || out.String() != want {
				t.Fatalf("code=%d stderr=%q output=%q; want %q", code, diagnostics.String(), out.String(), want)
			}
		})
	}
}
