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
		archive, wantError                string
		outputs                           map[string]string
		legacy, mismatch, wrongPR         bool
		unverified, duplicate, structured bool
		missingDigest                     bool
	}{
		"API results":                 {},
		"archived legacy results":     {archive: "valid", legacy: true},
		"wrong commit":                {mismatch: true, wantError: "PipelineRun SHA annotation"},
		"archive wrong commit":        {archive: "valid", mismatch: true, wantError: "PipelineRun SHA annotation"},
		"different PR":                {archive: "valid", wrongPR: true},
		"archive missing PR number":   {archive: "missing PR", wantError: "PR event identity unavailable"},
		"archive malformed PR number": {archive: "malformed PR", wantError: "PR event identity unavailable"},
		"archive multiple records":    {archive: "multiple", wantError: "records=2, more_pages=false"},
		"archive continuation token":  {archive: "paginated", wantError: "records=1, more_pages=true"},
		"archive missing records":     {archive: "missing", wantError: "records=0, more_pages=false"},
		"archive malformed record":    {archive: "malformed", wantError: "decode PipelineRun identity"},
		"archive malformed response":  {archive: "bad envelope", wantError: "decode PipelineRun archive response"},
		"verification disabled":       {unverified: true},
		"ambiguous output":            {duplicate: true},
		"non-string output":           {structured: true},
		"missing digest":              {missingDigest: true},
		"signed URL instead of image": {outputs: map[string]string{"IMAGE_URL": "https://registry.example/image?sig=private"}},
		"invalid digest":              {outputs: map[string]string{"IMAGE_DIGEST": "fixture-private-value"}},
		"API and archive unavailable": {archive: "forbidden", wantError: "HTTP 403 (Forbidden)"},
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
			switch tc.archive {
			case "missing PR":
				delete(a, prefix+"pull-request")
			case "malformed PR":
				a[prefix+"pull-request"] = "not-a-number"
			}
			var imageValue any = image
			if value, ok := tc.outputs["IMAGE_URL"]; ok {
				imageValue = value
			}
			if tc.structured {
				imageValue = []string{image}
			}
			outputs := []any{map[string]any{"name": "IMAGE_URL", "value": imageValue}}
			if !tc.missingDigest {
				value := digest
				if override, ok := tc.outputs["IMAGE_DIGEST"]; ok {
					value = override
				}
				outputs = append(outputs, map[string]any{"name": "IMAGE_DIGEST", "value": value})
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
					if tc.archive != "" {
						status = http.StatusNotFound
						value = map[string]string{}
					} else {
						value = json.RawMessage(pipeline)
					}
				case "tekton-results-tekton-results.apps.example.openshiftapps.com":
					requests++
					if rejection := invalidArchivePageSize(r); rejection != nil {
						return rejection, nil
					}
					if !strings.Contains(r.URL.Query().Get("filter"), `data.metadata.name == "run"`) || r.URL.Query().Has("page_token") {
						t.Fatal("unexpected archive query", r.URL.RawQuery)
					}
					if tc.archive == "forbidden" {
						status = http.StatusForbidden
						value = map[string]string{}
					} else {
						if tc.archive == "malformed" {
							pipeline = []byte(`{`)
						}
						record := map[string]any{"data": map[string]any{"value": pipeline}}
						archive := map[string]any{"records": []any{record}}
						switch tc.archive {
						case "multiple":
							archive["records"] = []any{record, record}
						case "missing":
							archive["records"] = []any{}
						case "paginated":
							archive["next_page_token"] = "another-page"
						case "bad envelope":
							archive["records"] = "not a list"
						}
						value = archive
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
			if tc.wantError == "" && err != nil || tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("observation error=%v", err)
			}
			if tc.wrongPR {
				if len(result.Runs) != 0 || len(result.Jobs) != 0 {
					t.Fatal("different PR contributed to retry planning", result)
				}
				return
			}
			if len(result.Runs) != 1 {
				t.Fatalf("runs=%+v", result.Runs)
			}
			run := result.Runs[0]
			known := !tc.unverified && tc.wantError == ""
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
			if _, ok := tc.outputs["IMAGE_URL"]; ok {
				wantImage = ""
			}
			if _, ok := tc.outputs["IMAGE_DIGEST"]; ok {
				wantDigest = ""
			}
			if run.ImageURL != wantImage || run.ImageDigest != wantDigest {
				t.Fatalf("outputs=%q %q, want %q %q", run.ImageURL, run.ImageDigest, wantImage, wantDigest)
			}
			if known && !slices.Contains(result.Jobs, "main-on-push") {
				t.Fatal("image output changed retry eligibility", result)
			}
			if tc.wantError != "" && (run.Status != "unknown" || run.Eligible || len(result.Jobs) != 0) {
				t.Fatalf("unknown identity authorized a retry: %+v", result)
			}
			if tc.unverified && requests != 0 {
				t.Fatal("non-strict mode read run outputs")
			}
			if tc.archive != "" && requests != 2 {
				t.Fatalf("made %d Konflux requests, want one live and one archive request", requests)
			}
		})
	}
}
