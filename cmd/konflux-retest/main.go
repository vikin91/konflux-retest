// Command konflux-retest prepares dependency-ordered Konflux retry comments for GitHub PRs and tags.
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
	"slices"
	"strconv"
	"strings"
	"time"

	konfluxretest "github.com/vikin91/konflux-retest"
	"github.com/vikin91/konflux-retest/cmd/konflux-retest/internal/commentwriter"
	"github.com/vikin91/konflux-retest/internal/refs"
)

type options struct {
	repo        string
	prs         []int
	tags        []string
	verifyPull  bool
	post        bool
	showVersion bool
}

const assessmentTimeout = 2 * time.Minute

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	login := konfluxLogin{transport: http.DefaultTransport}
	config := konfluxretest.Config{
		HTTPClient:   &http.Client{Transport: &login, Timeout: 30 * time.Second},
		KonfluxToken: login.token,
	}
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, config)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, out, diagnostics io.Writer, config konfluxretest.Config) int {
	opts, err := parseOptions(args, diagnostics)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(diagnostics, "error: %v\n", err)
		return 2
	}
	if opts.showVersion {
		fmt.Fprintln(out, version)
		return 0
	}
	token, _, err := resolveToken(ctx, os.Getenv, savedToken)
	if err != nil {
		fmt.Fprintf(diagnostics, "error: %v\n", err)
		return 1
	}
	config.VerifyRunIdentity = true
	config.VerifyMissingPRTriggers = true
	config.GitHubToken = token
	config.QuayUsername = os.Getenv("QUAY_USERNAME")
	config.QuayPassword = os.Getenv("QUAY_PASSWORD")
	if opts.verifyPull {
		config.PullImage = dockerPull
	}
	var writer *commentwriter.Client
	if opts.post {
		writer = commentwriter.New(config.HTTPClient, token)
	}
	code := 0
	report := func(result konfluxretest.Result, checkErr error, pr int) {
		if checkErr != nil {
			code = 1
		}
		output := commentReport{retries: retryComments(result), starts: prComments(result.MissingPRJobs)}
		if writer == nil || len(output.retries)+len(output.starts) == 0 {
			printResult(out, result, output)
			return
		}

		comments := slices.Concat(output.retries, output.starts)
		var links []string
		var err error
		if result.Tag == "" {
			comments = prPostingComments(result)
			links, err = writer.PostPR(ctx, opts.repo, pr, result.SHA, comments)
		} else {
			links, err = writer.PostTag(ctx, opts.repo, result.Tag, result.SHA, comments)
		}
		printResult(out, result, commentReport{links: links, posted: true})
		if err != nil {
			fmt.Fprintf(diagnostics, "error: %s: %d of %d comment links confirmed: %v\n", result.URL, len(links), len(comments), err)
			code = 1
		}
	}
	for i, pr := range opts.prs {
		if i > 0 {
			fmt.Fprintln(out)
		}
		checkCtx, cancel := context.WithTimeout(ctx, assessmentTimeout)
		result, err := konfluxretest.CheckPR(checkCtx, config, opts.repo, pr)
		cancel()
		report(result, err, pr)
		if ctx.Err() != nil {
			break
		}
	}
	for i, tag := range opts.tags {
		if i > 0 {
			fmt.Fprintln(out)
		}
		checkCtx, cancel := context.WithTimeout(ctx, assessmentTimeout)
		result, err := konfluxretest.CheckTag(checkCtx, config, opts.repo, tag)
		cancel()
		report(result, err, 0)
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
	post := fs.Bool("post", false, "post suggested comments to GitHub")
	firstPR := fs.String("prs", "", "PR numbers, separated by spaces or commas; put this flag last")
	firstTag := fs.String("tags", "", "tag names, separated by spaces or commas; put this flag last")
	var showVersion bool
	fs.BoolVar(&showVersion, "version", false, "print the version")
	fs.BoolVar(&showVersion, "V", false, "alias for --version")
	fs.Usage = func() {
		fmt.Fprint(diagnostics, `Usage: konflux-retest [flags] (--prs PR... | --tags TAG...)
       konflux-retest (-V | --version)

Flags:
  -h, --help
        show this help
`)
		fs.SetOutput(diagnostics)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)
		fmt.Fprint(diagnostics, `
Description:

Prints ---, the PR or tagged commit link, Next action, and Warnings on stdout.
Argument, setup, and posting errors go to stderr.
Shows eligible failures and verified missing PR builds in the earliest unfinished
milestone. Re-run this command after those builds pass to get the next milestone.
By default, commands are printed for manual posting. Use --post before
--prs or --tags to post them and print the comment links. PR commands normally
share one comment. Recovered GitHub check update failures and tag commands
each get a separate comment.
The configured GitHub login must be able to comment.
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
Only github.com is supported. A matching saved oc login lets the tool verify
which PR or tag triggered each run. A temporary admission denial can also
qualify for a retry when GitHub associates the check with that PR and head.
Retag decisions use the latest Konflux wait-for-image log when conclusive,
otherwise registry checks (anonymous, or QUAY_USERNAME and QUAY_PASSWORD).
Use --verify-pull before --prs or --tags to check with local docker pull first.
A successful pull confirms existence and permits retests without further input checks.
Failed pulls fall back to registry and log checks. Pulling can download image layers.
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
Collector (stackrox/collector) uses one milestone: collector-on-push.
FACT (stackrox/fact) uses one milestone: fact-on-push.

Examples:
  konflux-retest --repo stackrox --prs 2345 3456 4567
  konflux-retest --repo OWNER/REPO --prs 2345,3456,4567
  konflux-retest --repo stackrox/scanner --tags 2.39.27
`)
	}
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if showVersion {
		return options{showVersion: true}, nil
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
	if !refs.ValidRepository(repository) {
		return options{}, errors.New("--repo must be a repository name or OWNER/REPO")
	}
	opts := options{repo: repository, verifyPull: *verifyPull, post: *post}
	if hasTags {
		seen := make(map[string]bool)
		for _, value := range append([]string{*firstTag}, fs.Args()...) {
			for _, tag := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
				if !refs.ValidTag(tag) {
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
