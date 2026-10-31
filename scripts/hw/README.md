# Hardware validation scripts

Repeatable workloads and captures for the real-DCGM validation ([#6](https://github.com/scttfrdmn/tach/issues/6)), so every instance runs the same commands.

| Script | Answers |
| --- | --- |
| `capture.sh <outdir>` | Raw `dcgmi dmon` output, direct and piped, with per-line arrival times (pipe buffering) and SIGINT behaviour ([#7](https://github.com/scttfrdmn/tach/issues/7)); N/A rate of the triage fields at 1 s and 200 ms ([#13](https://github.com/scttfrdmn/tach/issues/13)); `dmon --help` for the `-i` syntax ([#14](https://github.com/scttfrdmn/tach/issues/14)) |
| `pcie_copy.cu` | Pinned H2D then D2H copy at a known GB/s, for PCIe units and direction ([#9](https://github.com/scttfrdmn/tach/issues/9)) |
| `matmul.py` | FP32 (TF32 off), BF16 or FP64 matmul load with achieved TFLOPS ([#10](https://github.com/scttfrdmn/tach/issues/10), [#11](https://github.com/scttfrdmn/tach/issues/11), [#13](https://github.com/scttfrdmn/tach/issues/13)) |
| `run-all.sh <outdir>` | The per-instance sequence: captures, then tach runs for idle ([#8](https://github.com/scttfrdmn/tach/issues/8)), PCIe, energy, the three precisions, 1 s vs 200 ms; optional `GROMACS=1` ([#12](https://github.com/scttfrdmn/tach/issues/12)) and `MIG=1` ([#14](https://github.com/scttfrdmn/tach/issues/14)); ends with a summary table |

## Running on an instance

On a Deep Learning Base GPU AMI (driver, CUDA, DCGM), with tach built on the instance or copied over:

```
python3 -m venv ~/tach-venv && ~/tach-venv/bin/pip install torch    # or INSTALL_TORCH=1
TACH=~/tach PYTHON=~/tach-venv/bin/python scripts/hw/run-all.sh testdata/real
```

The scripts start `nv-hostengine` as root (via sudo) if it is not running. `MIG=1` only prints the MIG sequence; `MIG_APPLY=1` runs it, which reconfigures GPU 0 and needs a GPU reset. The no-profiling fallback run of #8 is done by hand.

## Outputs

Everything goes to `<outdir>/<instance>-*`, `<instance>` being the EC2 instance type from IMDSv2 (`unknown` off EC2): `-identity.txt` and `-dmon*.txt` from `capture.sh`; per tach run `-<name>.csv`, `.csv.summary.json`, `.out.txt` (workload stdout) and `.tach.txt` (tach stderr). Commit them under `testdata/real/` as fixtures.

DCGM rejects a whole `dmon` if one field is unsupported (`fp64_active` on an L40S: "Feature not supported"). So `capture.sh` probes each field with `dmon -e <id> -c 1` and, after the first `-dmon.txt` capture (which keeps every field so the error is on record), requests only the fields that return a row. `-identity.txt` lists the requested, supported and dropped fields.

Every output is scrubbed of AWS account IDs where they appear (ARNs, ECR hosts, `account` keys), which become `<acct>`. Other 12-digit numbers are left alone, since they are often measurements (PCIe B/s, float fractions), and reported for a check by hand. Check before committing anyway: no account number goes into a committed file or an issue.
