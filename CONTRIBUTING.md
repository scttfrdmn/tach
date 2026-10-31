# Contributing to tach

Thanks for your interest in tach. A few rules keep the project coherent.

## Issue first

Every change starts as a GitHub issue. Planning, roadmap, design and validation results live
in GitHub Issues, Labels, Milestones, and the project board — not in tracked markdown files.
The design document is the pinned "Design" issue; hardware validation results are recorded as
comments on their validation issues. Open or find an issue before you start; reference it in
your commits and PR.

## Conventional commits

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/):

```
feat: add a cfd profile
fix: count dmon rows with a missing trailing field as blank, not dropped
chore: bump golangci-lint
docs: document the MIG entity syntax dcgmi accepts
test: parse captured g6e dmon output
ci: fuzz the dmon parser
```

Reference issues in the body or footer: `Refs #12`, `Closes #12`.

## Changelog

Every user-visible change gets a line under `[Unreleased]` in `CHANGELOG.md`, using the
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) headings (`Added`, `Changed`,
`Deprecated`, `Removed`, `Fixed`, `Security`). The PR template has a checkbox for this.

## Versioning

[Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html), on 0.x. Releases are tagged
`vX.Y.Z`; the release workflow takes the release notes from that version's `CHANGELOG.md`
section and fails if there isn't one.

## The summary JSON is a contract

`<csv>.summary.json` is read by `tach report` and `tach annotate`, and by other tools that use
tach as their instrument. **Add fields; do not rename or remove them.** Finding `code`s are
stable identifiers; their `text` is for people and may change.

Two tests hold this:

- `TestSummaryContract` compares every JSON path a summary can carry with
  `testdata/contract/summary-fields.txt`. A missing path fails as a contract break; a new
  path fails until it is added to the list.
- `TestGoldenSummaries` compares the summary from each mock scenario with
  `testdata/golden/`. After an intended output change, regenerate both with
  `go test -run 'Golden|Contract' -update .` and review the diff in the PR.

## Findings thresholds

The thresholds in `summary.go` are heuristics. Change one only when a validation run shows it
misfires, and name that run (issue and capture) in the commit message.

## DCGM behaviour is measured, not assumed

What tach knows about `dcgmi dmon` output, field units and entity syntax starts from
documentation. When a real run disagrees, the run wins:

- Keep the raw output under `testdata/real/<instance>-<what>.txt` and add a test that reads it.
- A fix to parsing or units comes with that test, so the mock can't drift from reality again.
- Record the instance type, GPU, driver and DCGM version with every measurement.
- Repeat a run before quoting it, and report the range rather than one draw.
- Keep AWS account numbers out of committed files and issues.

## Before you push

- `make lint` (`go vet` + `golangci-lint`) is clean.
- `make test` (race-enabled; includes the integration tests against the mock `dcgmi`) passes; new code has tests and touches no network.
- Every `.go` file starts with the header:

  ```go
  // Copyright 2026 Scott Friedman
  // SPDX-License-Identifier: Apache-2.0
  ```

## License

tach is Copyright 2026 Scott Friedman and licensed under the Apache License 2.0. By
contributing you agree that your contributions are licensed under the same terms.
