package konfluxretest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestRunIdentityArchiveTimeoutBudget(t *testing.T) {
	const pipeline = `{"metadata":{"name":"run","namespace":"builds","annotations":{
		"pipelinesascode.tekton.dev/check-run-id":"123",
		"pipelinesascode.tekton.dev/sha":"head",
		"pipelinesascode.tekton.dev/repo-url":"https://github.com/stackrox/fact",
		"pipelinesascode.tekton.dev/event-type":"push",
		"pipelinesascode.tekton.dev/branch":"refs/tags/v1.2.3"
	}}}`
	archive, err := json.Marshal(map[string]any{"records": []any{map[string]any{"data": map[string]any{"value": []byte(pipeline)}}}})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		callerTimeout, clientTimeout, liveBudget time.Duration
		cancelLive, stallArchive                 bool
		wantError                                error
	}{
		"should bound the live request without a caller deadline": {liveBudget: 5 * time.Second},
		"should reserve time within a short caller deadline":      {callerTimeout: 4 * time.Second, liveBudget: 2 * time.Second},
		"should keep a shorter HTTP client timeout":               {clientTimeout: time.Second, liveBudget: time.Second},
		"should stop before fallback when the caller cancels":     {liveBudget: 5 * time.Second, cancelLive: true, wantError: context.Canceled},
		"should stop the archive at the caller deadline":          {callerTimeout: 4 * time.Second, liveBudget: 2 * time.Second, stallArchive: true, wantError: context.DeadlineExceeded},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				if tc.callerTimeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.callerTimeout)
					defer cancel()
				}
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				start := time.Now()
				var liveCalls, archiveCalls int
				client := &konfluxClient{
					token: func(context.Context, string) (string, error) { return "fixture", nil },
					http: &http.Client{Timeout: tc.clientTimeout, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
						switch r.URL.Host {
						case "api.example.openshiftapps.com":
							liveCalls++
							if deadline, ok := r.Context().Deadline(); !ok || deadline.Sub(start) != tc.liveBudget {
								t.Fatalf("live deadline = %v, want %s from start", deadline, tc.liveBudget)
							}
							if tc.cancelLive {
								cancel()
							}
							<-r.Context().Done()
							return nil, r.Context().Err()
						case "tekton-results-tekton-results.apps.example.openshiftapps.com":
							archiveCalls++
							if r.Context().Err() != nil || time.Since(start) != tc.liveBudget {
								t.Fatalf("archive started after %s with context error %v", time.Since(start), r.Context().Err())
							}
							wantDeadline, wantDeadlineSet := ctx.Deadline()
							if tc.clientTimeout > 0 {
								wantDeadline, wantDeadlineSet = time.Now().Add(tc.clientTimeout), true
							}
							if deadline, ok := r.Context().Deadline(); ok != wantDeadlineSet || !deadline.Equal(wantDeadline) {
								t.Fatalf("archive deadline = %v, want %v", deadline, wantDeadline)
							}
							if rejection := invalidArchivePageSize(r); rejection != nil {
								return rejection, nil
							}
							if tc.stallArchive {
								<-r.Context().Done()
								return nil, r.Context().Err()
							}
							return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(archive)))}, nil
						default:
							t.Fatalf("unexpected host %s", r.URL.Host)
							return nil, nil
						}
					})},
				}
				check := checkRun{ID: 123, DetailsURL: "https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/run"}
				observation, err := client.runIdentity(ctx, check, "stackrox/fact", "head", "v1.2.3", 0)
				if !errors.Is(err, tc.wantError) || observation.matches != (tc.wantError == nil) {
					t.Fatalf("observation=%+v error=%v, want error %v", observation, err, tc.wantError)
				}
				if tc.stallArchive && time.Since(start) != tc.callerTimeout {
					t.Fatalf("archive stopped after %s, want caller deadline at %s", time.Since(start), tc.callerTimeout)
				}
				wantArchiveCalls := 1
				if tc.cancelLive {
					wantArchiveCalls = 0
				}
				if liveCalls != 1 || archiveCalls != wantArchiveCalls {
					t.Fatalf("live calls=%d archive calls=%d, want 1 and %d", liveCalls, archiveCalls, wantArchiveCalls)
				}
			})
		})
	}
}

