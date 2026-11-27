package main

import (
	"fmt"
	"io"

	konfluxretest "github.com/vikin91/konflux-retest"
)

type commentReport struct {
	retries, starts, links []string
	posted                 bool
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
	if len(result.Jobs) == 0 && !startOnly {
		fmt.Fprintln(out, result.Message)
	}
	if comments.posted {
		if len(comments.links) == 0 {
			fmt.Fprintln(out, "No comments confirmed.")
		}
		for _, link := range comments.links {
			fmt.Fprintln(out, "Posted", link)
		}
	} else {
		if len(comments.retries) > 0 && result.Tag != "" {
			fmt.Fprintln(out, "Post each command below as a separate GitHub comment.")
		} else if len(result.StandalonePRJobs) > 0 {
			fmt.Fprintln(out, "Post each retest command as a separate GitHub comment.")
		}
		for _, command := range comments.retries {
			fmt.Fprintln(out, command)
		}
		if len(comments.starts) > 0 && !startOnly {
			fmt.Fprintln(out, "\nStart missing builds:")
		}
		for _, command := range comments.starts {
			fmt.Fprintln(out, command)
		}
	}
	fmt.Fprintln(out, "\nWarnings:")
	if len(result.Warnings) == 0 {
		fmt.Fprintln(out, "None.")
	}
	for _, warning := range result.Warnings {
		fmt.Fprintln(out, warning)
	}
}
