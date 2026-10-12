#!/bin/sh
# Tear the sandbox down: stop the daemons, destroy the jails' interfaces (an epair disappears with both ends), the bridges and
# the jails. Logs and state stay under $SBX until "sh down.sh clean". Safe to run when nothing is up.
. "$(dirname "$0")/lib.sh"
quiet=0; clean=0
for a in "$@"; do case "$a" in quiet) quiet=1 ;; clean) clean=1 ;; esac; done
say() { [ $quiet -eq 1 ] || echo "$*"; }
[ -f "$SBX/monitor.pid" ] && { touch "$SBX/monitor.stop"; kill "$(cat "$SBX/monitor.pid")" 2>/dev/null; rm -f "$SBX/monitor.pid"; }
for n in A B C; do
	if jls -j "$(jn $n)" >/dev/null 2>&1; then
		jx $n pkill -9 -f "hasyncd daemon" 2>/dev/null
		# destroy from inside: a renamed interface would otherwise come back to the host under its new name
		for i in $(jx $n ifconfig -l); do
			case "$i" in lo0 | enc0 | pflog0 | pfsync0) ;; *) jx $n ifconfig "$i" destroy 2>/dev/null ;; esac
		done
		jx $n pfctl -d >/dev/null 2>&1
		jail -r "$(jn $n)" 2>/dev/null && say "jail $(jn $n) removed"
	fi
done
if [ -f "$SBX/net.env" ]; then
	# shellcheck disable=SC1090
	. "$SBX/net.env"
	for b in $SWD $SWH $SWM; do ifconfig "$b" destroy 2>/dev/null && say "bridge $b destroyed"; done
	for e in $(ifconfig -l); do case "$e" in epair*) ifconfig "$e" destroy 2>/dev/null ;; esac; done
	rm -f "$SBX/net.env"
fi
for n in A B; do chflags noschg "$SBX/$n/config.xml" 2>/dev/null; rm -f "$SBX/$n/ctl.sock" "$SBX/$n/daemon.pid"; done
[ -f "$SBX/pfil.save" ] && { sysctl net.link.bridge.pfil_member="$(cat "$SBX/pfil.save")" >/dev/null; rm -f "$SBX/pfil.save"; }
[ -f "$SBX/marker-created" ] && rm -f /var/run/activestandby.state "$SBX/marker-created"
if [ $clean -eq 1 ]; then
	for n in A B C; do chflags -R noschg "$SBX/$n" 2>/dev/null; done
	rm -rf "$SBX/A" "$SBX/B" "$SBX/C" "$SBX/scripts" "$SBX/shims" "$SBX/monitor."* "$SBX/mode"
	say "state removed"
fi
exit 0
