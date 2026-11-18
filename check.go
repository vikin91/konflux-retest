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
		// VerifyRunIdentity requires PipelineRun evidence matching the PR or full tag ref.
		// Without readable evidence, affected jobs remain unknown rather than retryable.
		VerifyRunIdentity bool
		GitHubToken       string
		QuayUsername      string
		QuayPassword      string

		// HTTPClient supplies transport and timeout settings. Nil uses a 30-second timeout.
		// The client is copied; redirect policies are set to protect service credentials.
		HTTPClient *http.Client
		// KonfluxToken returns a credential only for an explicitly trusted cluster API hostname.
		// The credential also authenticates to that cluster's Tekton Results route.
		// Nil disables Konflux access.
		KonfluxToken func(context.Context, string) (string, error)
		// AllowKonfluxLookup optionally authorizes a lookup before KonfluxToken runs.
		// appID is the check's GitHub App ID, or zero if GitHub omitted it.
		AllowKonfluxLookup func(ctx context.Context, appID int64, apiHost, namespace string) bool
		// PullImage optionally verifies image existence. Return an empty reason on success,
		// a nonempty reason for a confirmed unusable image, or an error if inconclusive.
		// Use "image is missing" for confirmed absence. Reports summarize other diagnostics.
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
		// Runs exposes latest scoped job observations, including successful resets.
		Runs []Run
		// Advisories includes Conforma failures even outside retry prerequisites.
		Advisories []Advisory
		// StartFailures reports creation failures among the commit's fetched checks,
		// including checks whose PR or tag association cannot be verified.
		StartFailures []StartFailure
		prNumber      int
	}

	// Run is stable check evidence. Eligible is assigned only by the retry planner.
	Run struct {
		Job                     string
		ID                      int64
		Status, Conclusion, URL string
		IdentityKnown, Eligible bool
		// ImageURL and ImageDigest contain a validated image reference and SHA-256 digest from a verified run.
		ImageURL, ImageDigest string
	}

	// Advisory is a policy finding for human review, not a retry or release gate.
	Advisory struct {
		ID                    int64
		Name, Conclusion, URL string
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
		req.Header.Del("Referer")
		return nil
	}
	client := githubClient{verifyIdentity: config.VerifyRunIdentity, baseURL: "https://api.github.com", token: config.GitHubToken, http: &httpClient}
	checker := inputChecker{
		registry: registryClient{
			baseURL: "https://quay.io", username: config.QuayUsername, password: config.QuayPassword,
			http: &registryHTTP,
		},
		pull: config.PullImage,
	}
	if config.KonfluxToken != nil {
		logs := &konfluxClient{http: &httpClient, token: config.KonfluxToken, allowLookup: config.AllowKonfluxLookup}
		client.identity = logs.runIdentity
		checker.logs = logs.evidence
	}
	return client, checker
}

// checkPR analyzes the open PR's head. The alternative /retest <job> branch:<branch>
// syntax may also work for PRs, but is unverified; PR output uses /konflux-retest <job>.
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
	result.prNumber = number
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
	if len(checks) == 0 {
		result.Action = ActionWait
		result.Message = "GitHub returned no check runs for this commit."
		return result, nil
	}
	for i := range checks {
		check := &checks[i]
		if failure := pipelineRunStartFailure(*check); failure != nil {
			check.startFailure = failure
			result.StartFailures = append(result.StartFailures, *failure)
		}
	}
	// Policy findings remain visible as commit-scoped advisories when run identity is unavailable.
	advisoryChecks := map[string]checkRun{}
	for _, check := range checks {
		job := strings.TrimPrefix(check.Name, checkPrefix)
		if isKonflux(check) && strings.HasPrefix(job, "acs-conforma-") && check.ID > advisoryChecks[job].ID {
			advisoryChecks[job] = check
		}
	}
	advisoryNames := make([]string, 0, len(advisoryChecks))
	for name := range advisoryChecks {
		advisoryNames = append(advisoryNames, name)
	}
	slices.Sort(advisoryNames)
	for _, name := range advisoryNames {
		check := advisoryChecks[name]
		if check.Status == "completed" && check.Conclusion != "success" && check.Conclusion != "skipped" {
			result.Advisories = append(result.Advisories, Advisory{ID: check.ID, Name: name, Conclusion: check.Conclusion, URL: reportURL(check.DetailsURL)})
		}
	}
	var identityErrors []error
	var identityWarnings []string
	const noVerifiedBuild = "No verified build checks match the requested tag or PR."
	if c.verifyIdentity {
		scoped := make([]checkRun, 0, len(checks))
		for _, check := range checks {
			if !isKonflux(check) || ignoredKonfluxCheck(check.Name) {
				continue
			}
			if check.startFailure != nil {
				scoped = append(scoped, check)
				continue
			}
			observed, identityErr := runObservation{}, errors.New("PipelineRun identity access is unavailable")
			if c.identity != nil {
				observed, identityErr = c.identity(ctx, check, repo, result.SHA, result.Tag, result.prNumber)
			}
			if identityErr != nil {
				identityErr = fmt.Errorf("check %q (run %d, URL %q): %w", check.Name, check.ID, reportURL(check.DetailsURL), identityErr)
				identityErrors = append(identityErrors, identityErr)
				identityWarnings = append(identityWarnings, "identity unknown: "+identityErr.Error())
				check.Status = "unknown"
				check.Conclusion = ""
				scoped = append(scoped, check)
			} else if observed.matches {
				check.identityKnown = true
				check.imageURL, check.imageDigest = observed.imageURL, observed.imageDigest
				scoped = append(scoped, check)
			}
		}
		if len(scoped) == 0 {
			result.Action = ActionWait
			result.Message = noVerifiedBuild
			return result, nil
		}
		checks = scoped
	}
	plan := planRetries(repo, checks, nil)
	unavailable, warnings, inputErr := checkRetagInputs(ctx, c, checker, repo, sourceRepo, result.SHA, checks, plan.jobs)
	if len(unavailable) > 0 {
		plan = planRetries(repo, checks, unavailable)
	}
	if c.verifyIdentity && !plan.hasVerifiedBuild {
		plan.noRetryAction = "Wait: " + noVerifiedBuild
	}
	result = resultFromPlan(result, plan, append(identityWarnings, warnings...))
	latest := map[string]checkRun{}
	for _, check := range checks {
		if isKonflux(check) && check.ID > latest[check.Name].ID {
			latest[check.Name] = check
		}
	}
	names := make([]string, 0, len(latest))
	for name := range latest {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		check := latest[name]
		job := strings.TrimPrefix(name, checkPrefix)
		result.Runs = append(result.Runs, Run{Job: job, ID: check.ID, Status: check.Status, Conclusion: check.Conclusion, URL: reportURL(check.DetailsURL), IdentityKnown: check.identityKnown, ImageURL: check.imageURL, ImageDigest: check.imageDigest, Eligible: slices.Contains(result.Jobs, job)})
	}
	return result, errors.Join(summarizeIdentityErrors(identityErrors), inputErr)
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
