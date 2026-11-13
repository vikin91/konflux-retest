// Command konflux-retest prints dependency-ordered Konflux retry comments for GitHub PRs and tags.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type options struct {
	repo       string
	prs        []int
	tags       []string
	verifyPull bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, out, diagnostics io.Writer) int {
	opts, err := parseOptions(args, diagnostics)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(diagnostics, "error: %v\n", err)
		return 2
	}
	token, _, err := resolveToken(ctx, os.Getenv, savedToken)
	if err != nil {
		fmt.Fprintf(diagnostics, "error: %v\n", err)
		return 1
	}
	client := githubClient{
		baseURL: "https://api.github.com", token: token,
		http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
	registry := registryClient{
		baseURL:  "https://quay.io",
		username: os.Getenv("QUAY_USERNAME"), password: os.Getenv("QUAY_PASSWORD"),
		http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 || req.URL.Scheme != "https" {
				return errors.New("unsupported registry redirect")
			}
			// Quay blobs can redirect to signed object-storage URLs.
			req.Header.Del("Authorization")
			return nil
		}},
	}
	logs := &konfluxClient{http: client.http, token: konfluxTokens()}
	checker := inputChecker{registry: registry, logs: logs.evidence}
	if opts.verifyPull {
		checker.pull = dockerPull
	}
	code := 0
	for i, pr := range opts.prs {
		if i > 0 {
			fmt.Fprintln(out)
		}
		if err := reportPR(ctx, client, checker, opts.repo, pr, out); err != nil {
			code = 1
		}
		if ctx.Err() != nil {
			break
		}
	}
	for i, tag := range opts.tags {
		if i > 0 {
			fmt.Fprintln(out)
		}
		if err := reportTag(ctx, client, checker, opts.repo, tag, out); err != nil {
			code = 1
		}
		if ctx.Err() != nil {
			break
		}
	}
	return code
}

