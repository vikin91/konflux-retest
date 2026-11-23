package main

import (
	"strings"
	"testing"

	konfluxretest "github.com/vikin91/konflux-retest"
)

func TestPrintResult(t *testing.T) {
	tests := map[string]struct {
		result konfluxretest.Result
		want   string
	}{
		"should print retries before warnings": {
			result: konfluxretest.Result{URL: "https://github.com/stackrox/stackrox/pull/1", Action: konfluxretest.ActionRetest, Jobs: []string{"roxctl-on-push"}, Warnings: []string{"policy check failed"}},
			want:   "---\nhttps://github.com/stackrox/stackrox/pull/1\n\nNext action: Retest\n/konflux-retest roxctl-on-push\n\nWarnings:\npolicy check failed\n",
		},
		"should print resolved tag and all retry comments": {
			result: konfluxretest.Result{URL: "https://github.com/stackrox/scanner/commit/head", Tag: "2.39.28", Action: konfluxretest.ActionRetest, Jobs: []string{"scanner-on-push", "scanner-slim-on-push"}},
			want:   "---\nhttps://github.com/stackrox/scanner/commit/head\nTag: 2.39.28\n\nNext action: Retest\nPost each command below as a separate GitHub comment.\n/retest scanner-on-push tag:2.39.28\n/retest scanner-slim-on-push tag:2.39.28\n\nWarnings:\nNone.\n",
		},
		"should preserve the literal namespaced tag in retry comments": {
			result: konfluxretest.Result{URL: "https://github.com/stackrox/scanner/commit/head", Tag: "release/v1", Action: konfluxretest.ActionRetest, Jobs: []string{"scanner-on-push"}},
			want:   "---\nhttps://github.com/stackrox/scanner/commit/head\nTag: release/v1\n\nNext action: Retest\nPost each command below as a separate GitHub comment.\n/retest scanner-on-push tag:release/v1\n\nWarnings:\nNone.\n",
		},
		"should preserve PR syntax for multiple retries": {
			result: konfluxretest.Result{URL: "https://github.com/stackrox/scanner/pull/1", Action: konfluxretest.ActionRetest, Jobs: []string{"scanner-on-push", "scanner-slim-on-push"}},
			want:   "---\nhttps://github.com/stackrox/scanner/pull/1\n\nNext action: Retest\n/konflux-retest scanner-on-push\n/konflux-retest scanner-slim-on-push\n\nWarnings:\nNone.\n",
		},
		"should omit comment instructions when a tag has no retries": {
			result: konfluxretest.Result{URL: "https://github.com/stackrox/scanner/commit/head", Tag: "2.39.28", Action: konfluxretest.ActionNone, Message: "No build retests needed."},
			want:   "---\nhttps://github.com/stackrox/scanner/commit/head\nTag: 2.39.28\n\nNext action: None\nNo build retests needed.\n\nWarnings:\nNone.\n",
		},
		"should print lookup failures once": {
			result: konfluxretest.Result{URL: "https://github.com/stackrox/scanner/tree/refs%2Ftags%2Fmissing", Action: konfluxretest.ActionWait, Message: "Could not determine which jobs to retry.", Warnings: []string{"error: tag not found"}},
			want:   "---\nhttps://github.com/stackrox/scanner/tree/refs%2Ftags%2Fmissing\n\nNext action: Wait\nCould not determine which jobs to retry.\n\nWarnings:\nerror: tag not found\n",
		},
		"should print closed PR status": {
			result: konfluxretest.Result{URL: "https://github.com/stackrox/stackrox/pull/1", Action: konfluxretest.ActionNone, Message: "PR is not open."},
			want:   "---\nhttps://github.com/stackrox/stackrox/pull/1\n\nNext action: None\nPR is not open.\n\nWarnings:\nNone.\n",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var out strings.Builder
			printResult(&out, tt.result, retryComments(tt.result), true)
			if out.String() != tt.want {
				t.Fatalf("got %q; want %q", out.String(), tt.want)
			}
		})
	}
}
