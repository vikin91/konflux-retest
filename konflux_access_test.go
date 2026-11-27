package konfluxretest_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/vikin91/konflux-retest"
)

func TestCheckKonfluxAccess(t *testing.T) {
	t.Parallel()
	const host, namespace, appID = "api.cluster.example.com", "builds", 123
	const token = "fixture-secret"
	for name, tc := range map[string]struct {
		status       int
		body         string
		transportErr error
		denyPolicy   bool
		want         konfluxretest.KonfluxAccessFailure
	}{
		"should accept a valid credential":       {status: http.StatusCreated, body: `{"status":{"allowed":true}}`},
		"should reject an invalid credential":    {status: http.StatusUnauthorized, body: token, want: konfluxretest.KonfluxAccessAuthentication},
		"should report denied namespace access":  {status: http.StatusCreated, body: `{"status":{"allowed":false,"reason":"` + token + `"}}`, want: konfluxretest.KonfluxAccessForbidden},
		"should report refused review":           {status: http.StatusForbidden, want: konfluxretest.KonfluxAccessForbidden},
		"should report an unreachable host":      {transportErr: errors.New("network unreachable: " + token), want: konfluxretest.KonfluxAccessTransport},
		"should preserve a deadline":             {transportErr: context.DeadlineExceeded, want: konfluxretest.KonfluxAccessTransport},
		"should reject an incomplete response":   {status: http.StatusCreated, body: `{}`, want: konfluxretest.KonfluxAccessReview},
		"should report an inconclusive review":   {status: http.StatusCreated, body: `{"status":{"allowed":false,"evaluationError":"failed"}}`, want: konfluxretest.KonfluxAccessReview},
		"should stop at a cluster redirect":      {status: http.StatusTemporaryRedirect, want: konfluxretest.KonfluxAccessReview},
		"should apply policy before credentials": {denyPolicy: true, want: konfluxretest.KonfluxAccessForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var tokenCalls, requests int
			config := konfluxretest.Config{
				AllowKonfluxLookup: func(_ context.Context, id int64, gotHost, gotNamespace string) bool {
					if id != appID || gotHost != host || gotNamespace != namespace {
						t.Fatalf("policy got (%d, %q, %q)", id, gotHost, gotNamespace)
					}
					return !tc.denyPolicy
				},
				KonfluxToken: func(_ context.Context, gotHost string) (string, error) {
					tokenCalls++
					if gotHost != host {
						t.Fatalf("token provider got host %q", gotHost)
					}
					return token, nil
				},
				HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					requests++
					const endpoint = "https://" + host + "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews"
					if r.Method != http.MethodPost || r.URL.String() != endpoint || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Content-Type") != "application/json" {
						t.Fatalf("unexpected review request %s %s", r.Method, r.URL)
					}
					defer r.Body.Close()
					var review struct {
						APIVersion, Kind string
						Spec             struct{ ResourceAttributes map[string]string }
					}
					if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
						t.Fatal(err)
					}
					attrs := review.Spec.ResourceAttributes
					if review.APIVersion != "authorization.k8s.io/v1" || review.Kind != "SelfSubjectAccessReview" || attrs["namespace"] != namespace || attrs["verb"] != "get" || attrs["group"] != "tekton.dev" || attrs["version"] != "v1" || attrs["resource"] != "pipelineruns" {
						t.Fatalf("unexpected authorization review %+v", review)
					}
					if tc.transportErr != nil {
						return nil, tc.transportErr
					}
					return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: http.Header{"Location": {"https://other.example.com/review"}}}, nil
				})},
			}
			err := konfluxretest.CheckKonfluxAccess(t.Context(), config, appID, host, namespace)
			wantCalls := 1
			if tc.denyPolicy {
				wantCalls = 0
			}
			if tokenCalls != wantCalls || requests != wantCalls {
				t.Fatalf("got %d token calls and %d requests, want %d each", tokenCalls, requests, wantCalls)
			}
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			failure, ok := errors.AsType[*konfluxretest.KonfluxAccessError](err)
			if !ok || failure.Failure != tc.want || failure.Host != host || failure.Namespace != namespace {
				t.Fatalf("got %v, want %s for %s/%s", err, tc.want, host, namespace)
			}
			if !strings.Contains(err.Error(), host) || !strings.Contains(err.Error(), namespace) || strings.Contains(err.Error(), token) {
				t.Fatalf("unexpected diagnostic %q", err)
			}
			if tc.transportErr == context.DeadlineExceeded && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected deadline cause, got %v", err)
			}
		})
	}
}
