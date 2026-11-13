# konflux-retest

Work out which failed StackRox Konflux jobs to restart next, and print the PR comments you can copy and post yourself.

## What problem does this solve?

Konflux is the build system behind the `Red Hat Konflux` checks on StackRox pull requests. Its jobs depend on one another: component jobs produce container images, the operator bundle uses those images, and the index and snapshot jobs use the bundle.

One failed image build can therefore leave several checks red. Restarting every red check at once can waste time: jobs that need the missing image may time out again while waiting for it. Even a check that GitHub does not mark as required can block a required check.

This tool reads the checks on each PR's latest commit and tells you what to restart **now**. It keeps successful jobs, suggests failed jobs in the first unfinished milestone, and tells you to wait when prerequisites are still running or unavailable. After the suggested jobs pass, run the tool again to get the next milestone.

It helps with restart order; it does not diagnose build logs or fix the underlying failure. A repeatable code error or a missing upstream image still needs a fix.

## Does this post anything in my name?

**No. Running this tool does not post anything to GitHub in your name.** It does not create comments, restart jobs, or change PRs. The `/konflux-retest` lines are printed text. A restart is requested only when you choose to post a line as a PR comment yourself.

The implementation uses only HTTP `GET` requests to read PR details and check runs. Its only subprocess is `gh auth token --hostname github.com`, used to retrieve an existing login when no token environment variable is set. There is no GitHub write request or comment-posting command in the tool, even if your token has write permissions. These paths are in [github.go](github.go) and [main.go](main.go); the HTTP test in [github_test.go](github_test.go) also checks that requests use `GET`.

## Build

Requires Go 1.26 or newer to build. The resulting binary has no Go runtime dependencies. GitHub CLI is needed only when using its saved login instead of an environment token.

```sh
go build -o konflux-retest .
```

## Usage examples

Check one PR:

```sh
./konflux-retest --repo stackrox --prs 23956
```

Check several PRs in the order you provide:

```sh
./konflux-retest --repo stackrox --prs 23956 23957 23958
```

Use the full repository name and a comma-separated PR list:

```sh
./konflux-retest --repo stackrox/stackrox --prs 23956,23957,23958
```

Save the links and suggested comments to a file. Authentication information and diagnostics remain visible in the terminal:

```sh
./konflux-retest --repo stackrox --prs 23956 23957 23958 > retest-comments.txt
```

Show help, including authentication and the embedded milestones:

```sh
./konflux-retest --help
```

`--repo stackrox` means `stackrox/stackrox`. Other short names use the `stackrox` owner; an explicit `OWNER/REPO` is also accepted. This does not change the embedded StackRox dependency graph. Put `--repo` before `--prs`. Space-separated and comma-separated PR numbers are accepted. Duplicate numbers are processed once.

Each PR link is immediately followed by its copyable retry comments, or a wait/status message when no comments can be suggested. Diagnostics follow that block on stderr. Example stdout:

```text
https://github.com/stackrox/stackrox/pull/2345

/konflux-retest roxctl-on-push

https://github.com/stackrox/stackrox/pull/3456

/konflux-retest operator-index-on-push
/konflux-retest create-custom-snapshot
```

Post each `/konflux-retest ...` line as a separate PR comment. Verify that Konflux creates a new PipelineRun. Run the tool again after the suggested jobs pass to obtain the next milestone. A PR with no eligible retry prints a wait/status message directly below its link. For example:

```text
https://github.com/stackrox/stackrox/pull/23956

Wait: milestone 1 is not ready: roxctl-on-push (in_progress). Run this tool again when the prerequisites pass.
```

## GitHub token

The first available authentication source wins:

1. `GH_TOKEN` environment variable.
2. `GITHUB_TOKEN` environment variable.
3. The saved GitHub CLI login, read by running `gh auth token --hostname github.com` internally.

For the CLI login method, run:

```sh
gh auth login --hostname github.com
```

The tool reports the selected source on stderr. It never prints or saves the token. An invalid environment token is reported as an API error; it does not silently switch accounts by falling back to the CLI login.

A fine-grained token needs access to the requested repositories, **Pull requests: read**, and **Checks: read**. Only github.com is supported. No Konflux CLI, Konflux login, or Kubernetes credentials are needed.

