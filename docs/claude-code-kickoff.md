# tach — Claude Code kickoff

You're picking up `tach`, a Go tool that wraps a command and records GPU utilization and efficiency through NVIDIA DCGM. It answers the right-sizing questions for one run (is the GPU busy, are tensor cores used, is FP64 on the critical path, is it memory-bound, how much memory it needs) and reports time, energy and $ per unit of work. `tach report` compares runs across instance types; that table is what a GPU right-sizing clinic hands a researcher.

Read `README.md` first. It is the spec, including the findings thresholds and the "Validation status" section.

## Where things stand

- Source for v0.1.0 is in this directory: `fields.go`, `dmon.go`, `stats.go`, `summary.go`, `report.go`, `imds.go`, `main.go`, `tach_test.go`, `scripts/e2e.sh`, `testdata/mock-dcgmi`. Go standard library only; it shells out to `dcgmi`.
- It was written in a cloud session and passed `gofmt`, `go vet`, `go test -race`, `scripts/e2e.sh` and linux/amd64 + linux/arm64 cross-compiles there. **It has never run against real DCGM.** Everything about `dcgmi dmon` output format, field units and entity syntax is from documentation and needs confirming on hardware.
- There is no git history here and no GitHub repo yet; the session couldn't write `.git` or `.github/` on this Mac.

## Step 1 — repo

1. `git init -b main`, commit everything as `tach v0.1.0 (pre-validation)`.
2. Create a **private** repo `scttfrdmn/tach` and push.
3. Add `.github/workflows/ci.yml`:

   ```yaml
   name: ci
   on: [push, pull_request]
   jobs:
     test:
       runs-on: ubuntu-latest
       steps:
         - uses: actions/checkout@v4
         - uses: actions/setup-go@v5
           with:
             go-version: stable
         - run: test -z "$(gofmt -l .)"
         - run: go vet ./...
         - run: go test -race ./...
         - run: bash scripts/e2e.sh
         - run: GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /dev/null .
   ```

4. Track the work below as GitHub issues (one per validation item and per defect found), not as status files in the repo. Close issues from commits with `fixes #N`.

## Step 2 — local checks on macOS

Run `go vet ./...`, `go test -race ./...` and `bash scripts/e2e.sh`. The mock and the e2e script were written on Linux; macOS ships bash 3.2 and BSD tools. If either breaks, fix the scripts so they run on both (CI stays on Linux).

## Step 3 — real-hardware validation

Use spore.host `spawn` for instances: spot where available, a short TTL on every instance, terminate when done. Deep Learning Base GPU AMI (has DCGM). Start `nv-hostengine` as root. Build tach on the instance or cross-compile.

Instances: `g6e.xlarge` (L40S, Ada, PCIe Gen4) and `g7e.2xlarge` (RTX PRO 6000 Blackwell, Gen5, MIG-capable). Add `p5.4xlarge` (1× H100) only for item 5.

1. **dmon format.** Capture raw `dcgmi dmon -e 1002,1003,1004,1005,1006,250,252,155,156 -d 1000 -c 5` output and the DCGM version into `testdata/real/<instance>-dmon.txt`. Check `parseDmonLine` against it: header lines, entity column layout, N/A spelling, values that aren't plain floats. Replace or extend the mock so it matches real output exactly, and add a parser test that reads the captured files.
2. **Idle and fallback.** `tach -- sleep 30` should give `starved`. Stop `nv-hostengine` profiling access (or run unprivileged) and confirm the device-only fallback, `dcp: false`, and a neutral verdict.
3. **PCIe units.** A fixed-size pinned `cudaMemcpy` loop (H2D, then D2H) at a known rate, under `--pcie`. Confirm fields 1009/1010 are bytes/second and that TX/RX map to the directions the README says. Fix README and code to match what is measured.
4. **Energy.** Compare the counter delta (field 156, documented as mJ) against the trapezoid of `power_w` for the same run. They should agree within a few percent; if not, find out why.
5. **FP64 and tensor findings.** A PyTorch matmul loop in FP32, BF16 and FP64 on each instance. Expected: FP32 → `tensor_idle`; BF16 → tensor_active high, no `tensor_idle`; FP64 → `fp64_required`, and on H100 a much higher FP64 throughput than on L40S/RTX PRO. Record achieved TFLOPS next to the pipe-active values.
6. **A real HPC code.** GROMACS benchmark (e.g. benchMEM) under `--profile hpc` and `triage`, with `--work` set from the log's ns/day and `--price-per-hour` from on-demand pricing. Run `tach report` across g6e and g7e.
7. **Ada multiplexing.** On g6e, run `triage` at 1 s and 200 ms and record the blank rate per field. If the default profile blanks badly on Ada, change the default sampling or split groups.
8. **MIG.** On g7e, 2× and 4× MIG with one job per instance. Confirm the `--entities` syntax `dcgmi` actually accepts, that per-instance rows appear, and that energy and PCIe stay whole-GPU.

Put the results in `VALIDATION.md`: what was run, on which instance and DCGM version, measured vs expected, and what changed in the code because of it. Keep raw outputs under `testdata/real/`. Keep AWS account numbers out of committed files and issues.

## Working rules

- Fix defects in tach as you find them; the instrument is ours. Each fix gets a test, against captured real output where possible.
- Make technical decisions yourself from the evidence and record them in the issue or commit message. Bring Scott only what is his: scope changes, version numbers, tags and releases, making the repo public.
- Findings thresholds in `summary.go` are heuristics. Change them only when a validation run shows they misfire, and say which run.
- Do not declare the project or the validation done. Report the state: what was measured, what passed, what is open.
- Commit messages and PRs end with the attribution lines your harness specifies.

## Context

tach is the measurement instrument behind a GPU right-sizing method for academic research workloads (AI and non-AI: MD, DFT, CFD, cryo-EM, genomics). The method: profile the researcher's real job, classify the bottleneck from DCGM counters, apply the precision gate (FP64 codes need A100/H100/H200/B200; L4, L40S, RTX PRO and B300 run FP64 at about 1:64), then compare time and $ per result on two or three instance types. tach is also named as the instrument in the $/result on-prem vs cloud harness and the Sanders-Brown Dorado calibration plan, so the per-run JSON is a contract other tools read: add fields, don't rename them.
