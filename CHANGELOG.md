# Changelog

This project follows [Semantic Versioning](https://semver.org/).

## Unreleased

### Added

1. `Result.StartFailures` reports explicit PipelineRun creation failures by check ID, job, and cluster. It distinguishes admission-policy rejection from other start failures and includes a short explanation.

### Fixed

1. Checks that report a PipelineRun creation failure skip identity lookup and remain ineligible for automatic retries.
2. Excluded developer Conforma checks skip build identity lookup. Their failures remain visible as commit-scoped advisories without causing assessment errors or monitoring backoff.
3. Strict assessments distinguish an empty GitHub check inventory from a commit with no verified build checks for the requested tag or PR. Lookup failures still report their causes.
4. Konflux diagnostics redact credentials from URLs embedded in error messages and omit malformed redirect locations.
5. Registry lookups preserve caller cancellation and deadline errors while reading image metadata or token responses.

### Changed

1. Repository and retag Makefile validation reuse precompiled patterns.
2. Retry planning reuses the initial plan when retag input checks find no unavailable inputs.
3. Reports use normalized check links, validated image references and digests, and concise diagnostics.
4. Registry downloads support signed storage redirects with minimal request headers.

## [0.1.0]

### Added

1. `CheckPR` and `CheckTag` expose check-run IDs, outcomes, retry eligibility, and Conforma advisories to Go callers.
2. Set `Config.VerifyRunIdentity` to match runs to the requested PR or tag. Verified runs include the image URL and digest when available.

### Fixed

1. PipelineRun identity errors identify affected jobs and report failures from both the live API and the archive when both lookups fail.
2. Tekton Results lookups use a supported page size and leave time for the archive when the live lookup stalls.

[0.1.0]: https://github.com/vikin91/konflux-retest/compare/v0.0.1...v0.1.0
