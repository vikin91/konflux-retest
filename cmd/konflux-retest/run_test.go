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
