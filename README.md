# tach

`tach` wraps a command and records GPU utilization and efficiency over time through NVIDIA DCGM. It answers the right-sizing questions for one run: is the GPU busy, are the tensor cores used, is FP64 on the critical path, is the job memory-bound, and how much of the GPU's memory it needs. With a unit of work and an instance price it also reports time, energy and cost per unit, and `tach report` puts several runs side by side.

```
tach -- python train.py
tach --profile hpc --label g6e-gromacs --work 12.5 --unit ns --price-per-hour 2.24 -- gmx mdrun -deffnm bench
tach report runs/*.summary.json
```

One static Go binary, standard library only. It shells out to `dcgmi`, so there is nothing to link.

## Requirements

- `dcgmi` on `PATH` (installed with DCGM; present on the AWS Deep Learning AMIs).
- For profiling fields: `nv-hostengine` running (`sudo systemctl --now enable nvidia-dcgm`) on a GPU DCGM profiles. With DCGM 4.7 that was every GPU measured: H100, RTX PRO 6000, L40S and a GeForce RTX 5090, with the service running as its own `nvidia-dcgm` account. Where profiling is unavailable tach records device-level fields only and says so. If `dcgmi` cannot read the GPUs at all (usually `nv-hostengine` not running), tach stops before starting the command and prints dcgmi's error.

## Build

```
make build          # bin/tach
make build-arm64    # bin/tach-linux-arm64, static (g5g)
```

Plain `go build .` also works. `make test` and `make lint` run the checks CI runs.

## Profiles

| Profile | DCGM profiling fields | Use |
| --- | --- | --- |
| `triage` (default) | sm_active, sm_occupancy, tensor_active, dram_active, fp64_active, fp32_active, tensor_imma_active, tensor_hmma_active | Right-sizing: busy, tensor use, FP64 need, memory-bound |
| `hpc` | sm_active, sm_occupancy, dram_active, fp64_active, fp32_active, tensor_active, tensor_imma_active, tensor_hmma_active | Simulation codes: compute vs memory, FP64 vs FP32 |
| `inference` | sm_active, dram_active, tensor_active, fp16_active | Inference engines: are the tensor cores fed |

Every profile adds device fields: power_w, sm_clock_mhz, fb_used_mib, fb_total_mib, gpu_util_pct, gpu_temp_c. `--pcie` adds pcie_tx_bps and pcie_rx_bps (fields 1009/1010) and raises the interval to 1 s, because PCIe counters are a separate metric group.

Not every GPU supports every field. The L40S (Ada) has no `fp64_active`, and DCGM rejects a whole watch set that contains an unsupported field, so tach checks the set first, drops what the GPU does not support, warns, and lists it as `unsupported_fields` in the summary; findings that need a dropped field are skipped. Where DCGM multiplexes fields across metric groups some samples can come back blank; blanks are counted as missing, not as zero. Blackwell FP8/FP4 work is reported inside tensor_active; there is no separate field.

## Output

- `tach-<unix>.csv`: one row per sample per entity (`timestamp, elapsed_s, entity_type, entity_id, <fields>`). Blank cells are missing samples.
- `<csv>.summary.json`: per-entity mean/p50/p95/min/max, energy, per-unit figures, findings and a verdict.
- A human summary on stderr.

The summary's main keys:

| Key | Meaning |
| --- | --- |
| `tach_version`, `profile`, `command`, `exit_code`, `wall_seconds`, `interval_ms`, `dcp` | the run |
| `entities[]` | one per GPU or MIG instance: `type` (`GPU`, `GPU-I`), `id`, `samples`, `fields` (per-field `n`, `missing`, `mean`, `p50`, `p95`, `min`, `max` over the whole run), `busy_fraction` and `busy_fields` (the same stats over samples with sm_active ≥ 0.05) |
| `energy_joules`, `energy_method`, `energy_note` | energy and how it was measured |
| `pcie_linerate_gbs`, `pcie_link`, `pcie_linerate_source` | with `--pcie` |
| `unsupported_fields`, `notes` | fields the GPU lacks; anything tach had to say about the run |
| `work`, `unit`, `price_per_hour`, `per_unit` (`seconds`, `joules`, `dollars`) | from `--work`/`--unit`/`--price-per-hour` or `tach annotate` |
| `findings[]` | `code` (stable identifier, as in the table below) and `text` (for people; may change) |
| `verdict` | the findings as one line |

