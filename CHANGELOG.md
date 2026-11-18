# Changelog

This project follows [Semantic Versioning](https://semver.org/).

## Unreleased

### Fixed

1. Excluded developer Conforma checks skip build identity lookup. Their failures remain visible as commit-scoped advisories without causing assessment errors or monitoring backoff.
2. Strict assessments distinguish an empty GitHub check inventory from a commit with no verified build checks for the requested tag or PR. Lookup failures still report their causes.

## [0.1.0]

### Added

1. `CheckPR` and `CheckTag` expose check-run IDs, outcomes, retry eligibility, and Conforma advisories to Go callers.
2. Set `Config.VerifyRunIdentity` to match runs to the requested PR or tag. Verified runs include the image URL and digest when available.

### Fixed

1. PipelineRun identity errors identify affected jobs and report failures from both the live API and the archive when both lookups fail.
2. Tekton Results lookups use a supported page size and leave time for the archive when the live lookup stalls.

[0.1.0]: https://github.com/vikin91/konflux-retest/compare/v0.0.1...v0.1.0
