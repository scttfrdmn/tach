#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# capture.sh <outdir>: raw dcgmi captures for the dmon format, pipe buffering
# and SIGINT checks (#7) and the multiplexing blank rate (#13).
#
# Writes <outdir>/<instance>-*.txt, <instance> being the EC2 instance type
# from IMDSv2 (or "unknown"):
#   -identity.txt      dcgmi --version, nvidia-smi -q head, dcgmi discovery -l, profile groups,
#                      requested, supported and dropped dmon fields
#   -dmon.txt          dmon -c 5 straight to a file with every requested field (an
#                      unsupported one shows as DCGM's error), then the supported
#                      fields piped through cat (as tach reads it)
#   -dmon-pipe-ts.txt  dmon -c 10 piped through a line timestamper (arrival time per line)
#   -dmon-stdbuf-ts.txt / -dmon-pty-ts.txt  same via stdbuf -oL and a pty, if available
#   -dmon-sigint.txt   streaming dmon, SIGINT after 3.5 s: rows after the signal, exit status
#   -dmon-blank-1s.txt / -dmon-blank-200ms.txt  triage fields 1002-1006, N/A counts per column
#   -dmon-pcie.txt     dmon -e 1009,1010 -c 3
#   -dmon-help.txt     dcgmi dmon --help (entity -i syntax) and dmon -l (field list)
#
# Env: LOAD_CMD  a shell command run in the background during the blank-rate
#                captures (e.g. "python3 matmul.py --dtype bf16 --seconds 90").
#      DCGMI     dcgmi binary (default dcgmi).
# Every step after the first -dmon.txt capture requests only the fields this
# GPU supports (supported_fields in lib.sh): DCGM rejects a dmon with any
# unsupported field, e.g. fp64_active (1006) on an L40S, and records nothing.
# All outputs are scrubbed of 12-digit AWS account numbers.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh source-path=SCRIPTDIR
source "$here/lib.sh"

