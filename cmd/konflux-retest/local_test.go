package main

import "testing"

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