func TestRunIdentity(t *testing.T) {
	for name, tc := range map[string]struct {
		event, branch, number, tag string
		pr                         int
		want                       bool
	}{
		"tag":                          {"push", "refs/tags/v1.2.3", "", "v1.2.3", 0, true},
		"branch on same commit":        {"push", "release-1.2", "", "v1.2.3", 0, false},
		"different tag on same commit": {"push", "refs/tags/v1.2.4", "", "v1.2.3", 0, false},
		"PR":                           {"pull_request", "release-1.2", "42", "", 42, true},
		"different PR":                 {"pull_request", "release-1.2", "43", "", 42, false},
	} {
		t.Run(name, func(t *testing.T) {
			const p = "pipelinesascode.tekton.dev/"
			annotations := map[string]string{p + "check-run-id": "123", p + "sha": "head", p + "repo-url": "https://github.com/stackrox/fact", p + "event-type": tc.event, p + "branch": tc.branch, p + "pull-request": tc.number}
			data, _ := json.Marshal(map[string]any{"metadata": map[string]any{"name": "run", "namespace": "builds", "annotations": annotations}})
			ok, err := matchRunIdentity(data, "builds", "run", 123, "stackrox/fact", "head", tc.tag, tc.pr)
			if err != nil || ok != tc.want {
				t.Fatal(ok, err)
			}
			if _, err := matchRunIdentity(data, "builds", "run", 124, "stackrox/fact", "head", tc.tag, tc.pr); err == nil {
				t.Fatal("accepted wrong check ID")
			}
		})
	}
}

func TestCommentRunIdentity(t *testing.T) {
	for name, tc := range map[string]struct {
		event, branch, sourceBranch, sourceRepo, number, tag string
		want, unknown                                        bool
	}{
		"PR comment": {
			event: "on-comment", branch: "release-1.2", sourceBranch: "fix", number: "42", want: true,
		},
		"PR comment from a fork with the same branch name": {
			event: "on-comment", branch: "release-1.2", sourceBranch: "release-1.2", sourceRepo: "https://github.com/contributor/fact", number: "42", want: true,
		},
		"comment on a different PR": {
			event: "on-comment", branch: "release-1.2", sourceBranch: "fix", number: "43",
		},
		"branch commit comment associated with the requested PR": {
			event: "on-comment", branch: "refs/heads/release-1.2", sourceBranch: "release-1.2", sourceRepo: "https://github.com/stackrox/fact", number: "42",
		},
		"comment with no source branch": {
			event: "on-comment", branch: "release-1.2", number: "42", unknown: true,
		},
		"comment with equal branches and no source repository": {
			event: "on-comment", branch: "release-1.2", sourceBranch: "release-1.2", number: "42", unknown: true,
		},
		"tag retest": {
			event: "retest-comment", branch: "refs/tags/v1.2.3", tag: "v1.2.3", want: true,
		},
		"tag retest associated with the requested PR": {
			event: "retest-comment", branch: "refs/tags/v1.2.3", number: "42",
		},
		"branch retest for a different ref": {
			event: "retest-comment", branch: "release-1.2", tag: "v1.2.3",
		},
	} {
		t.Run(name, func(t *testing.T) {
			const p = "pipelinesascode.tekton.dev/"
			annotations := map[string]string{
				p + "check-run-id": "123", p + "sha": "head", p + "repo-url": "https://github.com/stackrox/fact",
				p + "event-type": tc.event, p + "branch": tc.branch, p + "source-branch": tc.sourceBranch,
				p + "source-repo-url": tc.sourceRepo, p + "pull-request": tc.number,
			}
			data, err := json.Marshal(map[string]any{"metadata": map[string]any{"name": "run", "namespace": "builds", "annotations": annotations}})
			if err != nil {
				t.Fatal(err)
			}
			matches, err := matchRunIdentity(data, "builds", "run", 123, "stackrox/fact", "head", tc.tag, 42)
			if matches != tc.want || (err != nil) != tc.unknown {
				t.Fatalf("matches=%t error=%v, want matches=%t unknown=%t", matches, err, tc.want, tc.unknown)
			}
		})
	}
}
