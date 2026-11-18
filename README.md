# konflux-retest

Work out which failed StackRox, Scanner, Collector, or FACT Konflux jobs to restart next, and print retest comments you can copy and post yourself.

## What problem does this solve?

Konflux is the build system behind the `Red Hat Konflux` checks on StackRox pull requests. Its jobs depend on one another: component jobs produce container images, the operator bundle uses those images, and the index and snapshot jobs use the bundle.

One failed image build can therefore leave several checks red. Restarting every red check at once can waste time: jobs that need the missing image may time out again while waiting for it. Even a check that GitHub does not mark as required can block a required check.

This tool reads the checks on each PR's latest commit or each tag's resolved commit and tells you what to restart **now**. It keeps successful jobs, suggests failed jobs in the first unfinished milestone, and tells you to wait when prerequisites are still running or unavailable. After the suggested jobs pass, run the tool again to get the next milestone.

It checks retag input images before suggesting those retries. It does not diagnose build logs or fix the underlying failure; a repeatable code error still needs investigation.

## Does this post anything in my name?

**No. Running this tool does not post anything to GitHub in your name.** It does not create comments, restart jobs, or change PRs. The `/konflux-retest` and `/retest` lines are printed text. A restart is requested only when you choose to post a suggested command as a GitHub comment yourself.

The implementation uses only HTTP `GET` requests to read PR details, tag commits, check runs, pipeline definitions, version pins, Quay image metadata, and archived Konflux task logs. It reads saved logins with `gh auth token --hostname github.com` and `oc config view` / `oc whoami -t`. With `--verify-pull`, it also runs `docker pull`, which may download image layers locally. There is no GitHub write request or comment-posting command in the tool, even if your token has write permissions. These paths are in [github.go](github.go), [registry.go](registry.go), and [cmd/konflux-retest](cmd/konflux-retest); the HTTP test in [github_test.go](github_test.go) also checks that requests use `GET`.

## Build

Requires Go 1.26 or newer to build. The resulting binary has no Go runtime dependencies. GitHub CLI is needed only when using its saved login instead of an environment token.

```sh
go build -o konflux-retest ./cmd/konflux-retest
```

## Use from another Go project

Import `github.com/vikin91/konflux-retest` as package `konfluxretest`. The root package exposes `CheckPR` and `CheckTag`; the CLI lives in `cmd/konflux-retest`.

After `v0.1.0` is published, pin it from your Go module with this command:

```sh
go get github.com/vikin91/konflux-retest@v0.1.0
```

```go
import (
    "context"

    "github.com/vikin91/konflux-retest"
)

func checkBuild(ctx context.Context, token string) (konfluxretest.Result, error) {
    config := konfluxretest.Config{GitHubToken: token}
    return konfluxretest.CheckPR(ctx, config, "stackrox/stackrox", 23956)
    // For a tag, use:
    // return konfluxretest.CheckTag(ctx, config, "stackrox/scanner", "2.39.27")
}
```

Both functions require a full `OWNER/REPO` name and accept a context for cancellation. `Result` contains the link, resolved commit SHA, tag when resolved, next action, retry job names, explanatory message, and warnings. An error can accompany a partial result: unaffected jobs remain eligible, while jobs whose inputs could not be verified are withheld. Inspect both the result and the error.

The library does not read environment variables, discover saved logins, execute subprocesses, print output, or exit the process. Supply `GitHubToken` and optional `QuayUsername`/`QuayPassword` through `Config`. Optional `KonfluxToken` and `PullImage` callbacks provide log authentication and image verification; leaving them nil uses registry checks only. `HTTPClient` optionally supplies a custom transport and timeout. The library copies it and applies credential-safe redirect policies. Without one, requests use a 30-second timeout.

Return a token from `KonfluxToken` only for exact cluster API hostnames you
trust. The library uses that token for the cluster API and its Tekton Results
route. For a hosted app, use `AllowKonfluxLookup` to check the GitHub App ID,
API hostname, and namespace before the library requests a token.

For local development, add a `replace github.com/vikin91/konflux-retest => /path/to/konflux-retest` directive to the consuming module.

## Module releases

The library's public API is the exported API of the root `konfluxretest` package, including the documented behavior of `CheckPR`, `CheckTag`, and their configuration and results. The library and CLI share the same module tag.

