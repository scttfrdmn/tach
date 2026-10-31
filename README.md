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
- For profiling fields: a datacenter GPU and `nv-hostengine` running as root (`sudo systemctl --now enable nvidia-dcgm`). Without them tach records device-level fields only and says so.

## Build

```
make build          # bin/tach
make build-arm64    # bin/tach-linux-arm64, static (g5g)
```

Plain `go build .` also works. `make test`, `make e2e` and `make lint` run the checks CI runs.

## Profiles

| Profile | DCGM profiling fields | Use |
| --- | --- | --- |
| `triage` (default) | sm_active, sm_occupancy, tensor_active, dram_active, fp64_active | Right-sizing: busy, tensor use, FP64 need, memory-bound |
| `hpc` | sm_active, sm_occupancy, dram_active, fp64_active, fp32_active | Simulation codes: compute vs memory, FP64 vs FP32 |
| `inference` | sm_active, dram_active, tensor_active, fp16_active | Inference engines: are the tensor cores fed |

Every profile adds device fields: power_w, sm_clock_mhz, fb_used_mib, fb_total_mib, gpu_util_pct, gpu_temp_c. `--pcie` adds pcie_tx_bps and pcie_rx_bps (fields 1009/1010) and raises the interval to 1 s, because PCIe counters are a separate metric group.

On Ada (L4, L40S) some profiling fields cannot be sampled together; DCGM multiplexes them and some samples come back blank. Blanks are counted as missing, not as zero. Blackwell FP8/FP4 work is reported inside tensor_active; there is no separate field.

## Output

- `tach-<unix>.csv`: one row per sample per entity (`timestamp, elapsed_s, entity_type, entity_id, <fields>`). Blank cells are missing samples.
- `<csv>.summary.json`: per-entity mean/p50/p95/min/max, energy, per-unit figures, findings and a verdict.
- A human summary on stderr.

Energy comes from the DCGM energy counter (`DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION`) as a start/stop delta per whole GPU. It includes anything else running on those GPUs. When the counter is unavailable tach integrates sampled power and records `energy_method: power-integral`.

tach's exit code is the wrapped command's exit code.

## Findings

Findings are heuristics over GPU-level means. The CSV is the ground truth.

| Code | Condition | Reading |
| --- | --- | --- |
| `starved` | sm_active < 0.30 | GPU mostly idle; check data loading and host-side work first |
| `pcie_bound` | starved and PCIe p95 ≥ 60% of line rate | Input-bound or host-device round-trips |
| `low_occupancy` | sm_active ≥ 0.50, sm_occupancy < 0.25 | Small kernels or batches; batch up or share the GPU (MPS/MIG) |
| `memory_bound` | dram_active ≥ 0.50, all pipes < 0.30 | Choose on memory bandwidth, not FLOPs |
| `fp64_required` | fp64_active ≥ 0.05 | Needs 1:2 FP64 (A100, H100/H200, B200) |
| `fp64_unused` | fp64_active < 0.01 | No FP64-class GPU needed |
| `tensor_idle` | tensor_active < 0.05 while busy, not FP64-bound, not `hpc` profile | Likely FP32; try BF16 or TF32 |
| `memory_headroom` | peak fb_used < 25% of installed | A smaller GPU or MIG slice fits |
| `no_dcp` | profiling fields unavailable | Device-level read only |

## Per-unit figures and reports

`--work N --unit U --price-per-hour P` adds seconds, joules and dollars per unit. The work count is often known only after the run (ns/day from a GROMACS log, samples from a training log), so it can be added later:

```
tach annotate --work 12.5 --unit ns --price-per-hour 2.24 tach-1729790000.csv.summary.json
tach report --format md runs/*.summary.json
```

`report` sorts by cost per unit when every run has a price, otherwise by time per unit, otherwise by wall time.

## MIG

`--entities i:0,i:1` (passed to `dcgmi -i`) measures MIG instances. DCGM attributes profiling fields per instance; energy and PCIe stay whole-GPU only, because DCGM has no per-instance counters for them.

## Instance type and PCIe line rate

tach reads the instance type from EC2 metadata (IMDSv2, 400 ms timeout); `--instance` overrides it and `--no-imds` skips the lookup. The PCIe line rate defaults from the instance family: 31.5 GB/s per direction (Gen4 x16) for g4dn, g5, g6, g6e, p4d; 63 GB/s (Gen5 x16) otherwise. `--pcie-linerate` overrides it.

DCGM documents fields 1009/1010 as bytes per second including protocol headers. tach records them as such; the first hardware validation checks this against a known `cudaMemcpy` rate.

## Validation status

v0.1.0 is tested against a mock `dcgmi` (`testdata/mock-dcgmi`, `scripts/e2e.sh`): parsing, profiling fallback, MIG entities, PCIe, energy, per-unit math, findings, exit codes. It has not yet run on real GPUs. The hardware validation (dmon format, idle and fallback, PCIe units, energy, FP64 and tensor findings, GROMACS, Ada multiplexing, MIG) is tracked in [#6](https://github.com/scttfrdmn/tach/issues/6).

## License

Apache-2.0. Copyright 2026 Playground Logic LLC.
