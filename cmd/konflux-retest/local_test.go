package main

import (
	"net/http"
	"testing"
)

func TestDockerPullFailureDistinguishesMissingFromUnknown(t *testing.T) {
	tests := map[string]struct {
		output  string
		missing bool
	}{
		"should recognize a missing manifest":                  {"Error response from daemon: manifest unknown", true},
		"should not treat denied access as missing":            {"pull access denied, repository does not exist or may require docker login", false},
		"should prioritize authentication over not-found text": {"unauthorized: manifest unknown", false},
		"should not infer absence from a daemon error":         {"Cannot connect to the Docker daemon", false},
		"should not infer absence from network failure":        {"TLS handshake timeout", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			reason, err := dockerPullFailure([]byte(tt.output))
			if (reason != "") != tt.missing || (err == nil) != tt.missing {
				t.Fatalf("reason=%q error=%v", reason, err)
			}
		})
	}
}

func TestKonfluxLoginUsesSavedPortOnlyForMatchingPipelineRuns(t *testing.T) {
	const host = "api.example.com"
	for name, test := range map[string]struct{ address, want string }{
		"should use the OpenShift port":       {"https://" + host + "/apis/tekton.dev/v1/pipelineruns", host + ":6443"},
		"should preserve an explicit port":    {"https://" + host + ":8443/apis/tekton.dev/v1/pipelineruns", host + ":8443"},
		"should preserve unrelated API paths": {"https://" + host + "/api/v1/namespaces", host},
		"should preserve other hosts":         {"https://other.example.com/apis/tekton.dev/v1/pipelineruns", "other.example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			login := konfluxLogin{transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host != test.want {
					t.Errorf("request address = %q, want %q", req.URL.Host, test.want)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
			})}
			login.connections.Store(host, konfluxConnection{address: host + ":6443"})
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, test.address, nil)
			if err != nil {
				t.Fatal(err)
			}
			res, err := login.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if req.URL.String() != test.address {
				t.Fatalf("transport changed original request to %s", req.URL)
			}
		})
	}
}
