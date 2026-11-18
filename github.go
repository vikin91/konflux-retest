package konfluxretest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type (
	githubClient struct {
		verifyIdentity bool
		identity       func(context.Context, checkRun, string, string, string, int) (runObservation, error)
		baseURL        string
		token          string
		http           *http.Client
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
		identityKnown         bool
		imageURL, imageDigest string
		ID                    int64     `json:"id"`
		DetailsURL            string    `json:"details_url"`
		StartedAt             time.Time `json:"started_at"`
		Name                  string    `json:"name"`
		Status                string    `json:"status"`
		Conclusion            string    `json:"conclusion"`
		Output                struct {
			Title   string `json:"title"`
			Summary string `json:"summary"`
			Text    string `json:"text"`
		} `json:"output"`
		App struct {
			Slug string `json:"slug"`
			Name string `json:"name"`
		} `json:"app"`
		startFailure *StartFailure
	}
)

// file reads pinned source, including fork-specific pipeline parameters.
func (c githubClient) file(ctx context.Context, repo, sha, path string) ([]byte, error) {
	var response struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/contents/%s?ref=%s", repo, path, url.QueryEscape(sha)), &response); err != nil {
		return nil, fmt.Errorf("read %s at commit %s: %w", path, sha, err)
	}
	if response.Encoding != "base64" {
		return nil, fmt.Errorf("unsupported GitHub content encoding for %s", path)
	}
	data, err := base64.StdEncoding.DecodeString(response.Content)
	if err != nil {
		return nil, fmt.Errorf("could not decode %s", path)
	}
	return data, nil
}

func (c githubClient) get(ctx context.Context, path string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return errors.New("invalid GitHub request URL")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "konflux-retest")
	resp, err := c.http.Do(req)
	if err != nil {
		return diagnosticError("GitHub API request failed", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Do not echo arbitrary response bodies, which can contain credentials or terminal controls.
		return fmt.Errorf("GitHub API returned HTTP %d (%s); check token access and API rate limits", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return diagnosticError("could not decode GitHub response", err)
	}
	return nil
}

func (c githubClient) checks(ctx context.Context, repo, sha string) ([]checkRun, error) {
	var checks []checkRun
	for page := 1; ; page++ {
		var response struct {
			CheckRuns []checkRun `json:"check_runs"`
		}
		filter := "latest"
		if c.verifyIdentity {
			filter = "all"
		}
		path := fmt.Sprintf("/repos/%s/commits/%s/check-runs?filter=%s&per_page=100&page=%d", repo, url.PathEscape(sha), filter, page)
		if err := c.get(ctx, path, &response); err != nil {
			return nil, err
		}
		checks = append(checks, response.CheckRuns...)
		if len(response.CheckRuns) < 100 {
			return checks, nil
		}
	}
}

func checkRetagInputs(ctx context.Context, c githubClient, checker inputChecker, repo, sourceRepo, sha string, checks []checkRun, jobs []string) (map[string]string, []string, error) {
	unavailable := make(map[string]string)
	var warnings []string
	var lookupErrors []error
	files := make(map[string][]byte)
	readFile := func(path string) ([]byte, error) {
		if data, exists := files[path]; exists {
			return data, nil
		}
		data, err := c.file(ctx, sourceRepo, sha, path)
		if err == nil {
			files[path] = data
		}
		return data, err
	}
	for _, job := range jobs {
		if !strings.HasPrefix(job, "retag-") {
			continue
		}
		requirement, err := resolveImage(readFile, job, "https://github.com/"+repo, sha)
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