[[ $# -eq 1 ]] || {
	echo "usage: $0 <outdir>" >&2
	exit 2
}
out="$1"
mkdir -p "$out"
DCGMI="${DCGMI:-dcgmi}"
inst="${INSTANCE:-$(instance_type)}"
p="$out/$inst"
req_fields="1002,1003,1004,1005,1006,250,252,155,156"
req_triage="1002,1003,1004,1005,1006"
work="$(mktemp -d)"
load_pid=""
cleanup() {
	if [[ -n "$load_pid" ]]; then kill "$load_pid" 2>/dev/null || true; fi
	rm -rf "$work"
}
trap cleanup EXIT

echo "capture: instance=$inst outdir=$out"
ensure_hostengine

# Probe once; every step after the first -dmon.txt capture uses these.
fields="$(supported_fields "$req_fields")"
triage="$(supported_fields "$req_triage")"
[[ -n "$fields" ]] || echo "capture: no requested field is supported; the dmon steps will fail" >&2

# dropped_from <requested> <supported> echoes the requested fields missing
# from the supported list, comma-joined, or "none".
dropped_from() {
	local id out=""
	local -a ids
	IFS=, read -ra ids <<<"$1"
	for id in "${ids[@]}"; do
		[[ ",$2," == *",$id,"* ]] || out="${out:+$out,}$id"
	done
	printf '%s\n' "${out:-none}"
}

# section prints a header naming the command that follows.
section() { printf '\n### %s\n### at %s\n' "$*" "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)"; }

# sigdfl runs a command with SIGINT reset to the default disposition. Without
# it, a background job of a non-interactive shell inherits SIGINT as ignored
# and the SIGINT test would measure bash, not dcgmi. It execs, so when run as
# `sigdfl ... &` the job's pid ($!) is the command's own pid: without the exec
# $! was the subshell, which deferred the SIGINT, and dmon streamed forever.
sigdfl() {
	exec python3 -c 'import os, signal, sys
signal.signal(signal.SIGINT, signal.SIG_DFL)
os.execvp(sys.argv[1], sys.argv[1:])' "$@"
}

# na_counts summarises N/A per column of dmon output on stdin. It assumes the
# documented layout (a "#Entity" header line of field names, rows starting "GPU <n>" or
# "GPU-I <n>"); the raw capture above it is the ground truth.
na_counts() {
	awk '
		/^# ?Entity/ {
			n = 0
			for (i = 1; i <= NF; i++)
				if ($i !~ /^(#|#Entity|Entity|ID)$/) name[++n] = $i
			next
		}
		/^(GPU|GPU-I|GPU-CI) +[0-9]+/ {
			rows++
			sub(/^(GPU|GPU-I|GPU-CI) +[0-9]+ +/, "")
			for (i = 1; i <= NF; i++) if ($i == "N/A") na[i]++
			if (NF > maxnf) maxnf = NF
		}
		END {
			printf "rows=%d\n", rows
			for (i = 1; i <= maxnf; i++)
				printf "%s N/A=%d (%.1f%%)\n", (i in name ? name[i] : "col" i), na[i], rows ? 100 * na[i] / rows : 0
		}'
}

# ---- identity -------------------------------------------------------------
{
	section "dcgmi --version"
	"$DCGMI" --version 2>&1 || echo "exit=$?"
	section "nvidia-smi -q (head)"
	nvidia-smi -q 2>&1 | head -45 || true
	section "nvidia-smi -L"
	nvidia-smi -L 2>&1 || true
	section "dcgmi discovery -l"
	"$DCGMI" discovery -l 2>&1 || echo "exit=$?"
	section "dcgmi profile -l -i 0 (profiling metric groups)"
	"$DCGMI" profile -l -i 0 2>&1 || echo "exit=$?"
	section "uname -a; /etc/os-release"
	uname -a
	grep -E '^(NAME|VERSION)=' /etc/os-release || true
	section "dmon fields (each probed with: $DCGMI dmon -e <id> -c 1; kept if it gives a GPU row)"
	echo "The first -dmon.txt capture requests every field; the other dmon steps"
	echo "request only the supported ones, so a dropped field has no column there."
	echo "fields requested: $req_fields"
	echo "fields supported: ${fields:-none}"
	echo "fields dropped:   $(dropped_from "$req_fields" "$fields")"
	echo "triage requested: $req_triage"
	echo "triage supported: ${triage:-none}"
	echo "triage dropped:   $(dropped_from "$req_triage" "$triage")"
} >"$p-identity.txt"

# ---- dmon format: direct to file, then piped ------------------------------
{
	section "dcgmi --version"
	"$DCGMI" --version 2>&1 || echo "exit=$?"
	section "dcgmi dmon -e $req_fields -d 1000 -c 5   (stdout is a file)"
} >"$p-dmon.txt"
rc=0
"$DCGMI" dmon -e "$req_fields" -d 1000 -c 5 >>"$p-dmon.txt" 2>&1 || rc=$?
echo "exit=$rc" >>"$p-dmon.txt"
{
	section "dcgmi dmon -e $fields -d 1000 -c 5 | cat   (stdout is a pipe; supported fields)"
	rc=0
	"$DCGMI" dmon -e "$fields" -d 1000 -c 5 2>&1 | cat || rc=$?
	echo "exit=$rc"
} >>"$p-dmon.txt"

# ---- pipe buffering: per-line arrival times -------------------------------
# One row per entity should arrive about once a second. Rows arriving
# together at the end mean dmon block-buffers when stdout is a pipe.
{
	section "dcgmi dmon -e $fields -d 1000 -c 10 | timestamper   (column 1: arrival, Unix s)"
	echo "start $(date +%s.%N)"
	rc=0
	"$DCGMI" dmon -e "$fields" -d 1000 -c 10 2>&1 | timestamp || rc=$?
	echo "end $(date +%s.%N) exit=$rc"
} >"$p-dmon-pipe-ts.txt"

if command -v stdbuf >/dev/null 2>&1; then
	{
		section "stdbuf -oL dcgmi dmon -e $fields -d 1000 -c 5 | timestamper"
		echo "start $(date +%s.%N)"
		rc=0
		stdbuf -oL "$DCGMI" dmon -e "$fields" -d 1000 -c 5 2>&1 | timestamp || rc=$?
		echo "end $(date +%s.%N) exit=$rc"
	} >"$p-dmon-stdbuf-ts.txt"
fi
if command -v script >/dev/null 2>&1; then
	{
		section "script (pty): dcgmi dmon -e $fields -d 1000 -c 5 | timestamper"
		echo "start $(date +%s.%N)"
		rc=0
		script -qfec "$DCGMI dmon -e $fields -d 1000 -c 5" /dev/null 2>&1 | tr -d '\r' | timestamp || rc=$?
		echo "end $(date +%s.%N) exit=$rc"
	} >"$p-dmon-pty-ts.txt"
fi

# ---- SIGINT: do the last rows arrive, and how does dmon exit? -------------
fifo="$work/dmon.fifo"
mkfifo "$fifo"
raw="$work/sigint.raw"
timestamp <"$fifo" >"$raw" &
ts_pid=$!
t_start="$(date +%s.%N)"
sigdfl "$DCGMI" dmon -e "$fields" -d 1000 >"$fifo" 2>&1 &
dmon_pid=$!
sleep 3.5
t_kill="$(date +%s.%N)"
kill -INT "$dmon_pid" 2>/dev/null || true
escalated="no"
for _ in $(seq 1 100); do
	kill -0 "$dmon_pid" 2>/dev/null || break
	sleep 0.1
done
if kill -0 "$dmon_pid" 2>/dev/null; then
	escalated="yes (still running 10 s after SIGINT; sent SIGTERM)"
	kill -TERM "$dmon_pid" 2>/dev/null || true
	sleep 2
	kill -KILL "$dmon_pid" 2>/dev/null || true
fi
dmon_rc=0
wait "$dmon_pid" || dmon_rc=$?
t_exit="$(date +%s.%N)"
wait "$ts_pid" || true
{
	section "SIGINT test: dcgmi dmon -e $fields -d 1000 (streaming) | timestamper; kill -INT after 3.5 s"
	echo "start=$t_start sigint=$t_kill exited=$t_exit"
	echo "exit_status=$dmon_rc (130 = killed by SIGINT without a handler; 0 = clean exit)"
	echo "escalated=$escalated"
	awk -v k="$t_kill" '
		$2 ~ /^GPU/ { if ($1 + 0 <= k + 0) before++; else after++ }
		END { printf "data_rows_before_sigint=%d data_rows_after_sigint=%d\n", before, after }' "$raw"
	echo "--- raw (column 1: arrival, Unix s)"
	cat "$raw"
} >"$p-dmon-sigint.txt"

# ---- blank rate of the triage fields at 1 s and 200 ms (#13) --------------
load_note="no load (LOAD_CMD unset)"
if [[ -n "${LOAD_CMD:-}" ]]; then
	load_note="load: $LOAD_CMD"
	bash -c "$LOAD_CMD" >"$work/load.log" 2>&1 &
	load_pid=$!
	sleep 15 # let the load reach steady state
fi
for spec in "1000 30 1s" "200 150 200ms"; do
	read -r d c tag <<<"$spec"
	f="$p-dmon-blank-$tag.txt"
	{
		section "dcgmi dmon -e $triage -d $d -c $c   ($load_note)"
		rc=0
		"$DCGMI" dmon -e "$triage" -d "$d" -c "$c" >"$work/blank.raw" 2>&1 || rc=$?
		cat "$work/blank.raw"
		echo "exit=$rc"
		section "N/A per column"
		na_counts <"$work/blank.raw"
	} >"$f"
done
if [[ -n "$load_pid" ]]; then
	pkill -P "$load_pid" 2>/dev/null || true
	kill "$load_pid" 2>/dev/null || true
	wait "$load_pid" 2>/dev/null || true
	load_pid=""
	{
		section "load output"
		cat "$work/load.log"
	} >>"$p-dmon-blank-1s.txt"
fi

# ---- PCIe fields ----------------------------------------------------------
{
	section "dcgmi dmon -e 1009,1010 -c 3"
	"$DCGMI" dmon -e 1009,1010 -c 3 2>&1 || echo "exit=$?"
} >"$p-dmon-pcie.txt"

# ---- help: entity (-i) syntax and field list -------------------------------
{
	section "dcgmi dmon --help"
	"$DCGMI" dmon --help 2>&1 || echo "exit=$?"
	section "dcgmi dmon -l"
	"$DCGMI" dmon -l 2>&1 || echo "exit=$?"
} >"$p-dmon-help.txt"

scrub_all "$p"-identity.txt "$p"-dmon*.txt
echo "capture: done"
ls -l "$p"-identity.txt "$p"-dmon*.txt
