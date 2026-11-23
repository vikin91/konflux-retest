package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	konfluxretest "github.com/vikin91/konflux-retest"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixtureKonfluxToken(context.Context, string) (string, error) {
	return "test-konflux-token", nil
}

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
			code := run(t.Context(), tt.args, &out, &diagnostics, konfluxretest.Config{KonfluxToken: fixtureKonfluxToken})
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
			body = `{"check_runs":[{"id":1,"app":{"id":296509},"name":"Red Hat Konflux / collector-on-push","details_url":"https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/collector","status":"completed","conclusion":"failure"},{"id":2,"app":{"id":296509},"name":"Red Hat Konflux / acs-conforma-dev-5-0 / collector-5-0","status":"completed","conclusion":"cancelled"}]}`
		case "/apis/tekton.dev/v1/namespaces/builds/pipelineruns/collector":
			body = `{"metadata":{"name":"collector","namespace":"builds","annotations":{"pipelinesascode.tekton.dev/check-run-id":"1","pipelinesascode.tekton.dev/sha":"head","pipelinesascode.tekton.dev/repo-url":"https://github.com/stackrox/collector","pipelinesascode.tekton.dev/event-type":"pull_request","pipelinesascode.tekton.dev/branch":"master","pipelinesascode.tekton.dev/pull-request":"4307"}}}`
		default:
			t.Fatalf("unexpected request: %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	var out, diagnostics strings.Builder
	code := run(t.Context(), []string{"--repo", "collector", "--prs", "4307"}, &out, &diagnostics, konfluxretest.Config{KonfluxToken: fixtureKonfluxToken})
	want := "---\nhttps://github.com/stackrox/collector/pull/4307\n\nNext action: Retest\n/konflux-retest collector-on-push\n\nWarnings:\nNone.\n"
	if code != 0 || diagnostics.Len() != 0 || out.String() != want {
		t.Fatalf("code=%d stderr=%q output=%q; want %q", code, diagnostics.String(), out.String(), want)
	}
}

func TestRunFactTag(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token")
	const sha = "7826bed24314e774deb43ae5db1e7b30b1eda796"
	tests := map[string]struct {
		repo, status, conclusion, ref, want string
	}{
		"should wait only for the running FACT build": {
			repo: "stackrox/fact", status: "in_progress", ref: "refs/tags/0.3.12",
			want: "Next action: Wait\nfact-on-push (in_progress).\n\nWarnings:\nMilestone 1 is not ready: fact-on-push (in_progress).\n",
		},
		"should retry the failed FACT build using the short repository name": {
			repo: "fact", status: "completed", conclusion: "failure", ref: "refs/tags/0.3.12",
			want: "Next action: Retest\nPost each command below as a separate GitHub comment.\n/retest fact-on-push tag:0.3.12\n\nWarnings:\nNone.\n",
		},
		"should withhold a branch failure on the same commit": {
			repo: "stackrox/fact", status: "completed", conclusion: "failure", ref: "refs/heads/main",
			want: "Next action: Wait\nNo verified build checks match the requested tag or PR.\n\nWarnings:\nNone.\n",
		},
		"should preserve a successful FACT build": {
			repo: "stackrox/fact", status: "completed", conclusion: "success", ref: "refs/tags/0.3.12",
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
					body = `{"check_runs":[{"id":1,"app":{"id":296509},"name":"Red Hat Konflux / fact-on-push","details_url":"https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/fact","status":"` + tt.status + `","conclusion":"` + tt.conclusion + `"},{"id":2,"app":{"id":296509},"name":"Red Hat Konflux / acs-conforma-dev-4-11 / fact-4-11","status":"completed","conclusion":"cancelled"}]}`
				case "/apis/tekton.dev/v1/namespaces/builds/pipelineruns/fact":
					body = `{"metadata":{"name":"fact","namespace":"builds","annotations":{"pipelinesascode.tekton.dev/check-run-id":"1","pipelinesascode.tekton.dev/sha":"` + sha + `","pipelinesascode.tekton.dev/repo-url":"https://github.com/stackrox/fact","pipelinesascode.tekton.dev/event-type":"push","pipelinesascode.tekton.dev/branch":"` + tt.ref + `"}}}`
				default:
					t.Fatalf("unexpected request: %s", r.URL)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			var out, diagnostics strings.Builder
			code := run(t.Context(), []string{"--repo", tt.repo, "--tags", "0.3.12"}, &out, &diagnostics, konfluxretest.Config{KonfluxToken: fixtureKonfluxToken})
			want := "---\nhttps://github.com/stackrox/fact/commit/" + sha + "\nTag: 0.3.12\n\n" + tt.want
			if code != 0 || diagnostics.Len() != 0 || out.String() != want {
				t.Fatalf("code=%d stderr=%q output=%q; want %q", code, diagnostics.String(), out.String(), want)
			}
		})
	}
}

