package konfluxretest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestIdentityErrorsRetainCheckContextAndBothServiceFailures(t *testing.T) {
	checks := []checkRun{
		{ID: 17, Name: checkPrefix + "fact-on-push", DetailsURL: "https://user:password@old.example/run?token=query-secret#fragment-secret"},
		{ID: 18, Name: checkPrefix + "collector-on-push", DetailsURL: "https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/build-18"},
	}
	requests := 0
	config := Config{VerifyRunIdentity: true, KonfluxToken: func(context.Context, string) (string, error) { return "cluster-secret", nil }, HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		status, body := http.StatusOK, ""
		switch r.URL.Host {
		case "api.github.com":
			data, _ := json.Marshal(map[string]any{"check_runs": checks})
			body = string(data)
		case "api.example.openshiftapps.com":
			status, body = http.StatusForbidden, `{"message":"pipelineruns.tekton.dev is forbidden: User builder cannot get resource; token cluster-secret"}`
		case "tekton-results-tekton-results.apps.example.openshiftapps.com":
			status, body = http.StatusNotFound, `{"error":{"message":"archive records not found"}}`
		default:
			t.Fatalf("unexpected request: %s", r.URL)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}}
	client, checker := config.clients()
	result, err := checkCommit(t.Context(), client, checker, "stackrox/stackrox", "stackrox/stackrox", Result{SHA: "head"})
	if err == nil {
		t.Fatal("identity failures were lost")
	}
	message := err.Error()
	for _, want := range []string{
		"identity verification failed for 2 checks", "1 API/archive failure", "1 unsupported URL",
		`check "Red Hat Konflux / fact-on-push" (run 17, URL "https://old.example/run")`,
		`check "Red Hat Konflux / collector-on-push" (run 18`,
		"embedded credentials are not allowed", "API: GET https://api.example.openshiftapps.com/apis/tekton.dev/v1/namespaces/builds/pipelineruns/build-18 returned HTTP 403 (Forbidden)",
		"User builder cannot get resource", "HTTP 404 (Not Found)", "archive records not found", "[redacted]",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("missing %q in error:\n%s", want, message)
		}
	}
	for _, secret := range []string{"user:password", "query-secret", "fragment-secret", "cluster-secret", "?filter="} {
		if strings.Contains(message, secret) || strings.Contains(strings.Join(result.Warnings, "\n"), secret) {
			t.Fatalf("credential or query leaked: %s", secret)
		}
	}
	if requests != 3 || len(result.Jobs) != 0 || len(result.Runs) != 2 {
		t.Fatalf("requests=%d result=%+v", requests, result)
	}
	for _, run := range result.Runs {
		if run.IdentityKnown || run.Eligible || run.Status != "unknown" {
			t.Fatalf("failed identity authorized a retry: %+v", run)
		}
	}
	t.Log(message)
}

func TestIdentityNetworkFailuresKeepTheirCauses(t *testing.T) {
	for name, cause := range map[string]error{
		"should retain deadline expiration": context.DeadlineExceeded,
		"should retain cancellation":        context.Canceled,
		"should retain DNS failure":         &net.DNSError{Err: "no such host", Name: "api.example.openshiftapps.com"},
	} {
		t.Run(name, func(t *testing.T) {
			client := &konfluxClient{http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, cause })}, token: func(context.Context, string) (string, error) { return "token", nil }}
			_, err := client.runIdentity(t.Context(), checkRun{DetailsURL: "https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/run"}, "stackrox/fact", "head", "", 1)
			if !errors.Is(err, cause) {
				t.Fatalf("lost cause: %v", err)
			}
			for _, want := range []string{cause.Error(), "API: GET https://api.example.openshiftapps.com/", "archive: GET https://tekton-results-tekton-results.apps.example.openshiftapps.com/"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("missing %q in %v", want, err)
				}
			}
		})
	}
}

func TestIdentityAuthenticationAndURLFailuresExplainTheCause(t *testing.T) {
	for name, tc := range map[string]struct{ address, want string }{
		"should identify an invalid namespace": {"https://konflux-ui.apps.example.openshiftapps.com/ns/INVALID/pipelinerun/run", `namespace "INVALID"; expected a lowercase Kubernetes resource name`},
		"should explain the rejected scheme":   {"http://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/run", `scheme "http"; expected https`},
		"should identify the unsupported host": {"https://old.example/run", `host "old.example"`},
		"should explain the unsupported path":  {"https://konflux-ui.apps.example.openshiftapps.com/new/path", "expected /ns/<namespace>/pipelinerun/<name>"},
		"should explain an explicit port":      {"https://konflux-ui.apps.example.openshiftapps.com:443/ns/builds/pipelinerun/run", `port "443"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := new(konfluxClient).runIdentity(t.Context(), checkRun{DetailsURL: tc.address}, "stackrox/fact", "head", "", 1)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
	t.Run("should preserve token provider failure", func(t *testing.T) {
		cause := errors.New("oc session expired; log in to api.example.openshiftapps.com")
		client := &konfluxClient{token: func(context.Context, string) (string, error) { return "", cause }}
		_, err := client.runIdentity(t.Context(), checkRun{DetailsURL: "https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/run"}, "stackrox/fact", "head", "", 1)
		if !errors.Is(err, cause) || !strings.Contains(err.Error(), cause.Error()) {
			t.Fatalf("lost authentication cause: %v", err)
		}
	})
}

func TestKonfluxHTTPDiagnosticsPreserveServerMessagesAndBoundBodies(t *testing.T) {
	for name, body := range map[string]string{
		"Kubernetes status":      `{"message":"User builder cannot get pipelineruns"}`,
		"archive string error":   `{"error":"User builder cannot get pipelineruns"}`,
		"signed URL in message":  `{"message":"User builder cannot get pipelineruns from https://storage.example/log?sig=signed-secret#body-fragment"}`,
		"plain text proxy error": "User builder cannot get pipelineruns",
		"oversized error":        "User builder cannot get pipelineruns " + strings.Repeat("x", 5000),
	} {
		t.Run(name, func(t *testing.T) {
			err := konfluxHTTPFailure(&http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(body))}, "https://user:password@api.example/path?token=query-secret", "cluster-secret")
			if !strings.Contains(err.Error(), "HTTP 403 (Forbidden)") || !strings.Contains(err.Error(), "User builder cannot get pipelineruns") {
				t.Fatalf("unhelpful or unsafe error: %v", err)
			}
			for _, secret := range []string{"password", "query-secret", "signed-secret", "body-fragment"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaked %q: %v", secret, err)
				}
			}
			if len(body) > 4096 && !strings.Contains(err.Error(), "truncated") {
				t.Fatalf("unmarked truncation: %v", err)
			}
		})
	}
	t.Run("should redact nested transport URL credentials", func(t *testing.T) {
		cause := &url.Error{Op: "Get", URL: "https://user:password@example/path?token=query-secret", Err: context.DeadlineExceeded}
		err := preserveCause(fmt.Errorf("request: %w", cause), "")
		if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "query-secret") {
			t.Fatalf("lost cause or leaked URL: %v", err)
		}
	})
	t.Run("should omit malformed redirect locations", func(t *testing.T) {
		cause := errors.New(`failed to parse Location header "/log%ZZ?sig=signed-secret": invalid URL escape`)
		err := preserveCause(&url.Error{Op: "Get", URL: "https://api.example/path", Err: cause}, "")
		if !errors.Is(err, cause) || err.Error() != "invalid HTTP redirect Location header" {
			t.Fatalf("lost cause or exposed the malformed location: %v", err)
		}
	})
}