Releases use `vMAJOR.MINOR.PATCH` Git tags. During `v0`, use minor releases for API changes, including breaking changes, and patch releases for compatible fixes. Starting at `v1.0.0`, use minor releases for compatible additions and major releases for incompatible API changes. Starting at `v2`, Go requires the major version in the module and import paths, such as `github.com/vikin91/konflux-retest/v2`. See [Go's module versioning guide](https://go.dev/doc/modules/version-numbers).

See the [changelog](CHANGELOG.md) for changes in each release starting with `v0.1.0`.

Run `go mod tidy -diff`, `go test ./...`, and `go vet ./...` before tagging the release commit. For `v0.1.0`, create and publish the tag from that commit:

```sh
git tag -a v0.1.0 -m "Release v0.1.0"
git push origin refs/tags/v0.1.0
```

Keep published tags immutable. Publish fixes under a new version. See [Go's module publishing guide](https://go.dev/doc/modules/publishing).

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

Check a tagged Scanner build:

```sh
./konflux-retest --repo stackrox/scanner --tags 2.39.27
```

Multiple tags work the same way: `--tags 2.39.27,2.39.28` or `--tags 2.39.27 2.39.28`. Lightweight and annotated tags resolve through `refs/tags/`, so a branch with the same name cannot select a different commit. Each report includes the tag name and its commit link. A missing tag produces an error report; the tool continues with the remaining tags.

Save the complete PR reports, including warnings, to a file:

```sh
./konflux-retest --repo stackrox --prs 23956 23957 23958 > retest-comments.txt
```

Show help, including authentication and the embedded milestones:

```sh
./konflux-retest --help
```

`--repo stackrox` means `stackrox/stackrox`. Other short names use the `stackrox` owner; an explicit `OWNER/REPO` is also accepted. The `stackrox/scanner`, `stackrox/collector`, and `stackrox/fact` repositories use their own job catalogs; other repositories use the StackRox catalog. Choose either `--prs` or `--tags` and put other flags before the list. Both lists accept spaces or commas and process duplicate entries once.

Every PR report follows this format on stdout. `Next action` is `Retest`, `Investigate missing images`, `Wait`, or `None`. Warnings appear once under `Warnings:`; argument and startup errors use stderr. No authentication banner is printed.

```text
---
https://github.com/stackrox/stackrox/pull/2345

Next action: Retest
/konflux-retest roxctl-on-push

Warnings:
None.

---
https://github.com/stackrox/stackrox/pull/3456

Next action: Wait
main-on-push (in_progress).

Warnings:
Milestone 1 is not ready: main-on-push (in_progress).
```

PR suggestions use `/konflux-retest <job>`. Tag suggestions use `/retest <job> tag:<tag>` and must be posted on the linked commit, with **exactly one retest command per GitHub comment**. Multiple lines in one tag comment do not work reliably. For example, post these as two separate comments for tag `2.39.28`:

```text
/retest scanner-on-push tag:2.39.28
```

```text
/retest scanner-slim-on-push tag:2.39.28
```

Verify that Konflux creates a new PipelineRun. Run the tool again after those jobs finish.

To optionally check input existence with your local Docker login:

```sh
./konflux-retest --verify-pull --repo stackrox --prs 23958
```

`docker pull` can download image layers. With `--verify-pull`, a successful pull reports that the image exists and permits the retest without further input checks. Failed pulls fall back to Quay and Konflux logs with their diagnostics.

## GitHub token

The first available authentication source wins:

1. `GH_TOKEN` environment variable.
2. `GITHUB_TOKEN` environment variable.
3. The saved GitHub CLI login, read by running `gh auth token --hostname github.com` internally.

For the CLI login method, run:

```sh
gh auth login --hostname github.com
```

The tool never prints or saves the token. An invalid environment token is reported as an API error; it does not silently switch accounts by falling back to the CLI login.

A fine-grained token needs access to the requested repositories, **Pull requests: read**, **Checks: read**, and **Contents: read** (including access to the head repository for fork PRs). Only github.com is supported. Archived Konflux logs use `oc` and an existing context for the cluster linked by the check. The current context is not changed. Without a matching login, the tool falls back to registry checks.

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

For `stackrox/scanner`, the catalog has one milestone: `scanner-on-push`, `scanner-db-on-push`, `scanner-slim-on-push`, and `scanner-db-slim-on-push`. These builds are independent, as defined by the [Scanner component pipeline](https://github.com/stackrox/scanner/blob/6938eeab2b7ba5235bc4c710d56e742aab6ac52f/.tekton/scanner-component-pipeline.yaml). Failed builds can be retried together; successful or running builds receive no retry comment. StackRox's operator and retag jobs are not prerequisites in this repository. Scanner Conforma policy checks are reported without retry comments, and Conforma-dev checks are ignored.

For `stackrox/collector` (or `--repo collector`), the catalog has one milestone: `collector-on-push`. Its [PipelineRun](https://github.com/stackrox/collector/blob/8157bb9966cbee728fc5775e99323c33b0931a60/.tekton/collector-build.yaml) builds all architectures within one pipeline. Failed builds receive `/konflux-retest collector-on-push`; successful or running builds receive no retry comment. Collector does not require StackRox's operator or retag jobs. Conforma policy failures are reported without retries, and Conforma-dev checks are ignored. For example, run `./konflux-retest --repo collector --prs 4307`.

For `stackrox/fact` (or `--repo fact`), the catalog has one milestone: `fact-on-push`, matching its [PipelineRun](https://github.com/stackrox/fact/blob/7826bed24314e774deb43ae5db1e7b30b1eda796/.tekton/fact-build.yaml). Failed builds receive `/konflux-retest fact-on-push` for PRs or `/retest fact-on-push tag:<tag>` for tags; running builds wait, and successful builds receive no retry comment. FACT uses its own build without StackRox job prerequisites. Conforma policy failures are reported without retries, and Conforma-dev checks are ignored. For example, run `./konflux-retest --repo stackrox/fact --tags 0.3.12`.

The four Scanner V2 retag jobs are independent milestone-1 producers on release branches that use them. Each consumes its own upstream Scanner image and retags it for the StackRox build. The tool checks that input before suggesting a retag retry. The operator bundle consumes all four outputs. If any of these checks is observed on the PR head, the planner requires the whole group to pass before suggesting milestone 2 or 3. If all four are absent, the group is not applicable; it does not block branches such as release-5.0.

Verified against the PR-head pipeline definitions for [release-4.10](https://github.com/stackrox/stackrox/blob/a9ca9e3c2241a3c85a0bf6b17db135ebbb7c0a02/.tekton/operator-bundle-pipeline.yaml#L382) and [release-4.11](https://github.com/stackrox/stackrox/blob/b119edb5790e7879feafcd8e0199b12e625fa18c/.tekton/operator-bundle-pipeline.yaml#L382), and the [shared retag pipeline](https://github.com/stackrox/stackrox/blob/a9ca9e3c2241a3c85a0bf6b17db135ebbb7c0a02/.tekton/retag-pipeline.yaml#L160). A missing upstream Scanner release image needs attention in its producer; retrying the retag alone cannot build that source image.

`ignoredKonfluxChecks` in `planner.go` is an arbitrary, tool-specific ignore list. It currently ignores `acs-conforma-dev` checks, including release-suffixed scenarios such as `acs-conforma-dev-5-0`, regardless of status or component. These checks produce no diagnostics or retry commands and do not block milestones. This is a deliberate noise filter, not a Konflux dependency rule.

The catalog and policy-name matching are in `planner.go`. Policy checks use the full shape `Red Hat Konflux / SCENARIO / COMPONENT`. New scenarios or components warn rather than being silently accepted.

## Selection rules

1. Resolve the PR head or tag to a commit SHA and query all pages of its check runs. For repeated check names, use the newest check-run ID, so a successful rerun supersedes an older failure.
2. Check input readiness for candidate `retag-collector`, `retag-fact`, and Scanner V2 retags as described below. Suggest completed checks with conclusions `failure`, `timed_out`, `cancelled`, `stale`, or `startup_failure` in the earliest unfinished milestone. Include failed independent `checks` at the same time. Jobs within a milestone can run in parallel.
3. Require every applicable predecessor in earlier milestones to have completed successfully before suggesting downstream jobs. Pending, missing, skipped, neutral, or action-required checks are not successful image producers and receive no retry command. Existing failures in their own milestone can still be retried.
4. Never retry policy checks automatically. Their failures may need CVE remediation or an upstream build fix.
5. Warn on every unknown Konflux check, even if successful. Recognize Konflux by the check-name prefix or its GitHub App name/slug. Unknown unfinished checks block milestones 2 and 3; they never become copyable commands.
6. Skip closed or merged PRs. Continue processing other PRs or tags after an API, registry-access, network, or requirement-resolution error, but exit with status 1. Invalid command-line arguments exit with status 2. Warnings and blocked milestones alone do not make the command fail.

Non-retag suggestions remain status-based. Outside the supported `wait-for-image` messages, the tool cannot distinguish a transient build failure from a reproducible code or configuration error. Repeated failures need investigation.

Branches with intentionally absent producers may need a catalog adjustment: the tool withholds downstream suggestions rather than assuming that a missing image dependency is satisfied. Each report is a snapshot of the checks for the commit resolved at the start of that PR or tag query.

## Retag input readiness

Before emitting a retag comment, the tool reads the PipelineRun, shared retag pipeline, Makefile recipe, and version file at the exact PR head (from the fork when applicable) or resolved tag commit. It resolves the input repository and the version plus `image-tag-suffix`, including run-level suffix overrides. It never executes code from the PR.

Without `--verify-pull`, the tool first reads the latest check's archived `wait-for-image` task from Tekton Results. It verifies the task's check-run ID, resolved commit SHA, PipelineRun, namespace, and exact image before using its logs. Explicit missing-image, missing-attestation, revision-mismatch, or failed-build messages justify waiting. An `Accepting this image.` message supports retrying a later failed task. A timeout alone is inconclusive.

Log-based decisions are labelled as inference from the latest run. They do not prove current registry state, and the task's “Image not found yet” message can also mask registry errors. If the logs are unavailable or inconclusive, the tool checks Quay directly. `--verify-pull` checks image existence first using the local Docker login and skips registry and log checks on success; ambiguous access errors do not count as absence.

Direct registry checks follow the consumer's [pinned wait-for-image task](https://github.com/stackrox/konflux-tasks/blob/39c045048877c4982eec677f92baac062a09a135/tasks/wait-for-image-task.yaml):

1. Read the image manifest and Linux/amd64 configuration, matching the Konflux worker platform rather than the CLI host.
2. Require `vcs-ref` and `source-location` labels. Require the resolved commit revision only when `source-location` exactly matches the consumer repository URL. Images from another repository, including Scanner, do not need the StackRox revision.
3. Require the digest's Cosign `.att` artifact. Accept SLSA v0.2 task results only when all statuses are `Succeeded`; for SLSA v1 with Tekton Chains v2, require a successful condition on every `pipelineTask` dependency.

These are the consumer's readiness checks, not cryptographic signature verification or full Conforma policy validation. The tool reads manifests, configuration, and attestations; it does not download image filesystem layers unless `--verify-pull` is enabled. It supports the inspected task bundles with digest prefixes `cc0e455b…`, `ad73f114…`, and `48cf6499…`. Unknown task bundles, changed parameter wiring, or unsupported Makefile recipes withhold the retest and report a resolution error so the tool does not assume that the consumer contract is unchanged.

Missing images produce `Investigate missing images`; other unavailable inputs produce `Wait`. Warnings name the exact required image. Other eligible jobs can still be suggested, and downstream milestones remain blocked. For example:

```text
---
https://github.com/stackrox/stackrox/pull/23958

Next action: Investigate missing images
Check the missing input images listed below.

Warnings:
Milestone 1 is not ready: retag-scanner (input unavailable).
retag-scanner: quay.io/rhacs-eng/release-scanner:2.39.27-fast: Konflux reported image not found.
Input availability inferred from the latest Konflux logs; not verified now.
```

Registry access is anonymous unless both `QUAY_USERNAME` and `QUAY_PASSWORD` are set. For private repositories, supply a Quay user or robot-account username and its password/token through those variables. Credentials are sent only to Quay's same-origin bearer-token endpoint, are never printed or saved, and are stripped from redirects. GitHub credentials are never sent to Quay. Private images or access restrictions produce authentication/access errors, not missing-image messages. Network/TLS failures, rate limits, unexpected registry responses, and malformed metadata also remain errors. If no conclusive log evidence is available, these errors withhold affected retests and cause exit status 1; a confirmed missing or unusable image alone does not.

The YAML parser is `go.yaml.in/yaml/v3`, which handles Tekton's YAML anchors and parameter defaults. Its Apache-2.0/MIT notices must be retained when distributing the dependency. It adds a parser dependency; registry access and provenance checks use the Go standard library.

## Validation

```sh
go test -cover ./...
go vet ./...
```

## Run evidence for library consumers

`Result.Runs` exposes latest job check IDs, status, conclusion, an HTTPS details
link when available, identity confidence, and planner eligibility.
`Result.Advisories` exposes commit-scoped Conforma failures, including optional
development scenarios. These findings do not emit policy retries.

Set `Config.VerifyRunIdentity` to require original PipelineRun annotations
matching the repository, SHA, check ID, and PR number or complete tag ref.
This requires `KonfluxToken` access to the cluster API or Tekton Results archive.
Missing or ambiguous metadata leaves affected jobs unknown; unrelated verified
jobs remain usable with a partial error. The default CLI behavior is unchanged.
Strict reads request all check runs so a newer branch build cannot hide an older
matching tag build on the same commit.

The annotation contract follows upstream
[Pipelines-as-Code labels.go](https://github.com/tektoncd/pipelines-as-code/blob/ae3aab8c3a54d46fc8c52f65484b78df532d02dd/pkg/kubeinteraction/labels.go)
and [keys.go](https://github.com/tektoncd/pipelines-as-code/blob/ae3aab8c3a54d46fc8c52f65484b78df532d02dd/pkg/apis/pipelinesascode/keys/keys.go).
Original annotation values preserve full branch refs; sanitized labels do not.

Verified run observations expose a valid image reference in `ImageURL` and a
valid SHA-256 digest in `ImageDigest`. Each field stays empty if its output is
unavailable or invalid. Image output observations do not alter retry eligibility
or confirm registry availability.
