// Package konfluxretest plans dependency-ordered Konflux retries for GitHub PRs and tags.
// Callers supply credentials explicitly; checking never posts comments or restarts jobs.
package konfluxretest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/vikin91/konflux-retest/internal/refs"
)

type (
	// Config supplies credentials and optional integrations for checking builds.
	// Its zero value uses anonymous HTTP requests without local commands or log access.
	Config struct {
		GitHubToken  string
		QuayUsername string
		QuayPassword string

		// HTTPClient supplies transport and timeout settings. Nil uses a 30-second timeout.
		// The client is copied; redirect policies are set to protect service credentials.
		HTTPClient *http.Client
		// KonfluxToken returns a token for the given cluster API hostname.
		// Nil disables log access. Errors fall back to registry checks.
		KonfluxToken func(context.Context, string) (string, error)
		// PullImage optionally verifies image existence. Return an empty reason on success,
		// a nonempty reason for a confirmed unusable image, or an error if inconclusive.
		// Success permits retries without registry or log checks; failures use those fallbacks.
		PullImage func(ctx context.Context, image string) (reason string, err error)
	}

	// Action identifies the next step for the checked PR or tag.
	Action string

	// Result describes a retry decision, including partial results when checking fails.
	// Jobs contains only eligible retries; Warnings explains blocked jobs and uncertainty.
	Result struct {
		URL      string
		Tag      string
		SHA      string
		Action   Action
		Message  string
		Jobs     []string
		Warnings []string
	}
)

const (
	// ActionNone indicates that no build retests are needed.
	ActionNone Action = "None"
	// ActionRetest indicates that Jobs can be retried now.
	ActionRetest Action = "Retest"
	// ActionWait indicates that checks or input readiness must be resolved first.
	ActionWait Action = "Wait"
	// ActionInvestigateMissingImages indicates confirmed or log-inferred missing inputs.
	ActionInvestigateMissingImages Action = "Investigate missing images"
)

// CheckPR checks the current head of an open PR in repo (OWNER/REPO).
// It returns a usable Result even on error; unaffected jobs may still be eligible.
// A closed PR returns ActionNone. No environment variables or local logins are read.
func CheckPR(ctx context.Context, config Config, repo string, number int) (Result, error) {
	if !refs.ValidRepository(repo) || number <= 0 {
		return failedResult(Result{}, errors.New("repo must be OWNER/REPO and PR number must be positive"))
	}
	client, checker := config.clients()
	return checkPR(ctx, client, checker, repo, number)
}

// CheckTag resolves a fully qualified tag ref and checks its commit in repo (OWNER/REPO).
// Annotated tags are peeled by GitHub; a same-named branch cannot select another commit.
// It returns a usable Result even on error; unaffected jobs may still be eligible.
func CheckTag(ctx context.Context, config Config, repo, tag string) (Result, error) {
	if !refs.ValidRepository(repo) || !refs.ValidTag(tag) {
		return failedResult(Result{}, errors.New("repo must be OWNER/REPO and tag must be a valid tag name"))
	}
	client, checker := config.clients()
	return checkTag(ctx, client, checker, repo, tag)
}

func (config Config) clients() (githubClient, inputChecker) {
	httpClient := http.Client{Timeout: 30 * time.Second}
	if config.HTTPClient != nil {
		httpClient = *config.HTTPClient
	}
	registryHTTP := httpClient
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	registryHTTP.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || req.URL.Scheme != "https" {
			return errors.New("unsupported registry redirect")
		}
		// Quay blobs can redirect to signed object-storage URLs.
		req.Header.Del("Authorization")
		return nil
	}
	client := githubClient{baseURL: "https://api.github.com", token: config.GitHubToken, http: &httpClient}
	checker := inputChecker{
		registry: registryClient{
			baseURL: "https://quay.io", username: config.QuayUsername, password: config.QuayPassword,
			http: &registryHTTP,
		},
		pull: config.PullImage,
	}
	if config.KonfluxToken != nil {
		logs := &konfluxClient{http: &httpClient, token: config.KonfluxToken}
		checker.logs = logs.evidence
	}
	return client, checker
}

