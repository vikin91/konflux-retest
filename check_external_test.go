package konfluxretest_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	konfluxretest "github.com/vikin91/konflux-retest"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCheckErrorClassification(t *testing.T) {
	for name, tt := range map[string]struct {
		status   int
		header   http.Header
		body     string
		cause    error
		wantKind konfluxretest.CheckErrorKind
	}{
		"should classify an invalid credential": {status: 401, wantKind: konfluxretest.ErrCheckCredential},
		"should classify a denied permission":   {status: 403, wantKind: konfluxretest.ErrCheckPermission},
		"should classify a rate limit status":   {status: 429, wantKind: konfluxretest.ErrCheckRateLimit},
		"should classify an exhausted quota": {
			status: 403, header: http.Header{"X-Ratelimit-Remaining": {"0"}}, wantKind: konfluxretest.ErrCheckRateLimit,
		},
		"should classify a rate limit retry header": {
			status: 403, header: http.Header{"Retry-After": {"30"}}, wantKind: konfluxretest.ErrCheckRateLimit,
		},
		"should classify a secondary limit without headers": {
			status: 403, body: `{"message":"You have exceeded a secondary rate limit. private-response"}`, wantKind: konfluxretest.ErrCheckRateLimit,
		},
		"should classify a connection failure": {cause: syscall.ECONNRESET, wantKind: konfluxretest.ErrCheckNetwork},
		"should preserve a deadline":           {cause: context.DeadlineExceeded, wantKind: konfluxretest.ErrCheckNetwork},
		"should preserve a canceled context":   {cause: context.Canceled},
	} {
		t.Run(name, func(t *testing.T) {
			config := konfluxretest.Config{HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				if tt.cause != nil {
					return nil, fmt.Errorf("private-transport: %w", tt.cause)
				}
				return &http.Response{StatusCode: tt.status, Header: tt.header, Body: io.NopCloser(strings.NewReader(tt.body))}, nil
			})}}
			checks := map[string]func() (konfluxretest.Result, error){
				"CheckPR": func() (konfluxretest.Result, error) {
					return konfluxretest.CheckPR(t.Context(), config, "stackrox/scanner", 42)
				},
				"CheckTag": func() (konfluxretest.Result, error) {
					return konfluxretest.CheckTag(t.Context(), config, "stackrox/scanner", "release/v1")
				},
			}
			for name, check := range checks {
				t.Run(name, func(t *testing.T) {
					result, err := check()
					if err == nil || tt.cause != nil && !errors.Is(err, tt.cause) {
						t.Fatalf("error = %v, want underlying cause %v", err, tt.cause)
					}
					for _, kind := range []konfluxretest.CheckErrorKind{
						konfluxretest.ErrCheckCredential, konfluxretest.ErrCheckPermission,
						konfluxretest.ErrCheckRateLimit, konfluxretest.ErrCheckNetwork,
					} {
						if errors.Is(err, kind) != (kind == tt.wantKind) {
							t.Fatalf("error = %v, unexpected match for %v", err, kind)
						}
					}
					if len(result.Diagnostics) != 1 || result.Diagnostics[0].Severity != konfluxretest.DiagnosticError ||
						result.Diagnostics[0].Err != err || strings.Contains(fmt.Sprintf("%+v", result), "private-") {
						t.Fatalf("unexpected diagnostic: %+v", result.Diagnostics)
					}
				})
			}
		})
	}
}

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
					body = `{"check_runs":[{"id":1,"name":"Red Hat Konflux / scanner-on-push","status":"completed","conclusion":"failure","app":{"id":296509,"slug":"red-hat-konflux"}}]}`
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

