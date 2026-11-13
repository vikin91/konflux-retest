package konfluxretest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestCheckPRUsesOptionalIntegrations(t *testing.T) {
	files := requirementFiles(t)
	checks := successfulChecks()
	for i := range checks {
		if checks[i].Name == checkPrefix+"retag-scanner" {
			checks[i].Conclusion = "failure"
			checks[i].DetailsURL = "https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/run"
		}
	}
	var tokenCalled, pullCalled bool
	config := Config{
		KonfluxToken: func(_ context.Context, host string) (string, error) {
			tokenCalled = true
			if host != "api.example.openshiftapps.com" {
				t.Errorf("unexpected host: %s", host)
			}
			return "", errors.New("no cluster login")
		},
		PullImage: func(_ context.Context, image string) (string, error) {
			pullCalled = true
			if image != "quay.io/rhacs-eng/release-scanner:2.39.27-fast" {
				t.Errorf("unexpected image: %s", image)
			}
			return "image is missing (docker pull)", nil
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			var body []byte
			switch {
			case r.URL.Path == "/repos/stackrox/stackrox/pulls/1":
				body = []byte(`{"state":"open","head":{"sha":"head"}}`)
			case r.URL.Path == "/repos/stackrox/stackrox/commits/head/check-runs":
				body, _ = json.Marshal(map[string]any{"check_runs": checks})
			case strings.HasPrefix(r.URL.Path, "/repos/stackrox/stackrox/contents/"):
				path := strings.TrimPrefix(r.URL.Path, "/repos/stackrox/stackrox/contents/")
				if r.URL.Query().Get("ref") != "head" {
					t.Fatal("requirements are not pinned")
				}
				body, _ = json.Marshal(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString(files[path])})
			default:
				t.Fatalf("unexpected request: %s", r.URL)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
		})},
	}
	result, err := CheckPR(t.Context(), config, "stackrox/stackrox", 1)
	if err != nil || !tokenCalled || !pullCalled || result.Action != ActionInvestigateMissingImages || len(result.Jobs) != 0 {
		t.Fatalf("result=%+v error=%v tokenCalled=%v pullCalled=%v", result, err, tokenCalled, pullCalled)
	}
}

func TestCheckPRRetainsUnaffectedJobsOnInputError(t *testing.T) {
	checks := successfulChecks()
	for i := range checks {
		if slices.Contains([]string{checkPrefix + "retag-scanner", checkPrefix + "roxctl-on-push"}, checks[i].Name) {
			checks[i].Conclusion = "failure"
		}
	}
	config := Config{HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"state":"open","head":{"sha":"head"}}`
		status := http.StatusOK
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			data, _ := json.Marshal(map[string]any{"check_runs": checks})
			body = string(data)
		case strings.Contains(r.URL.Path, "/contents/"):
			status = http.StatusServiceUnavailable
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	result, err := CheckPR(t.Context(), config, "stackrox/stackrox", 1)
	if err == nil || result.Action != ActionRetest || !slices.Equal(result.Jobs, []string{"roxctl-on-push"}) || !strings.Contains(strings.Join(result.Warnings, "\n"), "HTTP 503") {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

func TestConfigProtectsRedirectCredentials(t *testing.T) {
	client, checker := (Config{}).clients()
	req, err := http.NewRequest(http.MethodGet, "https://storage.example/image", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.http.CheckRedirect(req, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("GitHub redirect: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	if err := checker.registry.http.CheckRedirect(req, nil); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatal("registry credential retained on redirect")
	}
	req.URL.Scheme = "http"
	if checker.registry.http.CheckRedirect(req, nil) == nil {
		t.Fatal("insecure redirect accepted")
	}
}