func checkPR(ctx context.Context, c githubClient, checker inputChecker, repo string, number int) (Result, error) {
	result := Result{URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)}
	var pr pullRequest
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), &pr); err != nil {
		return failedResult(result, err)
	}
	if pr.State != "open" {
		result.Action = ActionNone
		result.Message = "PR is not open."
		return result, nil
	}
	if pr.Head.SHA == "" {
		return failedResult(result, errors.New("GitHub returned an empty PR head SHA"))
	}
	result.SHA = pr.Head.SHA
	headRepo := pr.Head.Repo.FullName
	if headRepo == "" {
		headRepo = repo
	}
	return checkCommit(ctx, c, checker, repo, headRepo, result)
}

func checkTag(ctx context.Context, c githubClient, checker inputChecker, repo, tag string) (Result, error) {
	result := Result{URL: fmt.Sprintf("https://github.com/%s/tree/%s", repo, url.PathEscape("refs/tags/"+tag))}
	var commit struct {
		SHA string `json:"sha"`
	}
	err := c.get(ctx, fmt.Sprintf("/repos/%s/commits/%s", repo, url.PathEscape("refs/tags/"+tag)), &commit)
	if err == nil && commit.SHA == "" {
		err = errors.New("GitHub returned an empty tag commit SHA")
	}
	if err != nil {
		return failedResult(result, fmt.Errorf("resolve tag %q: %w", tag, err))
	}
	result.URL = fmt.Sprintf("https://github.com/%s/commit/%s", repo, commit.SHA)
	result.Tag = tag
	result.SHA = commit.SHA
	return checkCommit(ctx, c, checker, repo, repo, result)
}

// checkCommit shares retry planning and input checks between PR heads and tags.
func checkCommit(ctx context.Context, c githubClient, checker inputChecker, repo, sourceRepo string, result Result) (Result, error) {
	checks, err := c.checks(ctx, repo, result.SHA)
	if err != nil {
		return failedResult(result, err)
	}
	plan := planRetries(repo, checks, nil)
	unavailable, warnings, inputErr := checkRetagInputs(ctx, c, checker, repo, sourceRepo, result.SHA, checks, plan.jobs)
	plan = planRetries(repo, checks, unavailable)
	return resultFromPlan(result, plan, warnings), inputErr
}

func failedResult(result Result, err error) (Result, error) {
	result.Action = ActionWait
	result.Message = "Could not determine which jobs to retry."
	result.Warnings = []string{"error: " + err.Error()}
	return result, err
}

func resultFromPlan(result Result, plan retryPlan, warnings []string) Result {
	missingImages := slices.ContainsFunc(plan.waits, func(message string) bool {
		return strings.HasSuffix(message, ": image is missing.") ||
			strings.HasSuffix(message, ": image is missing (docker pull).") ||
			strings.HasSuffix(message, ": Konflux reported image not found.")
	})
	result.Action = ActionNone
	switch {
	case len(plan.jobs) > 0:
		result.Action = ActionRetest
	case missingImages:
		result.Action = ActionInvestigateMissingImages
	case strings.HasPrefix(plan.noRetryAction, "Wait") || len(plan.waits) > 0:
		result.Action = ActionWait
	}
	result.Jobs = plan.jobs
	switch {
	case len(plan.jobs) > 0:
	case missingImages:
		result.Message = "Check the missing input images listed below."
	case len(plan.waits) > 0:
		result.Message = "Waiting for usable input images."
	default:
		result.Message = strings.TrimPrefix(plan.noRetryAction, "Wait: ")
	}
	result.Warnings = slices.Concat(plan.messages, plan.waits, warnings)
	return result
}
