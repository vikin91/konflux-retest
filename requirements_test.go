package konfluxretest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
)

func requirementFiles(t *testing.T) map[string][]byte {
	t.Helper()
	files := map[string][]byte{
		"SCANNER_VERSION":   []byte("2.39.27\n"),
		"COLLECTOR_VERSION": []byte("3.24.17\n"),
		"FACT_VERSION":      []byte("0.2.17\n"),
		"Makefile":          []byte("scanner-tag:\n\t@echo \"$$(cat SCANNER_VERSION)\"\n\ncollector-tag:\n\t@echo \"$$(cat COLLECTOR_VERSION)\"\n\nfact-tag:\n\t@echo \"$$(cat FACT_VERSION)\"\n\n"),
	}
	for _, name := range []string{"retag-pipeline", "retag-scanner", "retag-collector", "retag-fact"} {
		data, err := os.ReadFile("testdata/" + name + ".yaml")
		if err != nil {
			t.Fatal(err)
		}
		files[".tekton/"+name+".yaml"] = data
	}
	for _, suffix := range []string{"-db", "-slim", "-db-slim"} {
		files[".tekton/retag-scanner"+suffix+".yaml"] = []byte(strings.ReplaceAll(string(files[".tekton/retag-scanner.yaml"]), "release-scanner", "release-scanner"+suffix))
	}
	return files
}

func TestResolveImage(t *testing.T) {
	tests := map[string]struct{ job, repository, tag string }{
		"should resolve scanner from the PR pin":    {"retag-scanner", "release-scanner", "2.39.27-fast"},
		"should resolve scanner db separately":      {"retag-scanner-db", "release-scanner-db", "2.39.27-fast"},
		"should resolve scanner slim separately":    {"retag-scanner-slim", "release-scanner-slim", "2.39.27-fast"},
		"should resolve scanner db slim separately": {"retag-scanner-db-slim", "release-scanner-db-slim", "2.39.27-fast"},
		"should resolve collector from the PR pin":  {"retag-collector", "release-collector", "3.24.17-fast"},
		"should resolve fact from the PR pin":       {"retag-fact", "release-fact", "0.2.17-fast"},
	}
	for bundle, digest := range map[string]string{
		"cc0e455b": "cc0e455bf2f74a2f5b3dac0a08a6db9dd1586848b6d949c350164aaf6821faa3",
		"ad73f114": "ad73f11475690a5d16d31ab4da48bb514da97954bdc2681439d76193a927e41c",
		"48cf6499": "48cf64993928085331f0bd5171bc87ec2fd488cfbf4ca2b66627ea6040e011ef",
	} {
		t.Run(bundle, func(t *testing.T) {
			for name, tt := range tests {
				t.Run(name, func(t *testing.T) {
					files := requirementFiles(t)
					pipeline := string(files[".tekton/retag-pipeline.yaml"])
					files[".tekton/retag-pipeline.yaml"] = []byte(strings.ReplaceAll(pipeline,
						"cc0e455bf2f74a2f5b3dac0a08a6db9dd1586848b6d949c350164aaf6821faa3", digest))
					got, err := resolveImage(func(path string) ([]byte, error) { return files[path], nil }, tt.job, "https://github.com/stackrox/stackrox", "head-sha")
					if err != nil {
						t.Fatal(err)
					}
					if got.image() != "quay.io/rhacs-eng/"+tt.repository+":"+tt.tag || got.revision != "head-sha" || got.sourceURL != "https://github.com/stackrox/stackrox" {
						t.Fatalf("got %+v", got)
					}
				})
			}
		})
	}
}

