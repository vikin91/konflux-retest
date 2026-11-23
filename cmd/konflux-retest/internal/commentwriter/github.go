// Package commentwriter posts GitHub comments on behalf of the konflux-retest CLI.
package commentwriter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client writes comments to github.com using the CLI's GitHub credential.
type Client struct {
	http  http.Client
	token string
}

// New creates a client with its own redirect policy to keep writes on GitHub.
func New(client *http.Client, token string) *Client {
	var httpClient http.Client
	if client != nil {
		httpClient = *client
	}
	if httpClient.Timeout == 0 {
		httpClient.Timeout = 30 * time.Second
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{http: httpClient, token: token}
}

// PostPR checks that the PR is still open at the assessed head before posting.
// It returns the links confirmed before the first posting error.
func (c *Client) PostPR(ctx context.Context, repo string, pr int, sha string, comments []string) ([]string, error) {
	var current struct {
		State string `json:"state"`
		Head  struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := c.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, pr), nil, http.StatusOK, &current); err != nil {
		return nil, fmt.Errorf("recheck PR head: %w", err)
	}
	if current.State != "open" || sha == "" || current.Head.SHA != sha {
		return nil, errors.New("PR closed or its head changed since assessment")
	}
	path := fmt.Sprintf("/repos/%s/issues/%d/comments", repo, pr)
	link := fmt.Sprintf("https://github.com/%s/pull/%d#issuecomment-", repo, pr)
	return c.post(ctx, path, link, comments)
}

// PostTag checks that the tag still resolves to the assessed commit before posting.
// It returns the links confirmed before the first posting error.
func (c *Client) PostTag(ctx context.Context, repo, tag, sha string, comments []string) ([]string, error) {
	if len(sha) != 40 || strings.Trim(sha, "0123456789abcdef") != "" {
		return nil, errors.New("invalid GitHub commit SHA")
	}
	var current struct {
		SHA string `json:"sha"`
	}
	path := "/repos/" + repo + "/commits/"
	if err := c.request(ctx, http.MethodGet, path+url.PathEscape("refs/tags/"+tag), nil, http.StatusOK, &current); err != nil {
		return nil, fmt.Errorf("recheck tag commit: %w", err)
	}
	if current.SHA != sha {
		return nil, errors.New("tag points to a different commit since assessment")
	}
	link := "https://github.com/" + repo + "/commit/" + sha + "#commitcomment-"
	return c.post(ctx, path+sha+"/comments", link, comments)
}

func (c *Client) post(ctx context.Context, path, linkPrefix string, comments []string) ([]string, error) {
	links := make([]string, 0, len(comments))
	for _, comment := range comments {
		body, err := json.Marshal(map[string]string{"body": comment})
		if err != nil {
			return links, fmt.Errorf("encode comment: %w", err)
		}
		var posted struct {
			ID int64 `json:"id"`
		}
		if err := c.request(ctx, http.MethodPost, path, body, http.StatusCreated, &posted); err != nil {
			return links, fmt.Errorf("%q: %w", comment, err)
		}
		if posted.ID <= 0 {
			return links, fmt.Errorf("%q: GitHub created the comment but returned no comment ID", comment)
		}
		links = append(links, fmt.Sprintf("%s%d", linkPrefix, posted.ID))
	}
	return links, nil
}

func (c *Client) request(ctx context.Context, method, path string, body []byte, wantStatus int, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid GitHub request URL")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "konflux-retest")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if method == http.MethodPost {
			return errors.New("GitHub request failed and the comment may have been posted")
		}
		return errors.New("GitHub request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		if method == http.MethodPost && (resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout) {
			return fmt.Errorf("GitHub returned HTTP %d and the comment may have been posted", resp.StatusCode)
		}
		return fmt.Errorf("GitHub returned HTTP %d (%s)", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		if method == http.MethodPost {
			return errors.New("GitHub created the comment but its response could not be read")
		}
		return errors.New("could not read GitHub response")
	}
	return nil
}