`testdata/contract/summary-fields.txt` lists every path, and a test keeps it current: paths are added, never renamed or removed.

Energy comes from the DCGM energy counter (`DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION`) as a start/stop delta per whole GPU. It includes anything else running on those GPUs. When the counter is unavailable tach integrates sampled power and records `energy_method: power-integral`.

tach's exit code is the wrapped command's exit code; a command killed by signal N gives 128+N, as a shell reports it.

## Findings

Findings are heuristics over GPU-level means. The CSV is the ground truth.

`starved` judges the whole run, and says which kind it is: long idle phases (waiting on data or the host), or work in nearly every sample at low SM activity (kernels too small for the GPU, or launch/CPU-bound). The other findings judge the **busy samples** (sm_active ≥ 0.05), so a job that spends most of its time loading data or a model is still read for what it does on the GPU. Each entity records `busy_fraction` and `busy_fields` (stats over the busy samples) next to the whole-run `fields`.

FP64 activity is `fp64_active` plus FP64 on tensor cores, estimated as `tensor_active` minus `tensor_hmma_active` and `tensor_imma_active`. On an H100 an FP64 DGEMM runs on the tensor cores and shows only there (`fp64_active` reads 0), while BF16, FP16 and TF32 count as hmma. GPUs that lack a field drop it; on such a GPU (the L40S has no `fp64_active`), FP64 need is reported only when it can be measured.

| Code | Condition | Reading |
| --- | --- | --- |
| `starved` | sm_active < 0.30 | GPU mostly idle; check data loading and host-side work first |
| `pcie_bound` | starved and PCIe p95 ≥ 60% of line rate | Input-bound or host-device round-trips |
| `low_occupancy` | sm_active ≥ 0.50, sm_occupancy < 0.25, no pipe ≥ 0.40 | Small kernels or batches; batch up or share the GPU (MPS/MIG). Not reported for saturated GEMMs, which run few warps per SM by design |
| `memory_bound` | dram_active ≥ 0.50, all pipes < 0.30 | Choose on memory bandwidth, not FLOPs |
| `fp64_required` | FP64 activity ≥ 0.05 | Needs 1:2 FP64 (A100, H100/H200, B200) |
| `fp64_unused` | FP64 activity < 0.01, and FP64 is measurable on this GPU | No FP64-class GPU needed |
| `fp64_unmeasured` | SMs ≥ 0.50 busy but every measured pipe < 0.10 | This GPU's counters do not show the work (FP64 on an RTX PRO 6000 or L40S); check the code's precision |
| `tensor_idle` | tensor_active < 0.05 while busy, not FP64-bound or unmeasured, not `hpc` profile | Likely FP32; try BF16 or TF32 |
| `memory_headroom` | peak fb_used < 25% of installed | A smaller GPU or MIG slice fits |
| `no_dcp` | profiling fields unavailable | Device-level read only |
| `no_samples` | dcgmi returned no data during the run | Nothing measured; dcgmi's message is in `notes` |

## Per-unit figures and reports

`--work N --unit U --price-per-hour P` adds seconds, joules and dollars per unit. The work count is often known only after the run (ns/day from a GROMACS log, samples from a training log), so it can be added later:

```
tach annotate --work 12.5 --unit ns --price-per-hour 2.24 tach-1729790000.csv.summary.json
tach report --format md runs/*.summary.json
```

`report` sorts by cost per unit when every run has a price, otherwise by time per unit, otherwise by wall time.

## MIG

`--entities i:0,i:1` (passed to `dcgmi -i`; DCGM also accepts `instance:1` and `0/*` for all instances on GPU 0) measures MIG instances. DCGM attributes profiling fields per instance, so each `GPU-I` entity has its own `sm_active` (busy fraction) and `tensor_active`. Per instance, `gpu_util_pct` is N/A and `power_w` is the whole GPU's power repeated. Energy and PCIe are whole-GPU only: with instance-only entities tach reads the energy counter of the parent GPU(s), found with `dcgmi discovery -c`, once per GPU, and says so in `energy_note`.

