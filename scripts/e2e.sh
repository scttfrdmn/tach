#!/usr/bin/env bash
# End-to-end checks against testdata/mock-dcgmi. Real-hardware validation is
# separate (see README, "Validation status").
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mock="$root/testdata/mock-dcgmi"
go build -race -o "$work/tach" "$root"
cd "$work"
fail() { echo "FAIL: $*" >&2; exit 1; }
t() { MOCK_ENERGY="$work/e_$1" "$work/tach" --dcgmi "$mock" --no-imds --csv "$1.csv" "${@:2}"; }

t fp64 --interval 200ms --work 10 --unit ns --price-per-hour 2 -- sleep 1.1 2>/dev/null
grep -q '"fp64_required"' fp64.csv.summary.json || fail "fp64 finding"
grep -q '"energy_method": "counter"' fp64.csv.summary.json || fail "energy counter"
grep -q '"dollars"' fp64.csv.summary.json || fail "per-unit cost"

MOCK_SCENARIO=starved t starved --pcie --instance g7e.4xlarge -- sleep 1.1 2>/dev/null
grep -q '"pcie_bound"' starved.csv.summary.json || fail "pcie finding"

MOCK_SCENARIO=ai_fp32 t ai -- sleep 1.1 2>/dev/null
grep -q '"tensor_idle"' ai.csv.summary.json || fail "tensor finding"

MOCK_NOPROF=1 t noprof -- sleep 1.1 2>/dev/null
grep -q '"dcp": false' noprof.csv.summary.json || fail "dcp fallback flag"
head -1 noprof.csv | grep -q active && fail "profiling columns in fallback CSV"

MOCK_MIG=1 t mig -- sleep 1.1 2>/dev/null
[ "$(cut -d, -f3 mig.csv | grep -c 'GPU-I')" -gt 0 ] || fail "MIG rows"

set +e
t ex -- sh -c 'exit 3' >/dev/null 2>&1
[ $? -eq 3 ] || fail "exit code passthrough"
set -e

"$work/tach" annotate --work 5 --unit ns noprof.csv.summary.json 2>/dev/null
"$work/tach" report fp64.csv.summary.json ai.csv.summary.json noprof.csv.summary.json > report.md
grep -q '^| run |' report.md || fail "report table"
echo "e2e: all checks passed"
