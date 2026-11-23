package main

import (
	"fmt"
	"io"

	konfluxretest "github.com/vikin91/konflux-retest"
)

type commentReport struct {
	retries, starts             []string
	postedRetries, postedStarts bool
}

func retryComments(result konfluxretest.Result) []string {
	if result.Tag == "" {
		return prComments(result.Jobs)
	}
	comments := make([]string, 0, len(result.Jobs))
	for _, job := range result.Jobs {
		comments = append(comments, "/retest "+job+" tag:"+result.Tag)
	}
	return comments
}

func prComments(jobs []string) []string {
	comments := make([]string, 0, len(jobs))
	for _, job := range jobs {
		comments = append(comments, "/konflux-retest "+job)
	}
	return comments
}

func printResult(out io.Writer, result konfluxretest.Result, comments commentReport) {
	fmt.Fprintf(out, "---\n%s\n", result.URL)
	if result.Tag != "" {
		fmt.Fprintf(out, "Tag: %s\n", result.Tag)
	}
	startOnly := result.Action == konfluxretest.ActionWait && len(result.MissingPRJobs) > 0
	if startOnly {
		fmt.Fprintln(out, "\nNext action: Start missing builds")
	} else {
		fmt.Fprintf(out, "\nNext action: %s\n", result.Action)
	}
	if len(result.Jobs) > 0 {
		if !comments.postedRetries && result.Tag != "" {
			fmt.Fprintln(out, "Post each command below as a separate GitHub comment.")
		}
		printCommentLines(out, comments.retries, comments.postedRetries)
	} else if !startOnly {
		fmt.Fprintln(out, result.Message)
	}
	if len(result.MissingPRJobs) > 0 {
		if !startOnly {
			fmt.Fprintln(out, "\nStart missing builds:")
		}
		printCommentLines(out, comments.starts, comments.postedStarts)
	}
	fmt.Fprintln(out, "\nWarnings:")
	if len(result.Warnings) == 0 {
		fmt.Fprintln(out, "None.")
	}
	for _, warning := range result.Warnings {
		fmt.Fprintln(out, warning)
	}
}

func printCommentLines(out io.Writer, lines []string, posted bool) {
	if posted && len(lines) == 0 {
		fmt.Fprintln(out, "No comments confirmed.")
	}
	for _, line := range lines {
		if posted {
			fmt.Fprintln(out, "Posted", line)
		} else {
			fmt.Fprintln(out, line)
		}
	}
}
