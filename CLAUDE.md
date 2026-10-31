# tach

Go tool that wraps a command and records GPU utilization and efficiency through NVIDIA DCGM
(`dcgmi dmon`). One static binary, standard library only, flat `package main`. `README.md` is
the user-facing spec, including the findings thresholds. The pinned "Design v0" issue holds the
design; work, milestones and validation results live in GitHub issues and the `tach` project
board, not in local files.

## Commands

- `make build` (→ `bin/tach`), `make test` (race), `make e2e` (mock `dcgmi`), `make lint`,
  `make fuzz`, `make build-arm64`.
- CI runs `.github/workflows/gate.yml`; the release workflow runs the same gate on a `v*` tag.

## Working rules

- The instrument is ours: fix defects in tach as they are found. Each fix gets a test, against
  captured real output under `testdata/real/` where possible.
- Everything about `dcgmi dmon` output, field units and entity syntax came from documentation
  until a hardware validation issue confirms it. The mock (`testdata/mock-dcgmi`) must be
  brought into line with captured real output, not the other way round.
- Findings thresholds in `summary.go` are heuristics. Change them only when a validation run
  shows they misfire, and name that run.
- The summary JSON is a contract that `tach report`, `tach annotate` and downstream tools read:
  add fields, don't rename them.
- Make technical decisions from the evidence and record them in the issue or commit message.
  Bring Scott only what is his: scope changes, version numbers, tags and releases, making the
  repo public.
- Do not declare the project or the validation done. Report the state: what was measured,
  what passed, what is open.
- Record validation results as comments on the validation issue (instance, DCGM version,
  measured vs expected, what changed). Keep AWS account numbers out of committed files and
  issues.

## Hardware runs

Spot instances where available, a short TTL on every instance, terminate when done. Deep Learning Base GPU AMI (has DCGM); start `nv-hostengine` as root.
Build tach on the instance or cross-compile (`make build-arm64` for g5g).

## Context

tach is the measurement instrument behind a GPU right-sizing method for academic research
workloads (AI and non-AI: MD, DFT, CFD, cryo-EM, genomics). The method: profile the
researcher's real job, classify the bottleneck from DCGM counters, apply the precision gate
(FP64 codes need A100/H100/H200/B200; L4, L40S, RTX PRO and B300 run FP64 at about 1:64), then
compare time and $ per result on two or three instance types. `tach report` produces the table
a right-sizing clinic hands a researcher.