## Instance type and PCIe line rate

tach reads the instance type from EC2 metadata (IMDSv2, 400 ms timeout); `--instance` overrides it and `--no-imds` skips the lookup.

With `--pcie`, the line rate per direction that `pcie_bound` compares against comes from, in order:

1. `--pcie-linerate` (GB/s), recorded as `pcie_linerate_source: "flag"`.
2. The link DCGM reports, read once at start: max link generation (field 235) times current width (field 238), using the slowest of the GPUs measured (with MIG instances only, their parent GPUs). Not the current generation, which idles down (an idle RTX PRO 6000 reports Gen1). Per lane after encoding: Gen1 0.25, Gen2 0.5, Gen3 0.985, Gen4 1.969, Gen5 3.938, Gen6 7.563 GB/s, so Gen5 x16 is 63 GB/s and Gen4 x8 is 15.75. The summary records the link as `pcie_link` (e.g. `"gen4 x8"`) and `pcie_linerate_source: "dcgm-link"`. The link can be narrower than the instance family suggests: the L40S on g6e.xlarge negotiates Gen4 x8.
3. When DCGM does not report the link, the instance family (`pcie_linerate_source: "instance-table"`): 15.75 GB/s (Gen3 x16) for g4dn and g5g; 31.5 GB/s (Gen4 x16) for g5, g6, g6e, gr6, p4d; 63 GB/s (Gen5 x16) otherwise.

`pcie_rx_bps` is host→device and `pcie_tx_bps` is device→host (the GPU's view), in bytes per second including protocol overhead: measured pinned copies read 7-12% above the payload rate on an RTX PRO 6000 and an L40S.

The link rate is an upper bound, not the rate a host can reach. On p5.4xlarge the H100 reports a Gen5 x16 link (63 GB/s), yet pinned copies reached only 14 GB/s host→device and 7 GB/s device→host, so `pcie_bound` cannot fire there against the link rate. For an accurate `pcie_bound`, measure a baseline once per instance type: run pinned copies under `tach --pcie` (`scripts/hw/pcie_copy.cu`), and pass the larger of the p95 `pcie_rx_bps` and `pcie_tx_bps` it records, in GB/s, as `--pcie-linerate`. Use the counters, not the copy's payload rate, because `pcie_bound` compares the counters, overhead included.

## Validation status

Measured with DCGM 4.7 on four GPUs: H100 (p5.4xlarge), RTX PRO 6000 Blackwell (g7e.2xlarge), L40S (g6e.xlarge) and GeForce RTX 5090.

| Checked on hardware | Result |
| --- | --- |
| `dcgmi dmon` output and pipe behaviour | parser matches; no buffering on a pipe; exits ~0.5 s after SIGINT |
| Idle run | `starved` |
| PCIe fields | bytes/s including protocol overhead; `rx` host→device, `tx` device→host |
| Energy counter | within 0.2–0.7% of integrated power on 60–180 s runs |
| FP32 / BF16 / FP64 matmul | `tensor_idle` / no tensor finding / `fp64_required` (H100) or `fp64_unmeasured` (RTX PRO 6000, L40S, RTX 5090) |
| Sampling blanks at 1 s and 200 ms | none on any of the four |
| MIG 2× and 4× | per-instance rows; energy from the parent GPU |

Raw captures are in `testdata/real/`, and `TestParseRealCaptures` and `busy_test.go` replay them. Still open: a real HPC code (GROMACS), and the device-only fallback on a GPU DCGM does not profile; every GPU measured was fully profiled. Tracked in [#6](https://github.com/scttfrdmn/tach/issues/6).

## License

Copyright 2026 Scott Friedman. Licensed under the [Apache License, Version 2.0](LICENSE).

Changes are recorded in [CHANGELOG.md](CHANGELOG.md), following [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions follow [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html); tach is on 0.x.
