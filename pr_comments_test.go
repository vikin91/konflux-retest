package konfluxretest_test

import (
	"reflect"
	"testing"

	"github.com/vikin91/konflux-retest"
)

func TestPRPostingComments(t *testing.T) {
	tests := map[string]struct {
		result konfluxretest.Result
		want   []string
	}{
		"should batch regular retries before missing builds and post standalone retries last": {
			result: konfluxretest.Result{
				Jobs:             []string{"standalone-z", "retry-z", "standalone-a", "retry-a"},
				MissingPRJobs:    []string{"missing-z", "missing-a"},
				StandalonePRJobs: []string{"standalone-a", "standalone-z"},
			},
			want: []string{
				"/konflux-retest retry-z\n/konflux-retest retry-a\n/konflux-retest missing-z\n/konflux-retest missing-a",
				"/konflux-retest standalone-z",
				"/konflux-retest standalone-a",
			},
		},
		"should return separate comments for standalone-only retries": {
			result: konfluxretest.Result{
				Jobs:             []string{"first", "second"},
				StandalonePRJobs: []string{"first", "second"},
			},
			want: []string{"/konflux-retest first", "/konflux-retest second"},
		},
		"should batch missing builds with no retries": {
			result: konfluxretest.Result{MissingPRJobs: []string{"first", "second"}},
			want:   []string{"/konflux-retest first\n/konflux-retest second"},
		},
		"should return nil for an empty result": {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := konfluxretest.PRPostingComments(tt.result); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("PRPostingComments() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
