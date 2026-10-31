## Summary

<!-- What does this change and why. -->

## Related issue

<!-- e.g. Closes #12 / Refs #12 -->

## Checklist

- [ ] Commits follow Conventional Commits
- [ ] **CHANGELOG updated** under `[Unreleased]`
- [ ] `make lint` is clean (`go vet` + `golangci-lint`)
- [ ] `make test` passes (race-enabled) and new code has tests
- [ ] `make e2e` passes against the mock `dcgmi`
- [ ] Summary JSON fields are only added, never renamed or removed
- [ ] A fix to DCGM parsing or units is tested against captured output in `testdata/real/` where one exists
- [ ] New `.go` files carry the `// Copyright 2026 Scott Friedman` and `// SPDX-License-Identifier: Apache-2.0` header
