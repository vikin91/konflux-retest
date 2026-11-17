package konfluxretest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestImageOutputsRequireVerifiedRunIdentity(t *testing.T) {
	const image = "quay.io/rhacs-eng/release-main:observed-build-fast"
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for name, tc := range map[string]struct {
		archive, legacy, mismatch, wrongPR, unverified, duplicate, structured, missingDigest, unavailable bool
	}{
		"API results":                 {},
		"archived legacy results":     {archive: true, legacy: true},
		"wrong commit":                {mismatch: true},
		"different PR":                {wrongPR: true},
		"verification disabled":       {unverified: true},
		"ambiguous output":            {duplicate: true},
		"non-string output":           {structured: true},
		"missing digest":              {missingDigest: true},
		"API and archive unavailable": {unavailable: true},
	} {
		t.Run(name, func(t *testing.T) {
			const prefix = "pipelinesascode.tekton.dev/"
			a := map[string]string{prefix + "check-run-id": "17", prefix + "sha": "head", prefix + "repo-url": "https://github.com/stackrox/stackrox", prefix + "event-type": "pull_request", prefix + "branch": "release-5.0", prefix + "pull-request": "123"}
			if tc.mismatch {
				a[prefix+"sha"] = "another-head"
			}
			if tc.wrongPR {
				a[prefix+"pull-request"] = "124"
			}
			var imageValue any = image
			if tc.structured {
				imageValue = []string{image}
			}
			outputs := []any{map[string]any{"name": "IMAGE_URL", "value": imageValue}}
			if !tc.missingDigest {
				outputs = append(outputs, map[string]any{"name": "IMAGE_DIGEST", "value": digest})
			}
			if tc.duplicate {
				outputs = append(outputs, map[string]any{"name": "IMAGE_URL", "value": "quay.io/rhacs-eng/release-main:other"})
			}
			field := "results"
			if tc.legacy {
				field = "pipelineResults"
			}
			pipeline, err := json.Marshal(map[string]any{"metadata": map[string]any{"name": "run", "namespace": "builds", "annotations": a}, "status": map[string]any{field: outputs}})
			if err != nil {
				t.Fatal(err)
			}
			requests := 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				status := http.StatusOK
				var value any
				switch r.URL.Host {
				case "api.github.com":
					switch {
					case strings.HasSuffix(r.URL.Path, "/pulls/123"):
						value = map[string]any{"state": "open", "head": map[string]any{"sha": "head", "repo": map[string]string{"full_name": "stackrox/stackrox"}}}
					case strings.HasSuffix(r.URL.Path, "/check-runs"):
						value = map[string]any{"check_runs": []any{map[string]any{"id": 17, "name": checkPrefix + "main-on-push", "status": "completed", "conclusion": "failure", "details_url": "https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/run"}}}
					default:
						t.Fatalf("unexpected GitHub path %s", r.URL.Path)
					}
				case "api.example.openshiftapps.com":
					requests++
					if tc.archive || tc.unavailable {
						status = http.StatusNotFound
						value = map[string]string{}
					} else {
						value = json.RawMessage(pipeline)
					}
				case "tekton-results-tekton-results.apps.example.openshiftapps.com":
					requests++
					if tc.unavailable {
						status = http.StatusForbidden
						value = map[string]string{}
					} else {
						value = map[string]any{"records": []any{map[string]any{"data": map[string]any{"value": pipeline}}}}
					}
				default:
					t.Fatalf("unexpected service %s", r.URL.Host)
				}
				body, marshalErr := json.Marshal(value)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
			})}
			result, err := CheckPR(t.Context(), Config{VerifyRunIdentity: !tc.unverified, HTTPClient: client, KonfluxToken: func(context.Context, string) (string, error) { return "fixture", nil }}, "stackrox/stackrox", 123)
			if (err != nil) != (tc.mismatch || tc.unavailable) {
				t.Fatalf("observation error=%v", err)
			}
			if tc.wrongPR {
				if len(result.Runs) != 0 {
					t.Fatal("different PR returned outputs", result.Runs)
				}
				return
			}
			if len(result.Runs) != 1 {
				t.Fatalf("runs=%+v", result.Runs)
			}
			run := result.Runs[0]
			known := !tc.unverified && !tc.mismatch && !tc.unavailable
			if run.IdentityKnown != known {
				t.Fatalf("identity known=%t", run.IdentityKnown)
			}
			wantImage, wantDigest := image, digest
			if !known || tc.duplicate || tc.structured {
				wantImage, wantDigest = "", ""
			}
			if tc.missingDigest {
				wantDigest = ""
			}
			if run.ImageURL != wantImage || run.ImageDigest != wantDigest {
				t.Fatalf("outputs=%q %q, want %q %q", run.ImageURL, run.ImageDigest, wantImage, wantDigest)
			}
			if known && !slices.Contains(result.Jobs, "main-on-push") {
				t.Fatal("image output changed retry eligibility", result)
			}
			if tc.unverified && requests != 0 {
				t.Fatal("non-strict mode read run outputs")
			}
		})
	}
}