func parseOptions(args []string, diagnostics io.Writer) (options, error) {
	fs := flag.NewFlagSet("konflux-retest", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", "stackrox", "repository name under stackrox, or OWNER/REPO")
	verifyPull := fs.Bool("verify-pull", false, "optionally verify input existence with local docker pull")
	firstPR := fs.String("prs", "", "PR numbers, separated by spaces or commas; put this flag last")
	firstTag := fs.String("tags", "", "tag names, separated by spaces or commas; put this flag last")
	fs.Usage = func() {
		fmt.Fprint(diagnostics, `Usage: konflux-retest --repo stackrox --prs 2345 3456 4567
       konflux-retest --repo OWNER/REPO --prs 2345,3456,4567
       konflux-retest --repo stackrox/scanner --tags 2.39.27

Prints ---, the PR or tagged commit link, Next action, and Warnings on stdout.
Argument and setup errors go to stderr.
Only failed jobs in the earliest unfinished milestone are suggested. Re-run this
command after those builds pass to get the next milestone. Nothing is posted.
Choose either --prs or --tags, and put other flags before the list.
Duplicate PR numbers or tag names are processed once.

Authentication (first available source wins):
  1. GH_TOKEN environment variable.
  2. GITHUB_TOKEN environment variable.
  3. Saved GitHub CLI login: gh auth token --hostname github.com.
     Set it up with: gh auth login --hostname github.com
Tokens are never printed or saved.
A fine-grained token needs repository access, Pull requests: read, Checks: read,
and Contents: read for tags, pipeline definitions, and version pins.
Only github.com is supported. Konflux logs use a matching saved oc login.
Retag decisions use the latest Konflux wait-for-image log when conclusive,
otherwise registry checks (anonymous, or QUAY_USERNAME and QUAY_PASSWORD).
Use --verify-pull before --prs or --tags to double-check with local docker pull.
Pulling can download image layers and only verifies existence, not attestations.
Unavailable sources remain warnings; unresolved readiness withholds the retest.

Embedded milestones (StackRox pipelines):
  1. central-db-on-push, main-on-push, operator-on-push, scanner-v4-on-push,
     scanner-v4-db-on-push, roxctl-on-push, retag-collector, retag-fact
     Branch-specific Scanner V2 group: retag-scanner, retag-scanner-db,
     retag-scanner-slim, retag-scanner-db-slim (all required if any is observed)
  2. operator-bundle-on-push
  3. operator-index-on-push, create-custom-snapshot
Independent: checks. Conforma-dev checks are deliberately ignored.
Other Conforma policy checks are reported, never automatically retried.
Missing/skipped prerequisites block later milestones. Unknown Konflux checks warn;
unfinished unknown checks also block later milestones. Successful jobs are retained.
Scanner (stackrox/scanner) uses one milestone: scanner-on-push,
scanner-db-on-push, scanner-slim-on-push, scanner-db-slim-on-push.
`)
	}
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	var hasPRs, hasTags bool
	fs.Visit(func(f *flag.Flag) {
		hasPRs = hasPRs || f.Name == "prs"
		hasTags = hasTags || f.Name == "tags"
	})
	if hasPRs == hasTags {
		return options{}, errors.New("exactly one of --prs or --tags is required; use --help for usage")
	}
	repository := *repo
	if !strings.Contains(repository, "/") {
		repository = "stackrox/" + repository
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(repository) {
		return options{}, errors.New("--repo must be a repository name or OWNER/REPO")
	}
	opts := options{repo: repository, verifyPull: *verifyPull}
	if hasTags {
		seen := make(map[string]bool)
		for _, value := range append([]string{*firstTag}, fs.Args()...) {
			for _, tag := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
				if !validTagName(tag) {
					return options{}, fmt.Errorf("invalid tag name %q; put flags before the tag list", tag)
				}
				if !seen[tag] {
					opts.tags = append(opts.tags, tag)
					seen[tag] = true
				}
			}
		}
		if len(opts.tags) == 0 {
			return options{}, errors.New("--tags must contain at least one tag name")
		}
		return opts, nil
	}
	seen := make(map[int]bool)
	for _, value := range append([]string{*firstPR}, fs.Args()...) {
		for _, number := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
			n, err := strconv.Atoi(number)
			if err != nil || n <= 0 {
				return options{}, fmt.Errorf("invalid PR number %q; put flags before the PR list", number)
			}
			if !seen[n] {
				opts.prs = append(opts.prs, n)
				seen[n] = true
			}
		}
	}
	if len(opts.prs) == 0 {
		return options{}, errors.New("--prs must contain at least one positive PR number")
	}
	return opts, nil
}

// validTagName rejects ref expressions and misplaced flags while allowing tag namespaces.
func validTagName(tag string) bool {
	if strings.HasPrefix(tag, "-") || strings.HasSuffix(tag, ".") ||
		strings.ContainsAny(tag, "~^:?*[\\") || strings.Contains(tag, "..") ||
		strings.Contains(tag, "@{") || strings.ContainsFunc(tag, func(r rune) bool { return r <= ' ' || r == 127 }) {
		return false
	}
	for part := range strings.SplitSeq(tag, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func resolveToken(ctx context.Context, getenv func(string) string, saved func(context.Context) (string, error)) (string, string, error) {
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if token := strings.TrimSpace(getenv(key)); token != "" {
			return token, key, nil
		}
	}
	token, err := saved(ctx)
	if err != nil || strings.TrimSpace(token) == "" {
		return "", "", errors.New("no GitHub token available: set GH_TOKEN or GITHUB_TOKEN, or run gh auth login --hostname github.com")
	}
	return strings.TrimSpace(token), "saved gh login (gh auth token --hostname github.com)", nil
}

func savedToken(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Capture stdout only: neither credentials nor subprocess diagnostics reach the terminal.
	data, err := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com").Output()
	return string(data), err
}
