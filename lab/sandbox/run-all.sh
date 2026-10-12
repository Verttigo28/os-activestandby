#!/bin/sh
# Run the whole battery in both modes, one log per mode:  sh run-all.sh [logdir]     (about 12 minutes per mode)
HERE="$(cd "$(dirname "$0")" && pwd)"
LOGDIR="${1:-/root}"
for m in classic warm; do
	sh "$HERE/scenarios.sh" $m > "$LOGDIR/scn-$m.log" 2>&1 < /dev/null
done
