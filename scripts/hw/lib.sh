#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Shared helpers for scripts/hw (sourced by capture.sh and run-all.sh).
set -euo pipefail

# instance_type prints the EC2 instance type from IMDSv2, or "unknown".
instance_type() {
	local tok it
	tok="$(curl -sf --max-time 2 -X PUT http://169.254.169.254/latest/api/token \
		-H 'X-aws-ec2-metadata-token-ttl-seconds: 60' 2>/dev/null || true)"
	if [[ -n "$tok" ]]; then
		it="$(curl -sf --max-time 2 -H "X-aws-ec2-metadata-token: $tok" \
			http://169.254.169.254/latest/meta-data/instance-type 2>/dev/null || true)"
	fi
	if [[ -n "${it:-}" && "$it" =~ ^[a-z0-9.-]+$ ]]; then
		printf '%s\n' "$it"
	else
		printf 'unknown\n'
	fi
}

# as_root runs a command as root (directly when already root, else via sudo).
as_root() {
	if [[ "$(id -u)" -eq 0 ]]; then
		"$@"
	else
		sudo "$@"
	fi
}

# ensure_hostengine starts nv-hostengine as root if it is not running.
ensure_hostengine() {
	if pgrep -x nv-hostengine >/dev/null 2>&1; then
		echo "nv-hostengine: running (pid $(pgrep -x nv-hostengine | tr '\n' ' '))"
		return 0
	fi
	echo "nv-hostengine: not running; starting as root"
	if systemctl list-unit-files nvidia-dcgm.service >/dev/null 2>&1 &&
		systemctl list-unit-files nvidia-dcgm.service | grep -q nvidia-dcgm; then
		as_root systemctl start nvidia-dcgm || as_root nv-hostengine
	else
		as_root nv-hostengine
	fi
	sleep 2
	pgrep -x nv-hostengine >/dev/null 2>&1 || {
		echo "nv-hostengine: failed to start" >&2
		return 1
	}
}

# supported_fields <comma-list> probes each DCGM field on its own with
# "$DCGMI" dmon -e <id> -c 1 and echoes the comma-joined list of fields that
# produced a row (a line starting "GPU", whatever its value, N/A included).
# The dropped fields go to stderr. One unsupported field makes DCGM reject
# the whole dmon ("Error setting watches. Result: -6: Feature not supported",
# fp64_active 1006 on an L40S), so a capture must not request it.
supported_fields() {
	local id probe keep="" drop=""
	local -a ids
	IFS=, read -ra ids <<<"$1"
	for id in "${ids[@]}"; do
		[[ -n "$id" ]] || continue
		probe="$("${DCGMI:-dcgmi}" dmon -e "$id" -c 1 2>&1 || true)"
		if grep -q '^GPU' <<<"$probe"; then
			keep="${keep:+$keep,}$id"
		else
			drop="${drop:+$drop,}$id"
		fi
	done
	if [[ -n "$drop" ]]; then
		echo "supported_fields: dropped $drop (no GPU row from dmon -e <id> -c 1)" >&2
	fi
	printf '%s\n' "$keep"
}

# timestamp prefixes each input line with its arrival time (Unix epoch, ns).
timestamp() {
	local l
	while IFS= read -r l; do
		printf '%s %s\n' "$(date +%s.%N)" "$l"
	done
}

# scrub_file replaces AWS account IDs with <acct> in place, in the contexts
# where they appear: ARNs, ECR registry hosts and "account" keys. It does not
# touch other 12-digit numbers: a blanket match corrupted measurements (PCIe
# rates in B/s, long float fractions in summary JSON) on the first g7e run.
# Remaining standalone 12-digit runs are reported for a manual check.
scrub_file() {
	local f="$1" tmp
	[[ -f "$f" ]] || return 0
	tmp="$(mktemp)"
	sed -E \
		-e 's/(arn:aws[a-z-]*:[a-z0-9-]*:[a-z0-9-]*:)[0-9]{12}/\1<acct>/g' \
		-e 's/(^|[^0-9])[0-9]{12}(\.dkr\.ecr\.)/\1<acct>\2/g' \
		-e 's/([Aa]ccount[_ -]?[Ii]d"?[[:space:]]*[:=][[:space:]]*"?)[0-9]{12}/\1<acct>/g' \
		"$f" >"$tmp"
	cat "$tmp" >"$f"
	rm -f "$tmp"
	if grep -qE '(^|[^0-9.])[0-9]{12}([^0-9.]|$)' "$f"; then
		echo "scrub: check $f by hand: it has a standalone 12-digit number" >&2
	fi
}

# scrub_all scrubs every file matching the given paths.
scrub_all() {
	local f
	for f in "$@"; do
		scrub_file "$f"
	done
}
