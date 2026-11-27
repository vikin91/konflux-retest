package konfluxretest

import (
	"slices"
	"strings"
)

// PRPostingComments returns the PR comment bodies in posting order, or nil if there are no commands.
// It batches regular retries followed by missing builds, then returns one comment per standalone retry.
// Each group preserves its input order from [Result.Jobs] or [Result.MissingPRJobs].
func PRPostingComments(result Result) []string {
	const prefix = "/konflux-retest "
	var batch, standalone []string
	for _, job := range result.Jobs {
		command := prefix + job
		if slices.Contains(result.StandalonePRJobs, job) {
			standalone = append(standalone, command)
		} else {
			batch = append(batch, command)
		}
	}
	for _, job := range result.MissingPRJobs {
		batch = append(batch, prefix+job)
	}
	if len(batch) > 0 {
		return append([]string{strings.Join(batch, "\n")}, standalone...)
	}
	return standalone
}
