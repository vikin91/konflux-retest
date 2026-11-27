package konfluxretest

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	scopeTimeout        = 30 * time.Second
	runLookupTimeout    = 10 * time.Second
	parallelScopeChecks = 4
)

type scopedJob struct {
	run    checkRun
	issues []CheckDiagnostic
}

// scopeChecks reads unrelated job histories in parallel within the identity budget.
func (c githubClient) scopeChecks(ctx context.Context, checks []checkRun, repo string, result *Result) ([]checkRun, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, scopeTimeout)
	defer cancel()
	var scoped []checkRun
	groups := map[string][]checkRun{}
	for _, check := range checks {
		if !isKonflux(check) || ignoredKonfluxCheck(check.Name) {
			continue
		}
		if name, prefixed := strings.CutPrefix(check.Name, checkPrefix); prefixed && policyCheck.MatchString(name) {
			scoped = append(scoped, check)
			continue
		}
		groups[check.Name] = append(groups[check.Name], check)
	}
	names := slices.Sorted(maps.Keys(groups))
	jobs := make(chan int, len(names))
	for i := range names {
		jobs <- i
	}
	close(jobs)

	results := make([]scopedJob, len(names))
	var workers sync.WaitGroup
	for range min(parallelScopeChecks, len(names)) {
		workers.Go(func() {
			for i := range jobs {
				results[i] = c.scopeJob(ctx, groups[names[i]], repo, *result)
			}
		})
	}
	workers.Wait()

	var warnings []string
	var lookupErrors []error
	for _, found := range results {
		if found.run.Name != "" {
			scoped = append(scoped, found.run)
		}
		for _, issue := range found.issues {
			warnings = append(warnings, "identity unknown: "+issue.Err.Error())
			if issue.Severity == DiagnosticError {
				lookupErrors = append(lookupErrors, issue.Err)
			}
			result.Diagnostics = append(result.Diagnostics, issue)
		}
	}
	return scoped, warnings, summarizeIdentityErrors(lookupErrors)
}

// scopeJob examines newer attempts first so superseded runs cannot cause lookup errors.
func (c githubClient) scopeJob(ctx context.Context, checks []checkRun, repo string, result Result) scopedJob {
	slices.SortStableFunc(checks, func(a, b checkRun) int { return cmp.Compare(b.ID, a.ID) })
	var scoped scopedJob
	for _, check := range checks {
		if check.startFailure != nil {
			if scoped.run.startFailure == nil {
				if check.isPRStartFailure() {
					return scopedJob{run: check}
				}
				scoped.run = check
			}
			continue
		}
		observed, err := runObservation{}, ctx.Err()
		if err == nil {
			if c.identity == nil {
				err = classify(ErrCheckCredential, errors.New("PipelineRun identity access is unavailable"))
			} else {
				lookupCtx, cancel := context.WithTimeout(ctx, runLookupTimeout)
				observed, err = c.identity(lookupCtx, check, repo, result.SHA, result.Tag, result.prNumber)
				cancel()
			}
		}
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrCheckNetwork) {
				err = classify(ErrCheckNetwork, err)
			}
			severity := DiagnosticError
			if _, unknownOrigin := errors.AsType[runOriginError](err); unknownOrigin {
				severity = DiagnosticWarning
			}
			scoped.issues = append(scoped.issues, CheckDiagnostic{
				Job: strings.TrimPrefix(check.Name, checkPrefix), CheckRunID: check.ID, Severity: severity,
				Err: fmt.Errorf("check %q (run %d, URL %q): %w", check.Name, check.ID, reportURL(check.DetailsURL), err),
			})
			if scoped.run.startFailure == nil {
				check.Status, check.Conclusion = "unknown", ""
				scoped.run = check
				return scoped
			}
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if observed.matches {
			check.identityKnown = true
			check.imageURL, check.imageDigest = observed.imageURL, observed.imageDigest
			check.startFailure = scoped.run.startFailure
			scoped.run = check
			return scoped
		}
	}
	return scoped
}
