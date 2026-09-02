#!/usr/bin/env bash
#
# Re-captures the console transcripts quoted in README.md's "Operating a light"
# section, against a real light, and prints them as fenced blocks ready to
# paste back in.
#
# This is the light half of scripts/recapture.sh, and exists separately because
# a light is not a sensor: the interesting commands are cluster commands rather
# than attribute reads, and the state that has to be put back afterwards is a
# colour and a brightness rather than a reporting configuration.
#
# Usage:
#   scripts/recapture-light.sh light1              # read-only captures
#   scripts/recapture-light.sh light1 --configure  # also the commands that change it
#
# Everything printed is also written to $OUT (recapture-light.md by default).
#
# Nothing here turns a light on. Every command used sets colour or level, both
# of which leave the on/off state alone, so this can be run in daylight against
# a light that is meant to stay dark.
set -uo pipefail

LIGHT=${1:-}
MODE=${2:-}
GZB=${GZB:-./gzb}
OUT=${OUT:-recapture-light.md}

if [ -z "$LIGHT" ]; then
	echo "usage: $0 <light> [--configure]" >&2
	echo "  <light> is a name, an IEEE address, or a 0xNNNN network address" >&2
	exit 2
fi
if [ ! -x "$GZB" ]; then
	echo "$GZB is not built; run make build" >&2
	exit 2
fi

: >"$OUT"

emit() {
	printf '%s\n' "$*" | tee -a "$OUT"
}

# quoted renders one argument the way it would have to be typed to survive a
# shell, for the same reason the sensor script does: a light called
# "hallway lamp" is not pasteable unquoted.
quoted() {
	local arg
	for arg in "$@"; do
		case $arg in
		*[!A-Za-z0-9_.:/%#-]*) printf '"%s" ' "$arg" ;;
		*) printf '%s ' "$arg" ;;
		esac
	done
}

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

# attr reads one attribute and prints its raw value, for recording state that
# has to be put back. It prints nothing if the device did not answer, which the
# caller has to treat as a failure rather than as a zero.
attr() {
	local cluster=$1 id=$2
	"$GZB" -json read "$LIGHT" "$cluster" "$id" 2>/dev/null |
		python3 -c '
import json, sys
try:
    values = json.load(sys.stdin)
except Exception:
    sys.exit(1)
for v in values:
    if v.get("status"):
        sys.exit(1)
    print(v["value"])
    break
'
}

# A light refuses a write to its colour, which is the whole reason cluster
# commands exist. It changes nothing, so it is a read-only capture.
capture "a colour is not a writable attribute" write "$LIGHT" color hue 0

capture "what the light can be told" read "$LIGHT" color 0x400A
capture "colour and brightness now" read "$LIGHT" color 0x0000 0x0001

if [ "$MODE" != "--configure" ]; then
	emit "### the commands that change it: skipped"
	emit "Pass --configure to capture them. Those write to the device."
	emit ""
	emit "written to $OUT"
	exit 0
fi

# Everything below changes the light, so it starts by finding out what to put
# back. As in the sensor script, reading first is not politeness: it is what
# makes the restore a measured state rather than a guess.
emit "### recording the existing state"
HUE=$(attr color 0x0000)
SAT=$(attr color 0x0001)
LEVEL=$(attr level 0x0000)
ONLEVEL=$(attr level 0x0011)
if [ -z "$HUE" ] || [ -z "$SAT" ] || [ -z "$LEVEL" ] || [ -z "$ONLEVEL" ]; then
	echo "could not read the current colour, level and on level, so they could not be restored" >&2
	exit 1
fi
RESTORE="hue:$HUE/$SAT level:$LEVEL"
emit "will restore with: $RESTORE, and on level $ONLEVEL"
emit ""

capture "telling a light what to be" light "$LIGHT" red dim

# --persist writes OnLevel, which is the one that survives something else
# switching the light on. It is captured separately because it is a different
# fact from the level the lamp is at now.
capture "a brightness that survives being switched on" light -persist "$LIGHT" red dim

capture "it never came on" read "$LIGHT" on/off 0x0000

emit "### restoring"
# shellcheck disable=SC2086
# Depends on pipefail: without it the pipeline reports tee's status, which is
# always success, and the restore would stop after one attempt whether or not
# it reached the light.
until "$GZB" light "$LIGHT" $RESTORE | tee -a "$OUT"; do
	emit "restore did not get through; trying again."
done
until "$GZB" write "$LIGHT" level "on level" "$ONLEVEL" | tee -a "$OUT"; do
	emit "on level restore did not get through; trying again."
done
emit ""

# Saying the restore succeeded is not the same as showing what the light now
# holds, and only the second one is evidence.
capture "verifying the restore" read "$LIGHT" color 0x0000 0x0001
capture "verifying the restore" read "$LIGHT" level 0x0000 0x0011

emit "written to $OUT"