func TestRunCommentPosting(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token")
	const (
		repoPath  = "/repos/stackrox/scanner"
		sha       = "6938eeab2b7ba5235bc4c710d56e742aab6ac52f"
		otherSHA  = "7826bed24314e774deb43ae5db1e7b30b1eda796"
		tag       = "release/v1"
		prLink    = "https://github.com/stackrox/scanner/pull/42#issuecomment-"
		tagLink   = "https://github.com/stackrox/scanner/commit/" + sha + "#commitcomment-"
		prCommand = "/konflux-retest scanner-on-push"
		prSecond  = "/konflux-retest scanner-slim-on-push"
		prMissing = "/konflux-retest scanner-db-on-push"
		prBatch   = prCommand + "\n" + prSecond
		tagOne    = "/retest scanner-on-push tag:" + tag
		tagTwo    = "/retest scanner-slim-on-push tag:" + tag
	)
	tests := map[string]struct {
		tag, post, stale, closed, noRetries bool
		showMissing, checkUpdate, noTrigger bool
		wantPosts                           []string
		wantReads, timeoutAt                int
		wantOut, wantError                  string
	}{
		"should print PR commands by default":  {wantReads: 1, wantOut: "\n" + prCommand + "\n"},
		"should print tag commands by default": {tag: true, wantReads: 1, wantOut: "\n" + tagOne + "\n" + tagTwo + "\n"},
		"should post one multiline PR comment": {post: true, wantReads: 2, wantPosts: []string{prBatch}, wantOut: "Posted " + prLink + "1\n\nWarnings:"},
		"should print a retry after a GitHub check update 404": {
			checkUpdate: true, wantReads: 1, wantOut: "Post each retest command as a separate GitHub comment.\n" + prBatch + "\n",
		},
		"should post the recovered retry as a whole comment": {
			checkUpdate: true, post: true, wantReads: 2, wantPosts: []string{prCommand, prSecond}, wantOut: "Posted " + prLink + "1\nPosted " + prLink + "2\n",
		},
		"should describe the blocker when the recovered job has no matching trigger": {
			checkUpdate: true, noTrigger: true, wantReads: 1, wantOut: "No matching PR comment trigger was verified.",
		},
		"should post separate tag comments":            {tag: true, post: true, wantReads: 2, wantPosts: []string{tagOne, tagTwo}, wantOut: "Posted " + tagLink + "1\nPosted " + tagLink + "2\n"},
		"should withhold comments on a changed PR":     {post: true, stale: true, wantReads: 2, wantError: "PR closed or its head changed"},
		"should withhold comments on a closed PR":      {post: true, closed: true, wantReads: 2, wantError: "PR closed or its head changed"},
		"should withhold comments on a moved tag":      {tag: true, post: true, stale: true, wantReads: 2, wantError: "tag points to a different commit"},
		"should skip posting with no eligible jobs":    {post: true, noRetries: true, wantReads: 1, wantOut: "Next action: Wait"},
		"should report a tag timeout without retrying": {tag: true, post: true, timeoutAt: 2, wantReads: 2, wantPosts: []string{tagOne, tagTwo}, wantOut: "Posted " + tagLink + "1\n", wantError: `1 of 2 comment links confirmed: "` + tagTwo + `": GitHub request failed and the comment may have been posted`},
		"should report a PR batch timeout once":        {post: true, timeoutAt: 1, wantReads: 2, wantPosts: []string{prBatch}, wantOut: "No comments confirmed.", wantError: "0 of 1 comment links confirmed"},
		"should print commands for missing builds":     {showMissing: true, noRetries: true, wantReads: 1, wantOut: "Next action: Start missing builds\n" + prMissing + "\n"},
		"should print retries and missing builds":      {showMissing: true, wantReads: 1, wantOut: "\n" + prCommand + "\n" + prSecond + "\n\nStart missing builds:\n" + prMissing + "\n"},
		"should post missing builds with --post":       {post: true, showMissing: true, noRetries: true, wantReads: 2, wantPosts: []string{prMissing}, wantOut: "Next action: Start missing builds\nPosted " + prLink + "1\n"},
		"should batch retries and missing builds":      {post: true, showMissing: true, wantReads: 2, wantPosts: []string{prBatch + "\n" + prMissing}, wantOut: "Posted " + prLink + "1\n\nWarnings:"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			args := []string{"--repo", "scanner"}
			if tt.post {
				args = append(args, "--post")
			}
			refPath, postPath := repoPath+"/pulls/42", repoPath+"/issues/42/comments"
			event, branch := "pull_request", "master"
			if tt.tag {
				args = append(args, "--tags", tag)
				refPath, postPath = repoPath+"/commits/refs%2Ftags%2Frelease%2Fv1", repoPath+"/commits/"+sha+"/comments"
				event, branch = "push", "refs/tags/"+tag
			} else {
				args = append(args, "--prs", "42")
			}

			var posts []string
			reads := 0
			httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				path := r.URL.EscapedPath()
				if r.URL.Host == "api.github.com" && r.Header.Get("Authorization") != "Bearer test-token" {
					t.Fatalf("missing GitHub token for %s %s", r.Method, path)
				}
				status, body := http.StatusOK, ""
				switch {
				case r.Method == http.MethodPost && path == postPath:
					if r.URL.Host != "api.github.com" || r.Header.Get("Content-Type") != "application/json" || reads != 2 || len(posts) >= len(tt.wantPosts) {
						t.Fatalf("unexpected POST %s after %d reads and %d posts", r.URL, reads, len(posts))
					}
					defer r.Body.Close()
					var fields map[string]string
					if err := json.NewDecoder(r.Body).Decode(&fields); err != nil || len(fields) != 1 {
						t.Fatalf("invalid comment payload %v: %v", fields, err)
					}
					posts = append(posts, fields["body"])
					if len(posts) == tt.timeoutAt {
						return nil, context.DeadlineExceeded
					}
					status, body = http.StatusCreated, fmt.Sprintf(`{"id":%d}`, len(posts))
				case r.Method == http.MethodGet && path == refPath:
					reads++
					currentSHA, state := sha, "open"
					if reads == 2 && tt.stale {
						currentSHA = otherSHA
					}
					if reads == 2 && tt.closed {
						state = "closed"
					}
					if tt.tag {
						body = fmt.Sprintf(`{"sha":%q}`, currentSHA)
					} else {
						body = fmt.Sprintf(`{"id":123,"number":42,"state":%q,"head":{"sha":%q,"ref":"topic"}}`, state, currentSHA)
					}
				case r.Method == http.MethodGet && path == repoPath+"/commits/"+sha+"/check-runs":
					body = `{"check_runs":[{"id":1,"app":{"id":296509},"name":"Red Hat Konflux / scanner-on-push","details_url":"https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/one","status":"completed","conclusion":"failure"},{"id":2,"app":{"id":296509},"name":"Red Hat Konflux / scanner-slim-on-push","details_url":"https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/two","status":"completed","conclusion":"failure"}]}`
					if tt.noRetries {
						body = `{"check_runs":[]}`
					}
					if tt.checkUpdate {
						const github404 = "cannot use the API on the provider platform to create a in_progress status: PATCH https://api.github.com/repos/stackrox/scanner/check-runs/200: 404 Not Found []"
						body = fmt.Sprintf(`{"check_runs":[
							{"id":1,"app":{"id":296509},"name":"Red Hat Konflux / scanner-on-push","details_url":"https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/one","status":"completed","conclusion":"failure"},
							{"id":2,"app":{"id":296509},"name":"Red Hat Konflux / scanner-slim-on-push","head_sha":%q,"status":"completed","conclusion":"failure",
							 "pull_requests":[{"id":123,"number":42,"head":{"sha":%q,"ref":"topic"}}],"output":{"title":"pipelinerun start failure","text":%q}}]}`, sha, sha, github404)
					}
				case r.Method == http.MethodGet && strings.HasPrefix(path, repoPath+"/contents/.tekton/"):
					if tt.tag || r.URL.Query().Get("ref") != sha {
						t.Fatalf("unexpected PipelineRun lookup: %s", r.URL)
					}
					if tt.checkUpdate && !tt.noTrigger && path == repoPath+"/contents/.tekton/scanner-slim-build.yaml" {
						manifest := "apiVersion: tekton.dev/v1\nkind: PipelineRun\nmetadata:\n  name: scanner-slim-on-push\n  annotations:\n    pipelinesascode.tekton.dev/on-comment: '^" + prSecond + "$'\n"
						body = fmt.Sprintf(`{"encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte(manifest)))
					} else if tt.showMissing && path == repoPath+"/contents/.tekton/scanner-db-build.yaml" {
						manifest := "apiVersion: tekton.dev/v1\nkind: PipelineRun\nmetadata:\n  name: scanner-db-on-push\n  annotations:\n    pipelinesascode.tekton.dev/on-comment: '^" + prMissing + "$'\n"
						body = fmt.Sprintf(`{"encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte(manifest)))
					} else {
						status = http.StatusNotFound
					}
				case r.Method == http.MethodGet && strings.HasPrefix(path, "/apis/tekton.dev/v1/namespaces/builds/pipelineruns/"):
					name := strings.TrimPrefix(path, "/apis/tekton.dev/v1/namespaces/builds/pipelineruns/")
					id := "1"
					if name == "two" {
						id = "2"
					}
					body = fmt.Sprintf(`{"metadata":{"name":%q,"namespace":"builds","annotations":{"pipelinesascode.tekton.dev/check-run-id":%q,"pipelinesascode.tekton.dev/sha":%q,"pipelinesascode.tekton.dev/repo-url":"https://github.com/stackrox/scanner","pipelinesascode.tekton.dev/event-type":%q,"pipelinesascode.tekton.dev/branch":%q,"pipelinesascode.tekton.dev/pull-request":"42"}}}`, name, id, sha, event, branch)
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			var out, diagnostics strings.Builder
			code := run(t.Context(), args, &out, &diagnostics, konfluxretest.Config{HTTPClient: httpClient, KonfluxToken: fixtureKonfluxToken})
			if !slices.Equal(posts, tt.wantPosts) || reads != tt.wantReads || !strings.Contains(out.String(), tt.wantOut) {
				t.Fatalf("reads=%d posts=%q stdout=%q stderr=%q", reads, posts, out.String(), diagnostics.String())
			}
			if tt.noTrigger && strings.Contains(out.String(), "\n"+prSecond+"\n") {
				t.Fatalf("unexpected retry for job without a matching trigger: %s", out.String())
			}
			if tt.wantError == "" {
				if code != 0 || diagnostics.Len() != 0 {
					t.Fatalf("code=%d stderr=%q", code, diagnostics.String())
				}
			} else if code != 1 || !strings.Contains(diagnostics.String(), tt.wantError) {
				t.Fatalf("code=%d stderr=%q; want %q", code, diagnostics.String(), tt.wantError)
			}
			if tt.post && (strings.Contains(out.String(), "Post each command") || strings.Contains(out.String(), "\n/retest ") || strings.Contains(out.String(), "\n/konflux-retest ")) {
				t.Fatalf("unexpected manual instructions in posting mode: %s", out.String())
			}
		})
	}
}
