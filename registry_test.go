package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAttestationReadiness(t *testing.T) {
	v02 := func(tasks string) string {
		return `{"predicateType":"https://slsa.dev/provenance/v0.2","predicate":{"buildConfig":{"tasks":` + tasks + `}}}`
	}
	v1 := func(task string) string {
		return `{"predicateType":"https://slsa.dev/provenance/v1","predicate":{"buildDefinition":{"buildType":"https://tekton.dev/chains/v2/slsa-tekton","resolvedDependencies":[{"name":"pipelineTask","content":"` + base64.StdEncoding.EncodeToString([]byte(task)) + `"}]}}}`
	}
	tests := map[string]struct{ statement, want string }{
		"should accept successful v0.2 tasks":          {v02(`[{"status":"Succeeded"}]`), ""},
		"should reject failed v0.2 tasks":              {v02(`[{"status":"Succeeded"},{"status":"Failed"}]`), "unsuccessful task"},
		"should reject missing task results":           {v02(`null`), "no task results"},
		"should accept successful v1 tasks":            {v1(`{"status":{"conditions":[{"type":"Succeeded","status":"True"}]}}`), ""},
		"should reject failed v1 tasks":                {v1(`{"status":{"conditions":[{"type":"Succeeded","status":"False"}]}}`), "unsuccessful task"},
		"should reject a missing v1 success condition": {v1(`{"status":{"conditions":[{"type":"Ready","status":"True"}]}}`), "unsuccessful task"},
		"should reject malformed v1 task content":      {v1(`not JSON`), "invalid task result"},
		"should reject unknown provenance":             {`{"predicateType":"other"}`, "unsupported predicate"},
		"should reject an unknown v1 build type":       {`{"predicateType":"https://slsa.dev/provenance/v1","predicate":{"buildDefinition":{"buildType":"other"}}}`, "unsupported build type"},
		"should reject malformed provenance":           {`not JSON`, "invalid statement"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(map[string][]byte{"payload": []byte(tt.statement)})
			if err != nil {
				t.Fatal(err)
			}
			got := attestationReadiness(data)
			if (tt.want == "" && got != "") || !strings.Contains(got, tt.want) {
				t.Fatalf("readiness=%q; want %q", got, tt.want)
			}
		})
	}
	if got := attestationReadiness([]byte(`{"payload":"invalid base64"}`)); !strings.Contains(got, "invalid envelope") {
		t.Fatalf("got %q", got)
	}
}

// registryFixture supplies a multi-platform image and its digest-bound attestation.
func registryFixture(t *testing.T, source, revision string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	encode := func(value any) string {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	blob := func(value any) string {
		data := encode(value)
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(data)))
		files["/v2/rhacs-eng/release-scanner/blobs/"+digest] = data
		return digest
	}
	config := blob(map[string]any{"config": map[string]any{"Labels": map[string]string{"source-location": source, "vcs-ref": revision}}})
	manifest := encode(map[string]any{"schemaVersion": 2, "config": map[string]string{"digest": config}})
	child := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(manifest)))
	files["/v2/rhacs-eng/release-scanner/manifests/"+child] = manifest
	index := encode(map[string]any{"schemaVersion": 2, "manifests": []any{map[string]any{"digest": child, "platform": map[string]string{"os": "linux", "architecture": "amd64"}}}})
	files["/v2/rhacs-eng/release-scanner/manifests/2.39.27-fast"] = index
	digest := fmt.Sprintf("sha256-%x.att", sha256.Sum256([]byte(index)))
	payload := blob(map[string][]byte{"payload": []byte(`{"predicateType":"https://slsa.dev/provenance/v0.2","predicate":{"buildConfig":{"tasks":[{"status":"Succeeded"}]}}}`)})
	files["/v2/rhacs-eng/release-scanner/manifests/"+digest] = encode(map[string]any{"schemaVersion": 2, "layers": []any{map[string]string{"digest": payload}}})
	return files
}

