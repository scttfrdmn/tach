#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# run-all.sh <outdir>: the per-instance hardware validation sequence (#7-#14).
#
#   1. start nv-hostengine as root if needed
#   2. capture.sh (raw dcgmi captures, #7 #13)
#   3. build pcie_copy
#   4. tach runs, each writing <outdir>/<instance>-<name>.csv (+ .summary.json),
#      the workload's stdout to <instance>-<name>.out.txt and tach's stderr to
#      <instance>-<name>.tach.txt
#   5. a summary table over all summaries
#
# Env:
#   TACH=/path        tach binary (default: tach on PATH)
#   PYTHON=/path      python with torch (default python3); INSTALL_TORCH=1
#                     creates ~/tach-venv and pip-installs torch into it
#   SKIP_CAPTURE=1    skip capture.sh
#   GROMACS=1         also run GROMACS benchMEM (#12) if gmx is on PATH
#                     (BENCHMEM_URL overrides the download URL)
#   MIG=1             print the MIG 2x/4x sequence (#14); MIG_APPLY=1 runs it
#                     (enables MIG on GPU 0, which needs a GPU reset);
#                     MIG_2X / MIG_4X profile lists, MIG_ENTITIES_2X /
#                     MIG_ENTITIES_4X --entities values (default i:0,...)
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh source-path=SCRIPTDIR
source "$here/lib.sh"

