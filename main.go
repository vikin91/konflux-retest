// Command konflux-retest prints dependency-ordered Konflux retry comments for GitHub PRs.
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
	repo string
	prs  []int
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
	token, source, err := resolveToken(ctx, os.Getenv, savedToken)
	if err != nil {
		fmt.Fprintf(diagnostics, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(diagnostics, "GitHub authentication: %s (token value is never printed).\n", source)
	client := githubClient{
		baseURL: "https://api.github.com", token: token,
		http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
	code := 0
	for i, pr := range opts.prs {
		if i > 0 {
			fmt.Fprintln(out)
		}
		if err := reportPR(ctx, client, opts.repo, pr, out, diagnostics); err != nil {
			fmt.Fprintf(diagnostics, "error: %s#%d: %v\n", opts.repo, pr, err)
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
	firstPR := fs.String("prs", "", "PR numbers, separated by spaces or commas; put this flag last")
	fs.Usage = func() {
		fmt.Fprint(diagnostics, `Usage: konflux-retest --repo stackrox --prs 2345 3456 4567
       konflux-retest --repo OWNER/REPO --prs 2345,3456,4567

Prints each PR link followed immediately by retry comments or a wait/status message.
Diagnostics go to stderr after that block.
Only failed jobs in the earliest unfinished milestone are suggested. Re-run this
command after those builds pass to get the next milestone. Nothing is posted.
Put --repo before --prs. Duplicate PR numbers are processed once.

Authentication (first available source wins):
  1. GH_TOKEN environment variable.
  2. GITHUB_TOKEN environment variable.
  3. Saved GitHub CLI login: gh auth token --hostname github.com.
     Set it up with: gh auth login --hostname github.com
The selected source is reported on stderr; the token is never printed or saved.
A fine-grained token needs repository access, Pull requests: read, and Checks: read.
Only github.com is supported. No Konflux login is needed.

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
`)
	}
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if *firstPR == "" {
		return options{}, errors.New("--prs is required; use --help for usage")
	}
	repository := *repo
	if !strings.Contains(repository, "/") {
		repository = "stackrox/" + repository
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(repository) {
		return options{}, errors.New("--repo must be a repository name or OWNER/REPO")
	}
	opts := options{repo: repository}
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
