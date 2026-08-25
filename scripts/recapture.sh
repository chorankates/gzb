#!/usr/bin/env bash
#
# Re-captures the console transcripts quoted in README.md, against a real
# device, and prints them as fenced blocks ready to paste back in.
#
# The transcripts in the README are the only part of it that can quietly stop
# being true: the golden tests in cmd/gzb pin how a line is rendered, but not
# that a device ever said it. This is the missing half.
#
# Usage:
#   scripts/recapture.sh "living room thermo"              # read-only captures
#   scripts/recapture.sh "living room thermo" --configure  # also the write path
#
# Everything printed is also written to $OUT (recapture.md by default), so the
# blocks can be read back from disk rather than copied out of a terminal.
#
# Without --configure this touches nothing: a read, a write the device is
# expected to refuse, and a question about its reporting configuration. With
# --configure it also applies the example reporting configuration, captures it,
# and puts back exactly what was there before.
set -uo pipefail

DEVICE=${1:-}
MODE=${2:-}
GZB=${GZB:-./gzb}
OUT=${OUT:-recapture.md}
CLUSTER=temperature
ATTR=temperature

if [ -z "$DEVICE" ]; then
	echo "usage: $0 <device> [--configure]" >&2
	echo "  <device> is a name, an IEEE address, or a 0xNNNN network address" >&2
	exit 2
fi
if [ ! -x "$GZB" ]; then
	echo "$GZB is not built; run make build" >&2
	exit 2
fi

: >"$OUT"

# emit writes a line to the terminal and to the transcript file at once, so a
# run that takes minutes can still be watched while it happens.
emit() {
	printf '%s\n' "$*" | tee -a "$OUT"
}

# quoted renders one argument the way it would have to be typed to survive a
# shell. Device names contain spaces, and at least one contains a '#', which
# without quotes starts a comment and silently truncates the command — so a
# transcript that is not quoted is a transcript nobody can paste.
quoted() {
	local arg
	for arg in "$@"; do
		case $arg in
		*[!A-Za-z0-9_.:/-]*) printf '"%s" ' "$arg" ;;
		*) printf '%s ' "$arg" ;;
		esac
	done
}

# capture runs one command and writes it as a console block. A battery device
# is only reachable while it happens to be awake, so a failure here is normal
# and worth saying plainly rather than aborting the whole run.
capture() {
	local label=$1
	shift
	emit "### $label"
	emit '```console'
	local line
	line=$(quoted "$@")
	emit "\$ gzb ${line% }"
	"$GZB" "$@" 2>&1 | tee -a "$OUT"
	emit '```'
	emit ""
}

capture "read" read "$DEVICE" "$CLUSTER"

# Writing a measured value is refused by the device, which is the point: it
# exercises the whole write path and changes nothing.
capture "write (expected to be refused)" write "$DEVICE" "$CLUSTER" "$ATTR" 2500

capture "reporting --show" reporting -show "$DEVICE" "$CLUSTER" "$ATTR"

if [ "$MODE" != "--configure" ]; then
	emit "### reporting --configure: skipped"
	emit "Pass --configure to capture it. That one writes to the device."
	emit ""
	emit "written to $OUT"
	exit 0
fi

# Everything below changes the device, so it starts by finding out what to put
# back. Reading the configuration first is not politeness, it is the only way
# the restore can be to a measured state rather than a guess.
emit "### recording the existing configuration"
BEFORE=$("$GZB" -json reporting -show "$DEVICE" "$CLUSTER" "$ATTR" 2>&1)
if ! echo "$BEFORE" | python3 -c 'import json,sys; json.load(sys.stdin)' >/dev/null 2>&1; then
	echo "could not read the current configuration, so it could not be restored:" >&2
	echo "$BEFORE" >&2
	exit 1
fi

RESTORE=$(echo "$BEFORE" | python3 -c '
import json, sys

held = json.load(sys.stdin)
if not held:
    sys.exit("the device answered about no attributes")
first = held[0]

# A device that is not reporting has no configuration to describe, and "off"
# is not the same state as "never configured". Refusing here is the whole
# lesson of this script: do not change something you cannot put back.
if not first.get("reporting"):
    sys.exit("this attribute is not being reported, so there is no prior state to restore")

args = [
    "-min", "%ds" % (first["min"] // 1_000_000_000),
    "-max", "%ds" % (first["max"] // 1_000_000_000),
]
if first.get("change") is not None:
    args += ["-change", str(first["change"])]
if first.get("type"):
    args += ["-type", first["type"]]
print(" ".join(args))
')
if [ $? -ne 0 ]; then
	echo "not configuring: $RESTORE" >&2
	exit 1
fi
emit "will restore with: $RESTORE"
emit ""

capture "reporting" reporting -min 60s -max 1h -change 50 "$DEVICE" "$CLUSTER" "$ATTR"

emit "### restoring"
# shellcheck disable=SC2086
# This loop depends on pipefail: without it the pipeline reports tee's status,
# which is always success, and the restore would stop after one attempt whether
# or not it reached the device.
until "$GZB" reporting $RESTORE "$DEVICE" "$CLUSTER" "$ATTR" | tee -a "$OUT"; do
	emit "restore did not get through; the device is asleep. Trying again."
done
emit ""

# Saying the write succeeded is not the same as showing what the device now
# holds, and only the second one is evidence.
capture "verifying the restore" reporting -show "$DEVICE" "$CLUSTER" "$ATTR"

emit "written to $OUT"
