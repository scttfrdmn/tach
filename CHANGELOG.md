# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- `starved` no longer says "idle most of the run" for a GPU that had work in nearly every
  sample at low SM activity (GROMACS benchMEM on an RTX 5090 runs at sm_active ~0.3
  throughout); it now says the kernels are too small for the GPU or it waits on the host.
  Busy samples are those with sm_active >= 0.05, not 0.30.

- `tach report` prints per-unit figures in plain decimals (`0.00002`, not `2e-05`), adds a `findings` column of finding codes to `--format csv`, keeps newlines out of markdown cells, and re-analyses only summaries without a verdict; the EC2 instance-type lookup now has a 400 ms total budget, closes its connection, and rejects an oversize or malformed response (#52).

## [0.1.0] - 2026-10-31

First release. Measured with DCGM 4.7 on an H100 (p5.4xlarge), an RTX PRO 6000 Blackwell
(g7e.2xlarge), an L40S (g6e.xlarge) and a GeForce RTX 5090: dmon format and pipe behaviour,
idle, PCIe units and direction, energy counter against integrated power, FP32/BF16/FP64
findings, sampling blanks, and MIG 2x/4x. Raw captures are in `testdata/real/`; results and
what is still open (GROMACS, the no-profiling fallback on real hardware) are on issue #6.

### Added

- **`tach [flags] -- <command>`** wraps a command, samples DCGM through `dcgmi dmon`, and
  passes the command's exit code through (128+N when the command is killed by signal N).
  A Ctrl-C reaches the command once, and sampling continues while the command shuts down.
  Writes a per-sample CSV (blank cells are missing samples, not zeros) and a
  `<csv>.summary.json` with per-entity mean/p50/p95/min/max.
- **Profiles** `triage` (default), `hpc` and `inference`, each a set of DCGM profiling fields
  plus device fields (power, SM clock, framebuffer used/total, GPU utilization, temperature).
  `--pcie` adds PCIe TX/RX (fields 1009/1010) and raises the interval to 1 s.
- **Fields the GPU does not support are dropped, not fatal.** DCGM rejects a whole watch set
  that contains one unsupported field (`fp64_active` on an L40S: "Feature not supported"),
  which left every run on that GPU with no samples. tach now drops such fields, warns, and
  lists them as `unsupported_fields` in the summary.
- **Device-level fallback** when profiling fields are unavailable, recorded as `dcp: false` and
  reported as such rather than as an idle GPU.
- **dcgmi failures are not mistaken for a device-only run.** When `dcgmi` cannot read the GPUs
  (typically no `nv-hostengine`), tach exits 1 before starting the command and prints dcgmi's
  error. If the sampler records nothing during a run, tach warns, keeps dcgmi's message in
  `notes`, and reports the `no_samples` finding with the verdict "no data".
- **Energy** from the DCGM energy counter delta per whole GPU, falling back to integrating
  sampled power (`energy_method: counter | power-integral | none`). The counter is read just
  before the command starts and just after it exits, so short runs are not inflated by tach's
  own startup and closing sample.
- **Heuristic findings and a verdict**: `starved`, `pcie_bound`, `low_occupancy`,
  `memory_bound`, `fp64_required`, `fp64_unused`, `fp64_unmeasured`, `tensor_idle`,
  `memory_headroom`, `no_dcp`, `no_samples`. FP64 activity includes FP64 on tensor cores
  (measured on H100: DGEMM shows only as tensor activity no hmma/imma counter claims), and
  `low_occupancy` is not reported for saturated GEMMs. Thresholds were checked against
  FP32/BF16/FP64 matmul runs on H100, RTX PRO 6000, L40S and GeForce RTX 5090.
- **Findings judge the busy samples.** `starved` uses the whole run and says what share of
  samples was busy; pipe and precision findings use the samples with sm_active ≥ 0.30, so a
  long load or staging phase no longer turns an FP64 job into `fp64_unused`. Summaries gain
  `busy_fraction` and `busy_fields` per entity.
- **Per-unit figures** with `--work`, `--unit` and `--price-per-hour`: seconds, joules and
  dollars per unit. `tach annotate` adds them to an existing summary after the run.
- **`tach report`** compares runs as a Markdown or CSV table, sorted by cost per unit, then
  time per unit, then wall time. It refuses runs that measure different units rather than compare
  unlike quantities.
- **MIG instances** through `--entities` (passed to `dcgmi -i`); energy and PCIe stay
  whole-GPU. With instance-only entities the energy counter is read from the parent GPU(s),
  once per GPU, instead of being lost.
- **Instance type** from EC2 metadata (IMDSv2, 400 ms timeout), and a PCIe line rate inferred
  from the instance family; `--instance`, `--no-imds` and `--pcie-linerate` override.
- **PCIe line rate from the negotiated link.** With `--pcie`, tach reads DCGM's max link gen
  and current width (fields 235/238) and uses the slowest GPU's link (the L40S on g6e.xlarge
  runs Gen4 x8, 15.75 GB/s, not the family's 31.5); the instance table, now Gen3 for
  g4dn/g5g, is the fallback. The summary records `pcie_link` and `pcie_linerate_source`.
- `tach version` prints the version, commit and build date.
