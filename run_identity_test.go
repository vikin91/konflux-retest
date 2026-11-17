package konfluxretest

import (
	"encoding/json"
	"testing"
)

func TestRunIdentity(t *testing.T) {
	for name, tc := range map[string]struct {
		event, branch, number, tag string
		pr                         int
		want                       bool
	}{
		"tag":                          {"push", "refs/tags/v1.2.3", "", "v1.2.3", 0, true},
		"branch on same commit":        {"push", "release-1.2", "", "v1.2.3", 0, false},
		"different tag on same commit": {"push", "refs/tags/v1.2.4", "", "v1.2.3", 0, false},
		"PR":                           {"pull_request", "release-1.2", "42", "", 42, true},
		"different PR":                 {"pull_request", "release-1.2", "43", "", 42, false},
	} {
		t.Run(name, func(t *testing.T) {
			const p = "pipelinesascode.tekton.dev/"
			annotations := map[string]string{p + "check-run-id": "123", p + "sha": "head", p + "repo-url": "https://github.com/stackrox/fact", p + "event-type": tc.event, p + "branch": tc.branch, p + "pull-request": tc.number}
			data, _ := json.Marshal(map[string]any{"metadata": map[string]any{"name": "run", "namespace": "builds", "annotations": annotations}})
			ok, err := matchRunIdentity(data, "builds", "run", 123, "stackrox/fact", "head", tc.tag, tc.pr)
			if err != nil || ok != tc.want {
				t.Fatal(ok, err)
			}
			if _, err := matchRunIdentity(data, "builds", "run", 124, "stackrox/fact", "head", tc.tag, tc.pr); err == nil {
				t.Fatal("accepted wrong check ID")
			}
		})
	}
}