func TestResolveImageOverridesAndUnknownContracts(t *testing.T) {
	tests := map[string]struct{ path, old, replacement, want, tag string }{
		"should use the pipeline suffix default":          {".tekton/retag-pipeline.yaml", `default: "-fast"`, `default: "-custom"`, "", "2.39.27-custom"},
		"should use an explicit empty suffix":             {".tekton/retag-scanner.yaml", "  - name: git-url", "  - name: image-tag-suffix\n    value: ''\n  - name: git-url", "", "2.39.27"},
		"should fail closed on an unknown task version":   {".tekton/retag-pipeline.yaml", "cc0e455bf2f74a2f5b3dac0a08a6db9dd1586848b6d949c350164aaf6821faa3", strings.Repeat("0", 64), "unsupported pinned task", ""},
		"should fail closed on changed tag generation":    {"Makefile", `@echo "$$(cat SCANNER_VERSION)"`, `@echo other-tag`, "unsupported Makefile recipe", ""},
		"should fail closed on an unknown target":         {".tekton/retag-scanner.yaml", "value: scanner-tag", "value: new-target", "unsupported input image Makefile target", ""},
		"should fail closed on changed image wiring":      {".tekton/retag-pipeline.yaml", `value: "$(params.input-image-repo):$(tasks.determine-input-image-tag.results.IMAGE_TAG)"`, `value: "quay.io/other/image:tag"`, "unsupported IMAGE", ""},
		"should reject parameter expressions in versions": {"SCANNER_VERSION", "2.39.27", "$(params.version)", "unsupported input image", ""},
		"should reject an invalid registry":               {".tekton/retag-scanner.yaml", "quay.io/rhacs-eng/release-scanner", "http://localhost/private", "unsupported input image", ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			files := requirementFiles(t)
			files[tt.path] = []byte(strings.ReplaceAll(string(files[tt.path]), tt.old, tt.replacement))
			got, err := resolveImage(func(path string) ([]byte, error) { return files[path], nil }, "retag-scanner", "https://github.com/stackrox/stackrox", "head")
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("error=%v; want %q", err, tt.want)
				}
			} else if err != nil || got.tag != tt.tag {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
}

func TestCheckPRChecksRetagInputs(t *testing.T) {
	tests := map[string]struct {
		status           int
		body             string
		wantError, retry bool
		message          string
	}{
		"should wait for missing scanner inputs":    {404, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, false, false, "image is missing"},
		"should distinguish registry access errors": {403, `private`, true, false, "authentication/access denied"},
		"should distinguish registry outages":       {503, `unavailable`, true, false, "HTTP 503"},
		"should suggest usable scanner inputs":      {200, "", false, true, ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			files := requirementFiles(t)
			checks := successfulChecks()
			for i := range checks {
				if checks[i].Name == checkPrefix+"roxctl-on-push" || checks[i].Name == checkPrefix+"operator-bundle-on-push" {
					checks[i].Conclusion = "failure"
				}
			}
			for _, job := range scannerRetagJobs {
				check := buildCheck(job, "completed", "failure")
				check.ID = 2
				checks = append(checks, check)
			}
			github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected mutation: %s", r.Method)
				}
				if r.Header.Get("Authorization") != "Bearer github-secret" {
					t.Error("missing GitHub authentication")
				}
				switch {
				case strings.Contains(r.URL.Path, "/pulls/"):
					fmt.Fprint(w, `{"state":"open","head":{"sha":"pr-head","repo":{"full_name":"fork/stackrox"}}}`)
				case strings.Contains(r.URL.Path, "/check-runs"):
					json.NewEncoder(w).Encode(map[string]any{"check_runs": checks})
				case strings.HasPrefix(r.URL.Path, "/repos/fork/stackrox/contents/"):
					if r.URL.Query().Get("ref") != "pr-head" {
						t.Error("not reading pinned PR head")
					}
					data, ok := files[strings.TrimPrefix(r.URL.Path, "/repos/fork/stackrox/contents/")]
					if !ok {
						t.Errorf("unexpected file %s", r.URL.Path)
					}
					json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString(data)})
				default:
					t.Errorf("unexpected request %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer github.Close()
			registryFiles := registryFixture(t, "https://github.com/stackrox/scanner", "scanner-head")
			registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Error("GitHub credential leaked to registry")
				}
				if tt.status != 200 {
					w.WriteHeader(tt.status)
					fmt.Fprint(w, tt.body)
					return
				}
				// The three unavailable variants must not suppress the usable scanner retest.
				data, ok := registryFiles[r.URL.Path]
				if !ok {
					w.WriteHeader(404)
					fmt.Fprint(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`)
					return
				}
				fmt.Fprint(w, data)
			}))
			defer registry.Close()
			result, err := checkPR(t.Context(), githubClient{baseURL: github.URL, token: "github-secret", http: github.Client()}, inputChecker{registry: registryClient{baseURL: registry.URL, http: registry.Client()}}, "stackrox/stackrox", 23958)
			if (err != nil) != tt.wantError {
				t.Fatalf("error=%v", err)
			}
			if slices.Contains(result.Jobs, "retag-scanner") != tt.retry {
				t.Fatalf("unexpected scanner decision: %s", strings.Join(result.Warnings, "\n"))
			}
			if !slices.Contains(result.Jobs, "roxctl-on-push") || slices.Contains(result.Jobs, "operator-bundle-on-push") {
				t.Fatalf("incorrect prerequisite handling: %s", strings.Join(result.Warnings, "\n"))
			}
			if tt.message != "" && (!strings.Contains(strings.Join(result.Warnings, "\n"), tt.message) || !strings.Contains(strings.Join(result.Warnings, "\n"), "quay.io/rhacs-eng/release-scanner:2.39.27-fast")) {
				t.Fatalf("missing wait detail: %s", strings.Join(result.Warnings, "\n"))
			}
			if !tt.retry && strings.Contains(strings.Join(result.Warnings, "\n"), "suggest retag-scanner") {
				t.Fatalf("misleading diagnostics: %s", strings.Join(result.Warnings, "\n"))
			}
		})
	}
}
