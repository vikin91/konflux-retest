package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
			SHA string `json:"sha"`
		} `json:"head"`
	}
	checkRun struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		App        struct {
			Slug string `json:"slug"`
			Name string `json:"name"`
		} `json:"app"`
	}
)

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

func reportPR(ctx context.Context, c githubClient, repo string, number int, out, diagnostics io.Writer) (err error) {
	fmt.Fprintf(out, "https://github.com/%s/pull/%d\n\n", repo, number)
	defer func() {
		if err != nil {
			fmt.Fprintln(out, "Wait: could not determine which jobs to retry. Resolve the API error below, then run this tool again.")
			fmt.Fprintln(diagnostics)
		}
	}()
	var pr pullRequest
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), &pr); err != nil {
		return err
	}
	if pr.State != "open" {
		fmt.Fprintln(out, "No retests: PR is not open.")
		return nil
	}
	if pr.Head.SHA == "" {
		return fmt.Errorf("GitHub returned an empty PR head SHA")
	}
	checks, err := c.checks(ctx, repo, pr.Head.SHA)
	if err != nil {
		return err
	}
	plan := planRetries(checks)
	for _, name := range plan.jobs {
		fmt.Fprintf(out, "/konflux-retest %s\n", name)
	}
	if len(plan.jobs) == 0 {
		fmt.Fprintln(out, plan.noRetryAction)
	}
	if len(plan.messages) > 0 {
		fmt.Fprintln(diagnostics)
		for _, message := range plan.messages {
			fmt.Fprintf(diagnostics, "%s#%d: %s\n", repo, number, message)
		}
	}
	return nil
}
