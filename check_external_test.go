package konfluxretest_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	konfluxretest "github.com/vikin91/konflux-retest"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestExportedChecks(t *testing.T) {
	tests := map[string]struct {
		check                func(context.Context, konfluxretest.Config) (konfluxretest.Result, error)
		path, body, url, tag string
	}{
		"should check a PR from another package": {
			check: func(ctx context.Context, config konfluxretest.Config) (konfluxretest.Result, error) {
				return konfluxretest.CheckPR(ctx, config, "stackrox/scanner", 42)
			},
			path: "/repos/stackrox/scanner/pulls/42", body: `{"state":"open","head":{"sha":"head"}}`,
			url: "https://github.com/stackrox/scanner/pull/42",
		},
		"should check a namespaced tag from another package": {
			check: func(ctx context.Context, config konfluxretest.Config) (konfluxretest.Result, error) {
				return konfluxretest.CheckTag(ctx, config, "stackrox/scanner", "release/v1#build%2F")
			},
			path: "/repos/stackrox/scanner/commits/refs/tags/release/v1#build%2F", body: `{"sha":"head"}`,
			url: "https://github.com/stackrox/scanner/commit/head", tag: "release/v1#build%2F",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			requests := 0
			client := &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Method != http.MethodGet || r.URL.Host != "api.github.com" || r.URL.RawQuery != "" && r.URL.Path == tt.path || r.Header.Get("Authorization") != "Bearer explicit-token" {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
				}
				body := tt.body
				if r.URL.Path != tt.path {
					if r.URL.Path != "/repos/stackrox/scanner/commits/head/check-runs" {
						t.Fatalf("unexpected path: %s", r.URL.Path)
					}
					body = `{"check_runs":[{"id":1,"name":"Red Hat Konflux / scanner-on-push","status":"completed","conclusion":"failure","app":{"slug":"pipelines-as-code"}}]}`
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			result, err := tt.check(t.Context(), konfluxretest.Config{GitHubToken: "explicit-token", HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			if requests != 2 || result.URL != tt.url || result.Tag != tt.tag || result.SHA != "head" || result.Action != konfluxretest.ActionRetest || !slices.Equal(result.Jobs, []string{"scanner-on-push"}) {
				t.Fatalf("unexpected result after %d requests: %+v", requests, result)
			}
			if client.CheckRedirect != nil || client.Timeout != time.Second {
				t.Fatal("caller HTTP client was modified")
			}
		})
	}
}

func TestExportedChecksRejectInvalidInputs(t *testing.T) {
	config := konfluxretest.Config{HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid input reached the network")
		return nil, errors.New("unexpected request")
	})}}
	tests := map[string]func() (konfluxretest.Result, error){
		"should require a qualified repository": func() (konfluxretest.Result, error) { return konfluxretest.CheckPR(t.Context(), config, "scanner", 1) },
		"should reject invalid PR numbers": func() (konfluxretest.Result, error) {
			return konfluxretest.CheckPR(t.Context(), config, "stackrox/scanner", 0)
		},
		"should reject repository query injection": func() (konfluxretest.Result, error) {
			return konfluxretest.CheckTag(t.Context(), config, "stackrox/scanner?ref=main", "v1")
		},
		"should reject empty tags": func() (konfluxretest.Result, error) {
			return konfluxretest.CheckTag(t.Context(), config, "stackrox/scanner", "")
		},
		"should reject revision expressions": func() (konfluxretest.Result, error) {
			return konfluxretest.CheckTag(t.Context(), config, "stackrox/scanner", "v1~1")
		},
	}
	for name, check := range tests {
		t.Run(name, func(t *testing.T) {
			result, err := check()
			if err == nil || result.Action != konfluxretest.ActionWait || len(result.Jobs) != 0 {
				t.Fatalf("result=%+v error=%v", result, err)
			}
		})
	}
}

func TestExportedCheckPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	config := konfluxretest.Config{HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, r.Context().Err()
	})}}
	result, err := konfluxretest.CheckPR(ctx, config, "stackrox/scanner", 1)
	if !errors.Is(err, context.Canceled) || result.Action != konfluxretest.ActionWait {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

func TestExportedCheckDoesNotReadEnvironmentCredentials(t *testing.T) {
	t.Setenv("GH_TOKEN", "environment-secret")
	t.Setenv("GITHUB_TOKEN", "other-secret")
	config := konfluxretest.Config{HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" {
			t.Fatal("unexpected ambient credential")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"state":"closed"}`))}, nil
	})}}
	result, err := konfluxretest.CheckPR(t.Context(), config, "stackrox/scanner", 1)
	if err != nil || result.Action != konfluxretest.ActionNone {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}
