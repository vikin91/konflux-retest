package main

import (
	"fmt"
	"io"

	konfluxretest "github.com/vikin91/konflux-retest"
)

func printResult(out io.Writer, result konfluxretest.Result) {
	fmt.Fprintf(out, "---\n%s\n", result.URL)
	if result.Tag != "" {
		fmt.Fprintf(out, "Tag: %s\n", result.Tag)
	}
	fmt.Fprintf(out, "\nNext action: %s\n", result.Action)
	if len(result.Jobs) > 0 {
		if result.Tag != "" {
			fmt.Fprintln(out, "Post each command below as a separate GitHub comment.")
		}
		for _, job := range result.Jobs {
			if result.Tag != "" {
				fmt.Fprintf(out, "/retest %s tag:%s\n", job, result.Tag)
			} else {
				fmt.Fprintf(out, "/konflux-retest %s\n", job)
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
