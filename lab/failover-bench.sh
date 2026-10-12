#!/bin/sh
# Measure the traffic interruption of a failover from a LAN client (Linux iputils ping, run as root for -i < 0.2).
#   sh failover-bench.sh run    <target-ip> <seconds> [out.csv]   # one run: start it, trigger the failover meanwhile
#   sh failover-bench.sh summary <out.csv>                        # median / P95 / max over all recorded runs
# The target must be reachable THROUGH the cluster (e.g. an address behind the WAN). The interruption is the longest run
# of lost echo requests times the interval: a measurement with a 20 ms resolution, not a guarantee.
# After each run, collect the node-side timeline with `hasyncd ctl history` on the new active node (phases, detect_ms).
INTERVAL=0.02

gaps() { # stdin: ping output; prints "<lost> <longest_gap_ms>"
	awk -v iv="$INTERVAL" '
		/icmp_seq=/ { for (i = 1; i <= NF; i++) if ($i ~ /^icmp_seq=/) { split($i, a, "="); s = a[2] + 0 }
			if (seen && s > last + 1) { g = s - last - 1; lost += g; if (g > max) max = g }
			last = s; seen = 1 }
		END { printf "%d %d\n", lost, max * iv * 1000 }'
}

case "$1" in
run)
	[ -n "$2" ] && [ -n "$3" ] || { echo "usage: $0 run <target> <seconds> [csv]"; exit 2; }
	n=$(awk -v s="$3" -v iv="$INTERVAL" 'BEGIN { printf "%d", s / iv }')
	res=$(ping -n -i "$INTERVAL" -c "$n" "$2" | gaps)
	echo "lost_packets longest_outage_ms: $res"
	[ -n "$4" ] && echo "$(date -u +%FT%TZ),$(echo "$res" | tr ' ' ',')" >> "$4"
	;;
summary)
	[ -f "$2" ] || { echo "no such file"; exit 2; }
	cut -d, -f3 "$2" | sort -n | awk '{ v[NR] = $1 } END {
		if (!NR) { print "no runs"; exit }
		p95 = v[int(NR * 0.95 + 0.999)]; if (!p95) p95 = v[NR]
		med = (NR % 2) ? v[(NR + 1) / 2] : (v[NR / 2] + v[NR / 2 + 1]) / 2
		printf "runs=%d median_ms=%s p95_ms=%s max_ms=%s\n", NR, med, p95, v[NR] }'
	;;
test) # self-test of the parser with synthetic output (no network needed)
	r=$(printf '64 bytes from x: icmp_seq=1 ttl=60\n64 bytes from x: icmp_seq=2 ttl=60\n64 bytes from x: icmp_seq=12 ttl=60\n64 bytes from x: icmp_seq=14 ttl=60\n' | gaps)
	[ "$r" = "10 180" ] && echo "parser ok" || { echo "parser FAILED: $r"; exit 1; }
	;;
*)
	echo "usage: $0 run|summary|test"; exit 2 ;;
esac