func TestRegistryReadiness(t *testing.T) {
	tests := map[string]struct{ source, revision, missing, want string }{
		"should accept a usable scanner image from another repository": {source: "https://github.com/stackrox/scanner", revision: "scanner-sha"},
		"should accept the matching consumer revision":                 {source: "https://github.com/stackrox/stackrox", revision: "pr-sha"},
		"should wait for the matching consumer revision":               {source: "https://github.com/stackrox/stackrox", revision: "old-sha", want: "does not match PR head"},
		"should require source labels":                                 {source: "", revision: "scanner-sha", want: "lacks required"},
		"should name a missing image":                                  {missing: "2.39.27-fast", want: "image is missing"},
		"should require the index attestation":                         {source: "https://github.com/stackrox/scanner", revision: "scanner-sha", missing: ".att", want: "attestation is missing"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			files := registryFixture(t, tt.source, tt.revision)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected mutation: %s", r.Method)
				}
				if r.Header.Get("Authorization") != "" {
					t.Error("unexpected credential")
				}
				data, exists := files[r.URL.Path]
				if !exists || (tt.missing != "" && strings.HasSuffix(r.URL.Path, tt.missing)) {
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`)
					return
				}
				fmt.Fprint(w, data)
			}))
			defer server.Close()
			client := registryClient{baseURL: server.URL, http: server.Client()}
			reason, err := client.readiness(t.Context(), imageRequirement{repository: "quay.io/rhacs-eng/release-scanner", tag: "2.39.27-fast", sourceURL: "https://github.com/stackrox/stackrox", revision: "pr-sha"})
			if err != nil {
				t.Fatal(err)
			}
			if (tt.want == "" && reason != "") || !strings.Contains(reason, tt.want) {
				t.Fatalf("reason=%q; want %q", reason, tt.want)
			}
		})
	}
}

func TestRegistryErrorsAndAnonymousAuthentication(t *testing.T) {
	tests := map[string]struct {
		status     int
		body, want string
		missing    bool
	}{
		"should distinguish missing manifests":            {404, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, "", true},
		"should not treat a proxy 404 as a missing image": {404, `not found`, "HTTP 404", false},
		"should distinguish denied access":                {403, `secret`, "authentication/access denied", false},
		"should distinguish registry failures":            {503, `secret`, "HTTP 503", false},
		"should distinguish rate limits":                  {429, `secret`, "HTTP 429", false},
		"should distinguish private repositories":         {401, `secret`, "authentication/access denied", false},
		"should accept anonymous bearer authentication":   {200, `manifest`, "", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected mutation: %s", r.Method)
				}
				if r.URL.Path == "/auth" {
					if r.Header.Get("Authorization") != "" {
						t.Error("credential sent to token endpoint")
					}
					if r.URL.Query().Get("scope") != "repository:test:pull" {
						t.Error("incorrect registry scope")
					}
					fmt.Fprint(w, `{"token":"registry-token"}`)
					return
				}
				if r.Header.Get("Authorization") == "" {
					w.Header().Set("WWW-Authenticate", `Bearer realm="`+server.URL+`/auth",service="registry",scope="repository:test:pull"`)
					w.WriteHeader(401)
					return
				}
				if r.Header.Get("Authorization") != "Bearer registry-token" {
					t.Error("wrong registry token")
				}
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			}))
			defer server.Close()
			client := registryClient{baseURL: server.URL, http: server.Client()}
			_, err := client.get(t.Context(), "/v2/test/manifests/tag")
			if errors.Is(err, errRegistryMissing) != tt.missing {
				t.Fatalf("missing classification: %v", err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("error=%v; want %q", err, tt.want)
			}
			if tt.status == 200 && err != nil {
				t.Fatal(err)
			}
			if err != nil && (strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "registry-token")) {
				t.Fatal("credential or response body leaked")
			}
		})
	}
}

func TestRegistryNetworkErrorAndDigestIntegrity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"schemaVersion":2}`) }))
	client := registryClient{baseURL: server.URL, http: server.Client()}
	_, _, err := client.manifest(t.Context(), "test", "sha256:"+strings.Repeat("0", 64))
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("got %v", err)
	}
	server.Close()
	_, err = client.get(t.Context(), "/v2/test/manifests/tag")
	if err == nil || !strings.Contains(err.Error(), "network") || errors.Is(err, errRegistryMissing) {
		t.Fatalf("got %v", err)
	}
}

func TestRegistryCredentialsStayAtTokenEndpoint(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth" {
			user, password, ok := r.BasicAuth()
			if !ok || user != "robot" || password != "registry-secret" {
				t.Error("missing registry credentials")
			}
			fmt.Fprint(w, `{"token":"scoped-token"}`)
			return
		}
		if r.Header.Get("Authorization") == "Bearer scoped-token" {
			fmt.Fprint(w, `metadata`)
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("credentials sent outside token endpoint")
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+server.URL+`/auth",service="registry",scope="repository:test:pull"`)
		w.WriteHeader(401)
	}))
	defer server.Close()
	client := registryClient{baseURL: server.URL, http: server.Client(), username: "robot", password: "registry-secret"}
	if _, err := client.get(t.Context(), "/v2/test/manifests/tag"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.registryToken(t.Context(), `Bearer realm="https://other.example/auth"`); err == nil || !strings.Contains(err.Error(), "unsupported token endpoint") {
		t.Fatalf("got %v", err)
	}
	client.password = ""
	if _, err := client.registryToken(t.Context(), `Bearer realm="`+server.URL+`/auth"`); err == nil || !strings.Contains(err.Error(), "both QUAY_USERNAME and QUAY_PASSWORD") {
		t.Fatalf("got %v", err)
	}
}