References: [GitHub CLI token lookup](https://cli.github.com/manual/gh_auth_token), [GitHub CLI environment variables](https://cli.github.com/manual/gh_help_environment), [GitHub checks API](https://docs.github.com/en/rest/checks/runs#list-check-runs-for-a-git-reference).

## Embedded job catalog

| Milestone | Jobs |
| --- | --- |
| 1: component images | `central-db-on-push`, `main-on-push`, `operator-on-push`, `scanner-v4-on-push`, `scanner-v4-db-on-push`, `roxctl-on-push`, `retag-collector`, `retag-fact` |
| 1: branch-specific Scanner V2 images | `retag-scanner`, `retag-scanner-db`, `retag-scanner-slim`, `retag-scanner-db-slim` |
| 2: bundle | `operator-bundle-on-push` |
| 3: bundle consumers | `operator-index-on-push`, `create-custom-snapshot` |
| Independent | `checks` |
| Policy validation; no retry comments | `acs-conforma-prod`, `acs-conforma-prod-like`, optionally suffixed with a release such as `-5-0`, for components `central-db`, `main`, `operator`, `scanner-v4`, `scanner-v4-db`, `operator-bundle`, `roxctl`, `collector`, and `fact`, also optionally release-suffixed |

The four Scanner V2 retag jobs are independent milestone-1 producers on release branches that use them. Each waits for its own upstream Scanner image and retags it for the StackRox build. The operator bundle consumes all four outputs. If any of these checks is observed on the PR head, the planner requires the whole group to pass before suggesting milestone 2 or 3. If all four are absent, the group is not applicable; it does not block branches such as release-5.0.

Verified against the PR-head pipeline definitions for [release-4.10](https://github.com/stackrox/stackrox/blob/a9ca9e3c2241a3c85a0bf6b17db135ebbb7c0a02/.tekton/operator-bundle-pipeline.yaml#L382) and [release-4.11](https://github.com/stackrox/stackrox/blob/b119edb5790e7879feafcd8e0199b12e625fa18c/.tekton/operator-bundle-pipeline.yaml#L382), and the [shared retag pipeline](https://github.com/stackrox/stackrox/blob/a9ca9e3c2241a3c85a0bf6b17db135ebbb7c0a02/.tekton/retag-pipeline.yaml#L160). A missing upstream Scanner release image needs attention in its producer; retrying the retag alone cannot build that source image.

`ignoredKonfluxChecks` in `planner.go` is an arbitrary, tool-specific ignore list. It currently ignores `acs-conforma-dev` checks, including release-suffixed scenarios such as `acs-conforma-dev-5-0`, regardless of status or component. These checks produce no diagnostics or retry commands and do not block milestones. This is a deliberate noise filter, not a Konflux dependency rule.

The catalog and policy-name matching are in `planner.go`. Policy checks use the full shape `Red Hat Konflux / SCENARIO / COMPONENT`. New scenarios or components warn rather than being silently accepted.

## Selection rules

1. Query the PR head SHA and all pages of its check runs. For repeated check names, use the newest check-run ID, so a successful rerun supersedes an older failure.
2. Suggest completed checks with conclusions `failure`, `timed_out`, `cancelled`, `stale`, or `startup_failure` in the earliest unfinished milestone. Include failed independent `checks` at the same time. Jobs within a milestone can run in parallel.
3. Require every applicable predecessor in earlier milestones to have completed successfully before suggesting downstream jobs. Pending, missing, skipped, neutral, or action-required checks are not successful image producers and receive no retry command. Existing failures in their own milestone can still be retried.
4. Never retry policy checks automatically. Their failures may need CVE remediation or an upstream build fix.
5. Warn on every unknown Konflux check, even if successful. Recognize Konflux by the check-name prefix or its GitHub App name/slug. Unknown unfinished checks block milestones 2 and 3; they never become copyable commands.
6. Skip closed or merged PRs. Continue processing other PRs after an API error, but exit with status 1. Invalid command-line arguments exit with status 2. Warnings and blocked milestones alone do not make the command fail.

These are status-based suggestions, not log analysis. The tool cannot distinguish a transient build failure from a reproducible code or configuration error. It does not inspect registry artifacts or Konflux logs. Repeated failures need investigation.

Branches with intentionally absent producers may need a catalog adjustment: the tool withholds downstream suggestions rather than assuming that a missing image dependency is satisfied. Each invocation is a snapshot of the checks for the head commit retrieved at the start of that PR's query.

## Validation

```sh
go test -cover ./...
go vet ./...
```
