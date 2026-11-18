package konfluxretest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestIdentityErrorsRetainCheckContextAndBothServiceFailures(t *testing.T) {
	checks := []checkRun{
		{ID: 17, Name: checkPrefix + "fact-on-push", DetailsURL: "https://user:password@old.example/run?token=query-secret#fragment-secret"},
		{ID: 18, Name: checkPrefix + "collector-on-push", DetailsURL: "https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/build-18"},
		{ID: 19, Name: checkPrefix + "acs-conforma-dev / main", Status: "completed", Conclusion: "failure", DetailsURL: "https://old.example/advisory?token=query-secret"},
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
		"HTTP 404 (Not Found)",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("missing %q in error:\n%s", want, message)
		}
	}
	visible := fmt.Sprintf("%+v %v", result, err)
	for _, private := range []string{"user:password", "query-secret", "fragment-secret", "cluster-secret", "?filter=", "User builder", "archive records not found"} {
		if strings.Contains(visible, private) {
			t.Fatalf("unexpected upstream data in report: %q", private)
		}
	}
	if requests != 3 || len(result.Jobs) != 0 || len(result.Runs) != 2 {
		t.Fatalf("requests=%d result=%+v", requests, result)
	}
	for _, run := range result.Runs {
		if run.IdentityKnown || run.Eligible || run.Status != "unknown" {
			t.Fatalf("failed identity authorized a retry: %+v", run)
		}
		if run.ID == 17 && run.URL != "https://old.example/run" {
			t.Fatalf("unexpected check URL: %q", run.URL)
		}
	}
	if len(result.Advisories) != 1 || result.Advisories[0].URL != "https://old.example/advisory" {
		t.Fatalf("unexpected advisory: %+v", result.Advisories)
	}
}

func TestIdentityNetworkFailuresPreserveContext(t *testing.T) {
	for name, cause := range map[string]error{
		"should retain deadline expiration": context.DeadlineExceeded,
		"should retain cancellation":        context.Canceled,
	} {
		t.Run(name, func(t *testing.T) {
			client := &konfluxClient{http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, cause })}, token: func(context.Context, string) (string, error) { return "token", nil }}
			_, err := client.runIdentity(t.Context(), checkRun{DetailsURL: "https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/run"}, "stackrox/fact", "head", "", 1)
			if !errors.Is(err, cause) {
				t.Fatalf("lost cause: %v", err)
			}
			if _, ok := errors.AsType[*url.Error](err); ok {
				t.Fatal("transport error is reachable through the error chain")
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
		"should identify an invalid namespace": {"https://konflux-ui.apps.example.openshiftapps.com/ns/INVALID/pipelinerun/run", "namespace, expected a lowercase Kubernetes resource name"},
		"should explain the rejected scheme":   {"http://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/run", "scheme, expected HTTPS"},
		"should identify the unsupported host": {"https://old.example/run", "unsupported PipelineRun URL host"},
		"should explain the unsupported path":  {"https://konflux-ui.apps.example.openshiftapps.com/new/path", "expected /ns/<namespace>/pipelinerun/<name>"},
		"should explain an explicit port":      {"https://konflux-ui.apps.example.openshiftapps.com:443/ns/builds/pipelinerun/run", "explicit port"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := new(konfluxClient).runIdentity(t.Context(), checkRun{DetailsURL: tc.address}, "stackrox/fact", "head", "", 1)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
	t.Run("should summarize token provider failure", func(t *testing.T) {
		cause := errors.New("oc session expired; log in to api.example.openshiftapps.com")
		client := &konfluxClient{token: func(context.Context, string) (string, error) { return "", cause }}
		_, err := client.runIdentity(t.Context(), checkRun{DetailsURL: "https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/run"}, "stackrox/fact", "head", "", 1)
		if err == nil || !strings.Contains(err.Error(), "authentication failed for api.example.openshiftapps.com") ||
			errors.Is(err, cause) || strings.Contains(err.Error(), cause.Error()) {
			t.Fatalf("unexpected authentication diagnostic: %v", err)
		}
	})
}

func TestReportURL(t *testing.T) {
	for name, tt := range map[string]struct{ input, want string }{
		"should preserve the HTTPS path": {"https://example.test/ns/builds/run", "https://example.test/ns/builds/run"},
		"should omit URL credentials":    {"https://user:password@example.test/run?token=secret#fragment", "https://example.test/run"},
		"should omit a non-HTTPS link":   {"http://example.test/run", ""},
		"should omit a relative link":    {"/run", ""},
		"should omit a malformed URL":    {"https://%zz", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := reportURL(tt.input); got != tt.want {
				t.Fatalf("reportURL=%q, want %q", got, tt.want)
			}
		})
	}
}
