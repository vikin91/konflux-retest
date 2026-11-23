package main

import (
	"fmt"
	"io"

	konfluxretest "github.com/vikin91/konflux-retest"
)

func retryComments(result konfluxretest.Result) []string {
	comments := make([]string, 0, len(result.Jobs))
	for _, job := range result.Jobs {
		if result.Tag == "" {
			comments = append(comments, "/konflux-retest "+job)
		} else {
			comments = append(comments, "/retest "+job+" tag:"+result.Tag)
		}
	}
	return comments
}

func printResult(out io.Writer, result konfluxretest.Result, lines []string, preview bool) {
	fmt.Fprintf(out, "---\n%s\n", result.URL)
	if result.Tag != "" {
		fmt.Fprintf(out, "Tag: %s\n", result.Tag)
	}
	fmt.Fprintf(out, "\nNext action: %s\n", result.Action)
	if len(result.Jobs) > 0 {
		if preview && result.Tag != "" {
			fmt.Fprintln(out, "Post each command below as a separate GitHub comment.")
		}
		for _, line := range lines {
			if preview {
				fmt.Fprintln(out, line)
			} else {
				fmt.Fprintln(out, "Posted", line)
			}
		}
	} else {
		fmt.Fprintln(out, result.Message)
	}
	fmt.Fprintln(out, "\nWarnings:")
	if len(result.Warnings) == 0 {
		fmt.Fprintln(out, "None.")
	}
	for _, warning := range result.Warnings {
		fmt.Fprintln(out, warning)
	}
}
