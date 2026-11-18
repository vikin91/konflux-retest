# Changelog

This project follows [Semantic Versioning](https://semver.org/).

## [0.1.0]

### Added

1. `CheckPR` and `CheckTag` expose check-run IDs, outcomes, retry eligibility, and Conforma advisories to Go callers.
2. Set `Config.VerifyRunIdentity` to match runs to the requested PR or tag. Verified runs include the image URL and digest when available.

### Fixed

1. PipelineRun identity errors identify affected jobs and report failures from both the live API and the archive when both lookups fail.
2. Tekton Results lookups use a supported page size and leave time for the archive when the live lookup stalls.

[0.1.0]: https://github.com/vikin91/konflux-retest/compare/v0.0.1...v0.1.0
