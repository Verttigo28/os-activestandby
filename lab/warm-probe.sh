#!/bin/sh
# Warm standby probe for ONE machine (Supermicro / Intel X710 ixl, or any NIC). It answers, on the real driver, the
# questions the warm standby depends on (docs/validation.md section H: H2, H3, H6):
#   A. how long does the link take to come back after an administrative down/up (the cost warm standby avoids)
#   B. does changing the MAC address with the link UP drop the link (and for how long)
#   C. does the NIC still receive frames for the new MAC (needs a link partner that pings the test address)
#   D. what does ifconfig print for the permanent MAC (hwaddr) and for IPv6 disabled
# Run as root on a spare OPNsense node, NOT in production, with the NIC cabled to a switch or a laptop:
#   sh warm-probe.sh ixl0 [test-ip/prefix]     e.g.  sh warm-probe.sh ixl0 192.0.2.10/24
# With a test IP, step C pauses 30 s: from the partner run  arping -I <partner-nic> 192.0.2.10  (or ping) and check that
# the replies come from 02:aa:bb:cc:dd:ee. Everything is restored at the end. Send the whole output back.
IF="${1:?usage: sh warm-probe.sh <nic e.g. ixl0> [test-ip/prefix]}"
TESTIP="$2"
NEWMAC="02:aa:bb:cc:dd:ee"
TMP=$(mktemp -d) || exit 1

ifconfig "$IF" >/dev/null 2>&1 || { echo "no such interface $IF"; exit 1; }
ORIG=$(ifconfig "$IF" | awk '$1=="ether"{print $2; exit}')
HW=$(ifconfig "$IF" | awk '$1=="hwaddr"{print $2; exit}')
[ -n "$HW" ] && ORIG="$HW"
echo "== $(date) $(uname -sr)"
echo "== NIC $IF permanent MAC $ORIG"
ifconfig "$IF" | grep -E 'flags|ether|hwaddr|status|media|nd6'

restore() {
	ifconfig "$IF" link "$ORIG" 2>/dev/null
	[ -n "$TESTIP" ] && ifconfig "$IF" inet "$TESTIP" -alias 2>/dev/null
	ifconfig "$IF" -promisc up 2>/dev/null
	rm -rf "$TMP"
	echo "== restored $IF to MAC $ORIG"
}
trap restore EXIT INT TERM

active() { ifconfig "$IF" | grep -q 'status: active'; }

echo
echo "== A. link-up time after administrative down/up (10 samples of 100 ms = 1 s; limit 20 s)"
ifconfig "$IF" up; i=0
while ! active && [ $i -lt 200 ]; do sleep 0.1; i=$((i + 1)); done
echo "link was up before the test: $([ $i -eq 0 ] && echo yes || echo no, waited $((i * 100)) ms)"
for run in 1 2 3; do
	ifconfig "$IF" down; sleep 3
	ifconfig "$IF" up; i=0
	while ! active && [ $i -lt 200 ]; do sleep 0.1; i=$((i + 1)); done
	if active; then echo "run $run: link up again after about $((i * 100)) ms"; else echo "run $run: NO LINK after 20 s"; fi
	sleep 2
done

echo
echo "== B. MAC change with the link up (samples every 20 ms; lost = samples without 'status: active')"
sample() { # $1 = seconds
	n=$(($1 * 50)); k=0; lost=0
	while [ $k -lt $n ]; do
		active || lost=$((lost + 1))
		sleep 0.02; k=$((k + 1))
	done
	echo "$lost" > "$TMP/lost"
}
change() { # $1 = label, $2 = new MAC (kept apart: a MAC contains colons)
	sample 4 &
	pid=$!
	sleep 1
	ifconfig "$IF" link "$2"
	wait $pid
	lost=$(cat "$TMP/lost")
	echo "change $1: about $((lost * 20)) ms without carrier (0 = the link never dropped)"
	echo "   ether now: $(ifconfig "$IF" | awk '$1=="ether"{print $2; exit}')   hwaddr line: $(ifconfig "$IF" | awk '$1=="hwaddr"{print $2; exit}')"
}
change "to $NEWMAC" "$NEWMAC"
change "back to $ORIG" "$ORIG"

# 'up' on an interface that is already up: does it re-initialise the link? (the warm commands end with it)
sample 4 &
pid=$!
sleep 1
ifconfig "$IF" up
wait $pid
echo "ifconfig up on an up interface: about $(($(cat "$TMP/lost") * 20)) ms without carrier"

echo
echo "== D. what ifconfig prints with a changed MAC, IPv6 disabled, promiscuous off"
ifconfig "$IF" link "$NEWMAC"
ifconfig "$IF" inet6 ifdisabled -promisc up
ifconfig "$IF" | grep -E 'flags|ether|hwaddr|nd6|status'

if [ -n "$TESTIP" ]; then
	echo
	echo "== C. receive test: $TESTIP on MAC $NEWMAC for 30 s, promiscuous OFF"
	ifconfig "$IF" inet6 -ifdisabled
	ifconfig "$IF" inet "$TESTIP" up
	echo ">>> NOW from the partner: arping / ping $(echo "$TESTIP" | cut -d/ -f1) and note whether it answers, with which MAC"
	sleep 30
	echo "frames received on $IF (input packets):"; netstat -I "$IF" -b | head -2
else
	echo
	echo "== C. skipped (give a test IP/prefix as second argument to test reception for the new MAC)"
fi