func TestCheckPRWithGitHubBaseURL(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/repos/stackrox/scanner/pulls/42", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"state":"open","head":{"sha":"head"}}`)
	})
	mux.HandleFunc("GET /api/repos/stackrox/scanner/commits/head/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"check_runs":[{"id":1,"name":"Red Hat Konflux / scanner-on-push","status":"completed","conclusion":"failure","app":{"id":296509}}]}`)
	})
	mux.HandleFunc("GET /api/repos/stackrox/scanner/pulls/43", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/redirect-target", http.StatusFound)
	})
	mux.HandleFunc("GET /redirect-target", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("GitHub redirect was followed")
		w.WriteHeader(http.StatusForbidden)
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("GitHub request is missing the configured token")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	defer server.Close()

	config := konfluxretest.Config{GitHubBaseURL: server.URL + "/api/", GitHubToken: "test-token"}
	result, err := konfluxretest.CheckPR(t.Context(), config, "stackrox/scanner", 42)
	if err != nil || result.URL != "https://github.com/stackrox/scanner/pull/42" || result.SHA != "head" ||
		result.Action != konfluxretest.ActionRetest || !slices.Equal(result.Jobs, []string{"scanner-on-push"}) {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	result, err = konfluxretest.CheckPR(t.Context(), config, "stackrox/scanner", 43)
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") || result.Action != konfluxretest.ActionWait {
		t.Fatalf("result=%+v error=%v", result, err)
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

func TestCheckPRRejectsInvalidGitHubBaseURL(t *testing.T) {
	for name, baseURL := range map[string]string{
		"relative URL":    "/api",
		"missing host":    "https:///api",
		"non-HTTP scheme": "ftp://example.test",
		"user info":       "https://user:secret@example.test",
		"query":           "https://example.test?secret=value",
		"fragment":        "https://example.test#secret",
	} {
		t.Run(name, func(t *testing.T) {
			config := konfluxretest.Config{
				GitHubBaseURL: baseURL,
				HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					t.Fatal("invalid GitHub base URL reached the network")
					return nil, errors.New("unexpected request")
				})},
			}
			result, err := konfluxretest.CheckPR(t.Context(), config, "stackrox/scanner", 42)
			if err == nil || err.Error() != "invalid GitHub request URL" || result.Action != konfluxretest.ActionWait {
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

func TestPipelineRunStartFailures(t *testing.T) {
	const sha = "c83bba1d0d8f1fbfc24e4df67d2fefe340213877"
	fixture, err := os.ReadFile("testdata/pipelinerun-start-failures.json")
	if err != nil {
		t.Fatal(err)
	}
	for name, strict := range map[string]bool{"should require identity in strict mode": true, "should preserve default eligibility": false} {
		t.Run(name, func(t *testing.T) {
			tokenRequests := 0
			config := konfluxretest.Config{
				VerifyRunIdentity: strict,
				GitHubToken:       "configured-gh-secret",
				QuayPassword:      "configured-quay-secret",
				KonfluxToken: func(_ context.Context, host string) (string, error) {
					tokenRequests++
					if host != "api.example.openshiftapps.com" {
						t.Fatalf("unexpected cluster: %s", host)
					}
					return "cluster-token", nil
				},
				HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.Method != http.MethodGet {
						t.Fatalf("unexpected method: %s", r.Method)
					}
					var body string
					switch r.URL.Host + r.URL.Path {
					case "api.github.com/repos/stackrox/stackrox/pulls/23958":
						body = `{"state":"open","head":{"sha":"` + sha + `"}}`
					case "api.github.com/repos/stackrox/stackrox/commits/" + sha + "/check-runs":
						body = string(fixture)
					case "api.example.openshiftapps.com/apis/tekton.dev/v1/namespaces/builds/pipelineruns/checks-22":
						body = `{"metadata":{"name":"checks-22","namespace":"builds","annotations":{
							"pipelinesascode.tekton.dev/repo-url":"https://github.com/stackrox/stackrox",
							"pipelinesascode.tekton.dev/sha":"` + sha + `",
							"pipelinesascode.tekton.dev/check-run-id":"22",
							"pipelinesascode.tekton.dev/event-type":"pull_request",
							"pipelinesascode.tekton.dev/branch":"refs/heads/topic",
							"pipelinesascode.tekton.dev/pull-request":"23958"
						}}}`
					default:
						t.Fatalf("unexpected request: %s", r.URL)
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
				})},
			}
			result, err := konfluxretest.CheckPR(t.Context(), config, "stackrox/stackrox", 23958)
			wantJobs := []string{"checks", "scanner-v4-on-push"}
			if strict {
				wantJobs = []string{"checks"}
				if err == nil || !strings.Contains(err.Error(), "identity verification failed for 1 check (1 unsupported URL)") ||
					!strings.Contains(err.Error(), "scanner-v4-on-push") || tokenRequests != 1 {
					t.Fatalf("expected only the unexplained homepage to fail identity lookup, got %v after %d token requests", err, tokenRequests)
				}
			} else if err != nil || tokenRequests != 0 {
				t.Fatalf("unexpected identity lookup, got %v after %d token requests", err, tokenRequests)
			}
			if result.Action != konfluxretest.ActionRetest || !slices.Equal(result.Jobs, wantJobs) {
				t.Fatalf("unexpected action or retry eligibility: %+v", result)
			}
			wantFailures := []konfluxretest.StartFailure{
				{CheckID: 134464223121, Job: "main-on-push", Cluster: "example.openshiftapps.com", Category: konfluxretest.StartFailureAdmissionPolicy,
					Reason: "An admission policy denied PipelineRun creation."},
				{CheckID: 20, Job: "operator-on-push", Cluster: "example.openshiftapps.com", Category: konfluxretest.StartFailureOther,
					Reason: "See the GitHub check for the PipelineRun creation failure."},
				{CheckID: 23, Job: "operator-bundle-on-push", Category: konfluxretest.StartFailureOther, Reason: "See the GitHub check for the PipelineRun creation failure."},
			}
			if !slices.Equal(result.StartFailures, wantFailures) {
				t.Fatalf("unexpected StartFailures: %+v", result.StartFailures)
			}
			if len(result.Runs) != 5 {
				t.Fatalf("unexpected run inventory: %+v", result.Runs)
			}
			for _, run := range result.Runs {
				idx := slices.IndexFunc(wantFailures, func(f konfluxretest.StartFailure) bool { return f.CheckID == run.ID })
				if idx >= 0 && (run.Status != "completed" || run.IdentityKnown || run.Eligible || run.ImageURL != "" || run.ImageDigest != "") {
					t.Fatalf("unexpected evidence for a failed start: %+v", run)
				}
				if run.ID == 22 && (run.IdentityKnown != strict || !run.Eligible) {
					t.Fatalf("normal PipelineRun lost its evidence: %+v", run)
				}
			}
			visible := fmt.Sprintf("%+v %v", result, err)
			for _, secret := range []string{"configured-gh-secret", "configured-quay-secret", "auth-secret", "value with spaces", "param-secret", "reason-user", "reason-password", "reason-query", "reason-fragment", "url-user", "url-password", "url-query-secret", "url-fragment", "deny-pipelinerun-create"} {
				if strings.Contains(visible, secret) {
					t.Error("credential leaked in check result or error")
				}
			}
		})
	}
}
