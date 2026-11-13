package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

type (
	githubClient struct {
		baseURL string
		token   string
		http    *http.Client
	}
	pullRequest struct {
		State string `json:"state"`
		Head  struct {
			SHA  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	}
	checkRun struct {
		ID         int64     `json:"id"`
		DetailsURL string    `json:"details_url"`
		StartedAt  time.Time `json:"started_at"`
		Name       string    `json:"name"`
		Status     string    `json:"status"`
		Conclusion string    `json:"conclusion"`
		App        struct {
			Slug string `json:"slug"`
			Name string `json:"name"`
		} `json:"app"`
	}
)

// file reads the PR's pinned source, including fork-specific pipeline parameters.
func (c githubClient) file(ctx context.Context, repo, sha, path string) ([]byte, error) {
	var response struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/contents/%s?ref=%s", repo, path, url.QueryEscape(sha)), &response); err != nil {
		return nil, fmt.Errorf("read %s at PR head: %w", path, err)
	}
	if response.Encoding != "base64" {
		return nil, fmt.Errorf("unsupported GitHub content encoding for %s", path)
	}
	data, err := base64.StdEncoding.DecodeString(response.Content)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return data, nil
}

func (c githubClient) get(ctx context.Context, path string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("create GitHub request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "konflux-retest")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("read GitHub API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Do not echo arbitrary response bodies, which can contain credentials or terminal controls.
		return fmt.Errorf("GitHub API returned HTTP %d (%s); check token access and API rate limits", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return fmt.Errorf("decode GitHub response: %w", err)
	}
	return nil
}

func (c githubClient) checks(ctx context.Context, repo, sha string) ([]checkRun, error) {
	var checks []checkRun
	for page := 1; ; page++ {
		var response struct {
			CheckRuns []checkRun `json:"check_runs"`
		}
		path := fmt.Sprintf("/repos/%s/commits/%s/check-runs?filter=latest&per_page=100&page=%d", repo, url.PathEscape(sha), page)
		if err := c.get(ctx, path, &response); err != nil {
			return nil, err
		}
		checks = append(checks, response.CheckRuns...)
		if len(response.CheckRuns) < 100 {
			return checks, nil
		}
	}
}

func reportPR(ctx context.Context, c githubClient, checker inputChecker, repo string, number int, out io.Writer) (err error) {
	fmt.Fprintf(out, "---\nhttps://github.com/%s/pull/%d\n\n", repo, number)
	reported := false
	defer func() {
		if err != nil && !reported {
			fmt.Fprintf(out, "Next action: Wait\nCould not determine which jobs to retry.\n\nWarnings:\nerror: %v\n", err)
		}
	}()
	var pr pullRequest
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), &pr); err != nil {
		return err
	}
	if pr.State != "open" {
		fmt.Fprintln(out, "Next action: None\nPR is not open.\n\nWarnings:\nNone.")
		return nil
	}
	if pr.Head.SHA == "" {
		return fmt.Errorf("GitHub returned an empty PR head SHA")
	}
	checks, err := c.checks(ctx, repo, pr.Head.SHA)
	if err != nil {
		return err
	}
	plan := planRetries(checks, nil)
	unavailable, warnings, inputErr := checkRetagInputs(ctx, c, checker, repo, pr, checks, plan.jobs)
	plan = planRetries(checks, unavailable)
	printPlan(out, plan, warnings)
	// The complete error is already in the Warnings section.
	reported = true
	return inputErr
}

func printPlan(out io.Writer, plan retryPlan, warnings []string) {
	missingImages := slices.ContainsFunc(plan.waits, func(message string) bool {
		return strings.HasSuffix(message, ": image is missing.") ||
			strings.HasSuffix(message, ": image is missing (docker pull).") ||
			strings.HasSuffix(message, ": Konflux reported image not found.")
	})
	action := "None"
	if len(plan.jobs) > 0 {
		action = "Retest"
	} else if missingImages {
		action = "Investigate missing images"
	} else if strings.HasPrefix(plan.noRetryAction, "Wait") || len(plan.waits) > 0 {
		action = "Wait"
	}
	fmt.Fprintf(out, "Next action: %s\n", action)
	if len(plan.jobs) > 0 {
		for _, job := range plan.jobs {
			fmt.Fprintf(out, "/konflux-retest %s\n", job)
		}
	} else if missingImages {
		fmt.Fprintln(out, "Check the missing input images listed below.")
	} else if len(plan.waits) > 0 {
		fmt.Fprintln(out, "Waiting for usable input images.")
	} else {
		fmt.Fprintln(out, strings.TrimPrefix(plan.noRetryAction, "Wait: "))
	}
	fmt.Fprintln(out, "\nWarnings:")
	messages := append(append(append([]string{}, plan.messages...), plan.waits...), warnings...)
	if len(messages) == 0 {
		fmt.Fprintln(out, "None.")
	}
	for _, message := range messages {
		fmt.Fprintln(out, message)
	}
}

func checkRetagInputs(ctx context.Context, c githubClient, checker inputChecker, repo string, pr pullRequest, checks []checkRun, jobs []string) (map[string]string, []string, error) {
	unavailable := make(map[string]string)
	var warnings []string
	var lookupErrors []error
	headRepo := pr.Head.Repo.FullName
	if headRepo == "" {
		headRepo = repo
	}
	files := make(map[string][]byte)
	readFile := func(path string) ([]byte, error) {
		if data, exists := files[path]; exists {
			return data, nil
		}
		data, err := c.file(ctx, headRepo, pr.Head.SHA, path)
		if err == nil {
			files[path] = data
		}
		return data, err
	}
	for _, job := range jobs {
		if !strings.HasPrefix(job, "retag-") {
			continue
		}
		requirement, err := resolveImage(readFile, job, "https://github.com/"+repo, pr.Head.SHA)
		if err != nil {
			unavailable[job] = "cannot resolve input requirements: " + err.Error()
			lookupErrors = append(lookupErrors, err)
			continue
		}
		var latest checkRun
		for _, check := range checks {
			if isKonflux(check) && check.Name == checkPrefix+job && check.ID > latest.ID {
				latest = check
			}
		}
		reason, notes, err := checker.check(ctx, latest, requirement)
		warnings = append(warnings, notes...)
		if reason != "" {
			unavailable[job] = requirement.image() + ": " + reason
		}
		if err != nil {
			lookupErrors = append(lookupErrors, err)
		}
	}
	return unavailable, uniqueMessages(warnings), errors.Join(lookupErrors...)
}

func uniqueMessages(messages []string) []string {
	var unique []string
	seen := make(map[string]bool)
	for _, message := range messages {
		if !seen[message] {
			unique = append(unique, message)
			seen[message] = true
		}
	}
	return unique
}
