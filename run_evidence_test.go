package konfluxretest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestStrictIdentityPreservesMatchingRunsAndPartialResults(t *testing.T) {
	for name, tc := range map[string]struct {
		newStatus, newOutcome string
		unknown               bool
		want                  []string
	}{
		"later branch does not hide tag": {"completed", "success", false, []string{"fact-on-push"}},
		"unknown later run withholds":    {"completed", "failure", true, nil},
	} {
		t.Run(name, func(t *testing.T) {
			checks := []checkRun{{ID: 10, Name: checkPrefix + "fact-on-push", Status: "completed", Conclusion: "failure"}, {ID: 11, Name: checkPrefix + "fact-on-push", Status: tc.newStatus, Conclusion: tc.newOutcome}}
			c := githubClient{baseURL: "https://api.github.com", verifyIdentity: true, http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Get("filter") != "all" {
					t.Fatal("strict reads must include all check runs")
				}
				data, _ := json.Marshal(map[string]any{"check_runs": checks})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
			})}, identity: func(_ context.Context, c checkRun, _, _, _ string, _ int) (runObservation, error) {
				if c.ID == 11 {
					if tc.unknown {
						return runObservation{}, errors.New("archive unavailable")
					}
					return runObservation{}, nil
				}
				return runObservation{matches: true}, nil
			}}
			result, err := checkCommit(t.Context(), c, inputChecker{}, "stackrox/fact", "stackrox/fact", Result{SHA: "head", Tag: "v0.3.77"})
			if !slices.Equal(result.Jobs, tc.want) || (err != nil) != tc.unknown {
				t.Fatal(result, err)
			}
			if !tc.unknown && (len(result.Runs) != 1 || result.Runs[0].ID != 10 || !result.Runs[0].IdentityKnown || !result.Runs[0].Eligible) {
				t.Fatal(result.Runs)
			}
		})
	}
}
func TestOptionalConformaAdvisoryWithoutIdentity(t *testing.T) {
	c := githubClient{baseURL: "https://api.github.com", verifyIdentity: true, http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"check_runs":[{"id":27,"name":"Red Hat Konflux / acs-conforma-dev / fact","status":"completed","conclusion":"failure","details_url":"https://example.com/finding"}]}`))}, nil
	})}}
	result, err := checkCommit(t.Context(), c, inputChecker{}, "stackrox/fact", "stackrox/fact", Result{SHA: "head"})
	if err == nil || len(result.Advisories) != 1 || result.Advisories[0].ID != 27 || len(result.Jobs) != 0 {
		t.Fatal(result, err)
	}
}