[[ $# -eq 1 ]] || {
	echo "usage: $0 <outdir>   (normally testdata/real)" >&2
	exit 2
}
mkdir -p "$1"
out="$(cd "$1" && pwd)"
TACH="${TACH:-tach}"
command -v "$TACH" >/dev/null 2>&1 || {
	echo "run-all: tach not found (set TACH=/path/to/tach)" >&2
	exit 1
}
TACH="$(command -v "$TACH")"
export PATH="$PATH:/usr/local/cuda/bin"
inst="$(instance_type)"
export INSTANCE="$inst"
p="$out/$inst"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

echo "run-all: instance=$inst outdir=$out tach=$TACH ($("$TACH" --version 2>&1 | head -1 || true))"

# ---- 1. nv-hostengine ------------------------------------------------------
ensure_hostengine

# ---- python with torch -----------------------------------------------------
PYTHON="${PYTHON:-python3}"
if ! "$PYTHON" -c 'import torch' >/dev/null 2>&1 && [[ "${INSTALL_TORCH:-0}" == 1 ]]; then
	echo "run-all: installing torch into ~/tach-venv"
	python3 -m venv "$HOME/tach-venv"
	"$HOME/tach-venv/bin/pip" install --quiet torch
	PYTHON="$HOME/tach-venv/bin/python"
fi
have_torch=0
if "$PYTHON" -c 'import torch, sys; sys.exit(0 if torch.cuda.is_available() else 1)' >/dev/null 2>&1; then
	have_torch=1
else
	echo "run-all: torch with CUDA not available to $PYTHON; matmul runs are skipped." >&2
	echo "         install: python3 -m venv ~/tach-venv && ~/tach-venv/bin/pip install torch" >&2
	echo "         then rerun with PYTHON=~/tach-venv/bin/python (or INSTALL_TORCH=1)" >&2
fi
matmul=("$PYTHON" "$here/matmul.py")

# ---- 2. raw captures -------------------------------------------------------
if [[ "${SKIP_CAPTURE:-0}" != 1 ]]; then
	if [[ $have_torch == 1 ]]; then
		LOAD_CMD="$(printf '%q ' "${matmul[@]}") --dtype bf16 --seconds 90" "$here/capture.sh" "$out"
	else
		"$here/capture.sh" "$out"
	fi
fi

# ---- 3. pcie_copy ----------------------------------------------------------
pcie_bin=""
if command -v nvcc >/dev/null 2>&1; then
	nvcc -O2 -o "$work/pcie_copy" "$here/pcie_copy.cu"
	pcie_bin="$work/pcie_copy"
else
	echo "run-all: nvcc not found (also looked in /usr/local/cuda/bin); PCIe run skipped" >&2
fi

# ---- 4. tach runs ----------------------------------------------------------
runs=()
# run <name> <tach flags...> -- <command...>
run() {
	local name="$1" rc=0
	shift
	echo "run-all: [$name] $*"
	"$TACH" --csv "$p-$name.csv" --label "$inst-$name" "$@" \
		>"$p-$name.out.txt" 2>"$p-$name.tach.txt" || rc=$?
	echo "run-all: [$name] exit=$rc"
	runs+=("$name")
}

# #8 idle: expect starved / underutilized
run idle -- sleep 30

# #9 PCIe units and direction: H2D 20 s then D2H 20 s at a known rate
if [[ -n "$pcie_bin" ]]; then
	run pcie --pcie -- "$pcie_bin"
fi

if [[ $have_torch == 1 ]]; then
	# #10 energy counter vs integrated power on a steady load
	run energy -- "${matmul[@]}" --dtype bf16 --seconds 180

	# #11 FP32 (TF32 off) / BF16 / FP64 findings
	for dt in fp32 bf16 fp64; do
		run "matmul-$dt" --profile triage -- "${matmul[@]}" --dtype "$dt" --seconds 60
	done

	# #13 Ada multiplexing blank rate at 1 s and 200 ms
	run triage-1s --profile triage --interval 1s -- "${matmul[@]}" --dtype bf16 --seconds 60
	run triage-200ms --profile triage --interval 200ms -- "${matmul[@]}" --dtype bf16 --seconds 60
fi

# ---- optional: GROMACS benchMEM (#12) --------------------------------------
if [[ "${GROMACS:-0}" == 1 ]]; then
	if command -v gmx >/dev/null 2>&1; then
		url="${BENCHMEM_URL:-https://www.mpinat.mpg.de/benchMEM}"
		gdir="$work/gromacs"
		mkdir -p "$gdir/dl" "$gdir/run"
		echo "run-all: downloading benchMEM from $url"
		if curl -fsSL -o "$gdir/dl/benchMEM.zip" "$url" &&
			unzip -q -o "$gdir/dl/benchMEM.zip" -d "$gdir/dl"; then
			tpr="$(find "$gdir/dl" -name 'benchMEM.tpr' | head -1)"
			if [[ -n "$tpr" ]]; then
				for prof in hpc triage; do
					(
						cd "$gdir/run"
						run "gromacs-$prof" --profile "$prof" -- \
							gmx mdrun -s "$tpr" -deffnm "bench-$prof" -nsteps 20000 -resethway -noconfout -nb gpu
					)
					cp "$gdir/run/bench-$prof.log" "$p-gromacs-$prof.log.txt" 2>/dev/null || true
					runs+=("gromacs-$prof")
					grep -E '^Performance:' "$p-gromacs-$prof.log.txt" 2>/dev/null || true
				done
				echo "run-all: add the work after the fact, e.g."
				echo "  $TACH annotate --work <ns> --unit ns --price-per-hour <usd> $p-gromacs-hpc.csv.summary.json"
			else
				echo "run-all: benchMEM.tpr not found in the download; GROMACS skipped" >&2
			fi
		else
			echo "run-all: benchMEM download failed ($url); GROMACS skipped" >&2
		fi
	else
		echo "run-all: GROMACS=1 but gmx is not on PATH; GROMACS skipped" >&2
	fi
fi

# ---- optional: MIG 2x / 4x (#14) -------------------------------------------
# Profile names below are the RTX PRO 6000 (96 GB) layout; check them against
# "nvidia-smi mig -lgip" on the instance and override with MIG_2X / MIG_4X.
if [[ "${MIG:-0}" == 1 ]]; then
	mig2="${MIG_2X:-2g.48gb,2g.48gb}"
	mig4="${MIG_4X:-1g.24gb,1g.24gb,1g.24gb,1g.24gb}"
	apply="${MIG_APPLY:-0}"
	# mig_do prints a command, and runs it only when MIG_APPLY=1.
	mig_do() {
		echo "  + $*"
		if [[ "$apply" == 1 ]]; then "$@" || echo "  (exit $?)"; fi
	}
	[[ "$apply" == 1 ]] || echo "run-all: MIG=1 without MIG_APPLY=1: printing the sequence only"
	mig_do as_root nvidia-smi -i 0 -mig 1
	mig_do as_root nvidia-smi --gpu-reset -i 0
	mig_do nvidia-smi mig -lgip
	for layout in "2x $mig2" "4x $mig4"; do
		read -r tag profs <<<"$layout"
		echo "run-all: MIG $tag ($profs)"
		mig_do as_root nvidia-smi mig -i 0 -cgi "$profs" -C
		mig_do nvidia-smi -L
		if [[ "$apply" == 1 ]]; then
			nvidia-smi -L >"$p-mig-$tag-smi.txt" 2>&1 || true
			{
				echo "### dcgmi discovery -c"
				dcgmi discovery -c 2>&1 || echo "exit=$?"
				# Which -i spellings does dmon accept for MIG instances?
				for ent in "i:0" "i:0,i:1" "gi:0" "ci:0" "0"; do
					echo "### dcgmi dmon -i $ent -e 1002,155,156,203 -c 3"
					dcgmi dmon -i "$ent" -e 1002,155,156,203 -c 3 2>&1 || echo "exit=$?"
				done
			} >"$p-mig-$tag-dcgmi.txt"
		fi
		n="$(tr ',' '\n' <<<"$profs" | wc -l | tr -d ' ')"
		# DCGM assigns GPU-I entity IDs; check "dcgmi discovery -c" and set
		# MIG_ENTITIES_2X / MIG_ENTITIES_4X if they are not 0..n-1.
		ents_var="MIG_ENTITIES_${tag^^}"
		ents="${!ents_var:-$(seq -s, 0 $((n - 1)) | sed 's/[0-9]\+/i:&/g')}"
		# One bf16 job per MIG instance, each pinned by its MIG UUID. The
		# single quotes are intended: bash -c expands $u, not this script.
		# shellcheck disable=SC2016
		job='for u in $(nvidia-smi -L | sed -n "s/.*(UUID: \(MIG-[^)]*\)).*/\1/p"); do CUDA_VISIBLE_DEVICES=$u '"$(printf '%q ' "${matmul[@]}")"' --dtype bf16 --size 4096 --seconds 60 & done; wait'
		echo "  + $TACH --csv $p-mig-$tag.csv --label $inst-mig-$tag --entities $ents -- bash -c '<one matmul per MIG UUID>'"
		if [[ "$apply" == 1 && $have_torch == 1 ]]; then
			run "mig-$tag" --entities "$ents" -- bash -c "$job"
		fi
		mig_do as_root nvidia-smi mig -i 0 -dci
		mig_do as_root nvidia-smi mig -i 0 -dgi
	done
	mig_do as_root nvidia-smi -i 0 -mig 0
	mig_do as_root nvidia-smi --gpu-reset -i 0
fi

# ---- scrub -----------------------------------------------------------------
shopt -s nullglob
scrub_all "$p"-*.csv "$p"-*.json "$p"-*.txt
shopt -u nullglob

# ---- 5. summary table ------------------------------------------------------
echo
echo "run-all: summary ($inst)"
summaries=()
for name in "${runs[@]}"; do
	[[ -f "$p-$name.csv.summary.json" ]] && summaries+=("$p-$name.csv.summary.json")
done
if [[ ${#summaries[@]} -eq 0 ]]; then
	echo "  no summaries written"
elif command -v python3 >/dev/null 2>&1; then
	python3 - "${summaries[@]}" <<'PY'
import json, sys
print("%-32s %-44s %12s %-15s %8s  %-30s %s" % (
    "run", "verdict", "energy_j", "energy_method", "samples", "findings", "blanks (field=missing)"))
for path in sys.argv[1:]:
    with open(path) as f:
        s = json.load(f)
    samples = sum(e.get("samples", 0) for e in s.get("entities") or [])
    blanks = {}
    for e in s.get("entities") or []:
        for k, v in (e.get("fields") or {}).items():
            blanks[k] = blanks.get(k, 0) + v.get("missing", 0)
    findings = ",".join(x.get("code", "") for x in s.get("findings") or [])
    print("%-32s %-44s %12.1f %-15s %8d  %-30s %s" % (
        s.get("label") or path, s.get("verdict", ""), s.get("energy_joules", 0.0),
        s.get("energy_method", ""), samples, findings or "-",
        " ".join("%s=%d" % kv for kv in sorted(blanks.items())) or "-"))
PY
elif command -v jq >/dev/null 2>&1; then
	for s in "${summaries[@]}"; do
		jq -r '[(.label // .csv), .verdict, (.energy_joules | tostring), .energy_method,
			([.entities[]?.samples] | add // 0 | tostring),
			([.findings[]?.code] | join(",")),
			([.entities[]?.fields | to_entries[] | "\(.key)=\(.value.missing)"] | join(" "))] | @tsv' "$s"
	done
else
	echo "  (no python3 or jq; see ${summaries[*]})"
fi
echo
echo "run-all: outputs in $out ($inst-*)"
