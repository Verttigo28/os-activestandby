#!/bin/sh
# Sandbox helpers (FreeBSD, run as root). Two cluster nodes (vnet jails hasynA, hasynB) and a client (hasynC) on three
# bridges that play the switches: data (swD), HA link (swH) and management (swM). The jails share the host file system but have
# their own network stack, so the real interfaces of the machine are not visible to them; the daemons run against stub scripts
# and shims (HASYNC_SCRIPTS, HASYNC_PATH), so nothing of the real OPNsense configuration is touched.
SBX="${SBX:-/root/sbx}"
PATHS="/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin:/usr/local/bin"
SHIMPATH="$SBX/shims:$PATHS"
export PATH="$PATHS"

# addressing (the same on every run)
SHARED_LAN_MAC=02:aa:bb:cc:dd:01
SHARED_WAN_MAC=02:aa:bb:cc:dd:02
LAN_IP=192.0.2.1
WAN_IP=198.51.100.1
CLIENT_IP=192.0.2.100
HA_A=10.99.0.1
HA_B=10.99.0.2
MG_A=10.98.0.1
MG_B=10.98.0.2

jn() { echo "hasyn$1"; }
jx() { n=$1; shift; jexec "$(jn "$n")" "$@"; }
nodeenv() { echo "HASYNC_DIR=$SBX/$1/state HASYNC_CONF=$SBX/$1/hasyncd.json HASYNC_CONFIG=$SBX/$1/config.xml HASYNC_SOCK=$SBX/$1/ctl.sock HASYNC_SCRIPTS=$SBX/scripts HASYNC_PATH=$SHIMPATH SBXNODE=$SBX/$1"; }
# shellcheck disable=SC2046
ctl() { n=$1; shift; jexec "$(jn "$n")" env $(nodeenv "$n") "$SBX/hasyncd" ctl "$@"; }

now() { php -r 'echo number_format(microtime(true), 3, ".", "");'; }
# jf <dotted.path>: read a field from JSON on stdin
jf() { php -r '$j = json_decode(stream_get_contents(STDIN), true); foreach (explode(".", $argv[1]) as $k) { $j = is_array($j) ? ($j[$k] ?? null) : null; } echo is_bool($j) ? ($j ? "true" : "false") : (is_array($j) ? json_encode($j) : (string)$j);' "$1"; }
st() { ctl "$1" status | jf "$2"; } # st A state

start_daemon() {
	n=$1
	# shellcheck disable=SC2046
	daemon -f -o "$SBX/$n/daemon.log" -p "$SBX/$n/daemon.pid" jexec "$(jn "$n")" env $(nodeenv "$n") "$SBX/hasyncd" daemon </dev/null >/dev/null 2>&1
}
stop_daemon() { # kill -9 style: no goodbye
	jx "$1" pkill -9 -f "hasyncd daemon" 2>/dev/null
	rm -f "$SBX/$1/ctl.sock"
}
daemon_up() { [ -S "$SBX/$1/ctl.sock" ]; }

# does node $1 hold the shared identity on tst0 (shared MAC and the LAN address)?
owns() {
	o=$(jx "$1" ifconfig tst0 2>/dev/null)
	echo "$o" | grep -qi "ether $SHARED_LAN_MAC" && echo "$o" | grep -q "inet $LAN_IP "
}
mac_of() { jx "$1" ifconfig "$2" 2>/dev/null | awk '$1=="ether"{print $2; exit}'; }
has_ip() { jx "$1" ifconfig "$2" 2>/dev/null | grep -q "inet $3 "; }

wait_for() { # <seconds> <description> <shell condition>
	t=$1; d=$2; shift 2
	end=$(($(date +%s) + t))
	while [ "$(date +%s)" -lt "$end" ]; do
		if eval "$*" >/dev/null 2>&1; then return 0; fi
		sleep 0.2
	done
	echo "   (timeout after ${t}s waiting for: $d)"
	return 1
}

PASS=0
FAIL=0
check() { # <name> <shell condition>
	name=$1; shift
	if eval "$*" >/dev/null 2>&1; then
		PASS=$((PASS + 1)); echo "PASS  $name"
	else
		FAIL=$((FAIL + 1)); echo "FAIL  $name"
	fi
}
info() { echo "      $*"; }
section() { echo; echo "=== $*"; }
summary() { echo; echo "RESULT: $PASS passed, $FAIL failed"; [ "$FAIL" -eq 0 ]; }

# --- the double-active detector: samples both nodes every 100 ms; two consecutive samples with the shared identity on
# both nodes at once are a violation (a hand-over never overlaps, sampling skew is a few ms)
monitor_start() {
	rm -f "$SBX/monitor.log" "$SBX/monitor.stop"
	(
		prev=0
		while [ ! -f "$SBX/monitor.stop" ]; do
			a=0; b=0
			owns A && a=1
			owns B && b=1
			t=$(date +%s)
			echo "$t $a $b" >> "$SBX/monitor.log"
			sleep 0.1
		done
	) &
	echo $! > "$SBX/monitor.pid"
}
monitor_stop() { touch "$SBX/monitor.stop"; sleep 0.3; kill "$(cat "$SBX/monitor.pid" 2>/dev/null)" 2>/dev/null; }
# violations: consecutive samples where both own the identity
monitor_violations() {
	awk 'BEGIN{p=0;v=0} { if ($2==1 && $3==1) { p++; if (p==2) v++ } else p=0 } END{print v+0}' "$SBX/monitor.log"
}
monitor_summary() { awk '{n++; if($2==1&&$3==1)d++; if($2==0&&$3==0)z++} END{printf "samples=%d both_active=%d nobody_active=%d (x100ms)\n", n, d+0, z+0}' "$SBX/monitor.log"; }

# --- client traffic: ping the shared address from the client jail and report lost packets and the longest outage
# usage: ping_run <seconds>  -> runs in the foreground, prints "lost=<n> longest_outage_ms=<ms>"
ping_run() {
	n=$(($1 * 50))
	jx C ping -n -i 0.02 -c "$n" -W 100 $LAN_IP 2>/dev/null | awk -v iv=20 '
		/icmp_seq=/ { for (i=1;i<=NF;i++) if ($i ~ /^icmp_seq=/) { split($i,a,"="); s=a[2]+0 }
			if (seen && s > last+1) { g=s-last-1; lost+=g; if (g>max) max=g }
			last=s; seen=1 }
		END { printf "lost=%d longest_outage_ms=%d\n", lost, max*iv }'
}

# --- more helpers for the scenarios
other() { [ "$1" = A ] && echo B || echo A; }
net() { . "$SBX/net.env"; } # LA LB LC HA HB MA MB WA WB and the bridges
bridge_port_of_mac() { # which bridge port the data switch learned <mac> on
	. "$SBX/net.env"
	ifconfig "$SWD" addr | awk -v m="$1" 'tolower($1)==m {print $3}'
}
tx_packets() { jx "$1" netstat -I "$2" -b -n 2>/dev/null | awk 'NR==2{print $(NF-3)}'; }
events() { ctl "$1" events; }
ensure_active() { # make node $1 the active one (graceful), whatever the state is
	[ "$(st "$1" state)" = active ] && return 0
	ctl "$1" failover >/dev/null 2>&1
	wait_for 30 "$1 active" '[ "$(st '"$1"' state)" = active ]'
}
monitor_clear() { : > "$SBX/monitor.log"; }
# power cut: no goodbye, every interface of the node goes down and its daemon dies
power_off() {
	stop_daemon "$1"
	for i in tst0 tst1 tsh0 tsm0; do jx "$1" ifconfig $i down; done
}
# boot after a power cut: interfaces come back with their own MACs and no data address (the early boot hook of the plugin
# runs "boot-standby" before anything else), then the daemon starts
power_on() {
	n=$1
	for i in tst0 tst1; do
		hw=$(jx "$n" ifconfig $i | awk '$1=="hwaddr"{print $2; exit}')
		[ -n "$hw" ] && jx "$n" ifconfig $i link "$hw"
		for a in $(jx "$n" ifconfig $i | awk '$1=="inet"{print $2}'); do jx "$n" ifconfig $i inet "$a" -alias; done
		jx "$n" ifconfig $i up
	done
	jx "$n" ifconfig tsh0 up; jx "$n" ifconfig tsm0 up
	# shellcheck disable=SC2046
	jexec "$(jn "$n")" env $(nodeenv "$n") "$SBX/hasyncd" boot-standby
	start_daemon "$n"
	wait_for 20 "daemon $n" "daemon_up $n"
}
