#!/bin/sh
# Scenarios against the two real daemons of the sandbox. Usage: sh scenarios.sh classic|warm [only-section-number...]
# The sandbox is rebuilt first. PASS/FAIL per check, a double-active detector runs over the whole time.
. "$(dirname "$0")/lib.sh"
MODE="${1:-classic}"; shift
ONLY="$*"
HERE="$(cd "$(dirname "$0")" && pwd)"
sh "$HERE/up.sh" "$MODE" || exit 1
. "$SBX/net.env"
want() { [ -z "$ONLY" ] || case " $ONLY " in *" $1 "*) return 0 ;; esac; return 1; }
echo "sandbox mode: $MODE   $(/usr/bin/uname -sr)   hasyncd $("$SBX/hasyncd" version)"

monitor_start
sleep 0.5

# ---------------------------------------------------------------------------------------------------------------------------
section "1. pairing and steady state"
TOK=$(ctl B prepare | jf token)
ck() { check "$@"; }
ck "pairing token issued" '[ -n "$TOK" ]'
ctl A pair --peer $HA_B --token "$TOK" > "$SBX/pair.out"
ck "pairing accepted" 'grep -q "\"status\":\"paired\"" "$SBX/pair.out"'
wait_for 20 "roles" '[ "$(st A state)" = active ] && [ "$(st B state)" = standby ]'
ck "A is master and active, B standby" '[ "$(st A role)" = master ] && [ "$(st A state)" = active ] && [ "$(st B state)" = standby ]'
ck "heartbeat alive on the HA link and the management path (seen by A and by B)" '[ "$(st A paths)" = "{\"ha\":true,\"mgmt\":true}" ] && [ "$(st B paths)" = "{\"ha\":true,\"mgmt\":true}" ]'
wait_for 30 "config in sync" '[ "$(ctl A status | jf sync.state)" = in_sync ]'
ck "configuration in sync" '[ "$(ctl A status | jf sync.state)" = in_sync ]'
ck "the standby holds the full configuration (active.xml) and its own hostname in config.xml" 'grep -q "<interfaces>" "$SBX/B/state/active.xml" && grep -q "<hostname>nodeB</hostname>" "$SBX/B/config.xml"'
ck "A owns the shared MAC and address, B does not" 'owns A && ! owns B'
ck "B has no address on its data interfaces" '! has_ip B tst0 $LAN_IP && ! has_ip B tst1 $WAN_IP'
if [ "$MODE" = warm ]; then
	wait_for 30 "warm ready" '[ "$(ctl B status | jf warm.ready)" = true ]'
	ck "warm: B reports ready" '[ "$(ctl B status | jf warm.ready)" = true ]'
	ck "warm: B keeps tst0 and tst1 up, on its own MAC, without address, IPv6 disabled" 'jx B ifconfig -l -u | grep -q tst0 && jx B ifconfig -l -u | grep -q tst1 && [ "$(mac_of B tst0)" != "$SHARED_LAN_MAC" ] && jx B ifconfig tst0 | grep -q IFDISABLED'
	ck "warm: A sees the standby ready in the heartbeat" '[ "$(ctl A status | jf peer_warm)" = ready ]'
else
	wait_for 10 "B data interfaces down" '! jx B ifconfig -l -u | grep -q tst0 && ! jx B ifconfig -l -u | grep -q tst1'
	ck "classic: B keeps tst0 and tst1 down" '! jx B ifconfig -l -u | grep -q tst0 && ! jx B ifconfig -l -u | grep -q tst1'
fi
ck "the client reaches the shared address" 'jx C ping -c 3 -t 2 $LAN_IP'
ck "the data switch learned the shared MAC on A's port" '[ "$(bridge_port_of_mac $SHARED_LAN_MAC)" = "${LA}b" ]'
t1=$(tx_packets B tst0); sleep 5; t2=$(tx_packets B tst0)
ck "the standby is silent on the wire (no frame sent by B's data interface in 5 s)" '[ "$t1" = "$t2" ]'
info "B tst0 tx packets: $t1 -> $t2"
ck "no double active so far" '[ "$(monitor_violations)" -eq 0 ]'

# ---------------------------------------------------------------------------------------------------------------------------
if want 2; then
section "2. graceful failover A -> B and back, with client traffic"
monitor_clear
for dir in "A B" "B A"; do
	set -- $dir; from=$1; to=$2
	ping_run 12 > "$SBX/ping.out" &
	pp=$!
	sleep 3
	t0=$(now)
	ctl $from failover > "$SBX/fo.out"
	t1=$(now)
	wait $pp
	info "failover $from -> $to: command took $(php -r "echo round(($t1-$t0)*1000);") ms; client: $(cat "$SBX/ping.out")"
	ck "$from -> $to: the failover reports success" 'grep -q "\"status\":\"ok\"" "$SBX/fo.out"'
	wait_for 20 "state" '[ "$(st '$to' state)" = active ] && [ "$(st '$from' state)" = standby ]'
	ck "$from -> $to: $to active, $from standby" '[ "$(st '$to' state)" = active ] && [ "$(st '$from' state)" = standby ]'
	ck "$from -> $to: only $to owns the shared MAC and address" 'owns '$to' && ! owns '$from
	eval "toport=\${L$to}b"
	ck "$from -> $to: the data switch learned the shared MAC on the port of $to" '[ "$(bridge_port_of_mac $SHARED_LAN_MAC)" = "$toport" ]'
	ck "$from -> $to: the client reaches the shared address" 'jx C ping -c 3 -t 2 $LAN_IP'
	ck "$from -> $to: the WAN interface moved too" 'has_ip '$to' tst1 $WAN_IP && ! has_ip '$from' tst1 $WAN_IP'
	ck "$from -> $to: the old active node is silent on its data interfaces (no address)" '! has_ip '$from' tst0 $LAN_IP && ! has_ip '$from' tst1 $WAN_IP'
	if [ "$MODE" = warm ]; then
		ck "$from -> $to: warm: the old active keeps its links up (silent, own MAC)" 'jx '$from' ifconfig -l -u | grep -q tst0 && [ "$(mac_of '$from' tst0)" != "$SHARED_LAN_MAC" ]'
		ck "$from -> $to: warm: the promotion used the native fast path" 'ctl '$to' history | grep -q "natively"'
	fi
	info "history on $to: $(ctl $to history | head -c 700)"
	wait_for 30 "sync" '[ "$(ctl '$to' status | jf sync.state)" = in_sync ]'
	sleep 2
done
ck "no double active during the graceful failovers" '[ "$(monitor_violations)" -eq 0 ]'
info "$(monitor_summary)"
fi

# ---------------------------------------------------------------------------------------------------------------------------
if want 3; then
section "3. daemon restarts: standby (no role change), active (short crash), unreadable state"
monitor_clear
ensure_active A
stop_daemon B; sleep 1; start_daemon B; wait_for 20 "B daemon" "daemon_up B"
sleep 6
ck "standby daemon restart: roles unchanged, A still owns the identity" '[ "$(st A state)" = active ] && [ "$(st B state)" = standby ] && owns A && ! owns B'
ck "standby daemon restart: heartbeats are back on both paths" '[ "$(st A paths)" = "{\"ha\":true,\"mgmt\":true}" ]'
stop_daemon A; sleep 0.5; start_daemon A; wait_for 20 "A daemon" "daemon_up A"
sleep 6
ck "active daemon restart within a second: no failover, A still active and owning" '[ "$(st A state)" = active ] && owns A && ! owns B'
ck "the standby did not promote" '[ "$(st B state)" = standby ]'
ck "no double active" '[ "$(monitor_violations)" -eq 0 ]'
fi

# ---------------------------------------------------------------------------------------------------------------------------
if want 4; then
section "4. power cut of the active node (automatic takeover), then its return"
monitor_clear
ensure_active A
ping_run 20 > "$SBX/ping.out" &
pp=$!
sleep 3
t0=$(now)
power_off A
wait_for 30 "B promoted" '[ "$(st B state)" = active ]'
t1=$(now)
wait $pp
info "power cut -> B active after $(php -r "echo round(($t1-$t0)*1000);") ms; client: $(cat "$SBX/ping.out")"
ck "B promoted by itself after the active node vanished" '[ "$(st B state)" = active ] && owns B'
ck "the client reaches the shared address again" 'jx C ping -c 3 -t 2 $LAN_IP'
ck "the data switch learned the shared MAC on B's port" '[ "$(bridge_port_of_mac $SHARED_LAN_MAC)" = "${LB}b" ]'
info "history on B: $(ctl B history | head -c 900)"
ck "the promotion recorded how long the peer had been silent (detect_ms)" 'ctl B history | grep -q detect_ms'
ck "no double active during the takeover" '[ "$(monitor_violations)" -eq 0 ]'
# A returns
power_on A
sleep 12
ck "A returns as standby and does not take the role back" '[ "$(st A state)" = standby ] && [ "$(st B state)" = active ] && owns B && ! owns A'
ck "heartbeats are back on both paths" '[ "$(st B paths)" = "{\"ha\":true,\"mgmt\":true}" ]'
wait_for 30 "sync" '[ "$(ctl B status | jf sync.state)" = in_sync ]'
ck "configuration in sync again after the return" '[ "$(ctl B status | jf sync.state)" = in_sync ]'
ck "no double active after the return" '[ "$(monitor_violations)" -eq 0 ]'
fi

# ---------------------------------------------------------------------------------------------------------------------------
if want 5; then
section "5. split-brain forced on purpose (resolution speed)"
ensure_active B
sleep 2
monitor_clear
t0=$(now)
ctl A promote --force > "$SBX/promote.out"
info "forced promote on the standby: $(cat "$SBX/promote.out" | head -c 200)"
wait_for 30 "single owner" '{ owns A && ! owns B; } || { owns B && ! owns A; }'
t1=$(now)
sleep 2
info "split-brain resolved after $(php -r "echo round(($t1-$t0)*1000);") ms from the forced promotion; $(monitor_summary)"
ck "exactly one node owns the shared identity afterwards" '{ owns A && ! owns B; } || { owns B && ! owns A; }'
ck "exactly one node is active" '[ "$(st A state)" != "$(st B state)" ]'
ck "the loser reported the conflict" 'events A | grep -qi "split-brain" || events B | grep -qi "split-brain"'
ck "the overlap was short (at most 5 s)" '[ "$(awk "\$2==1&&\$3==1{n++} END{print n+0}" "$SBX/monitor.log")" -le 50 ]'
fi

# ---------------------------------------------------------------------------------------------------------------------------
if want 6; then
section "6. packet filter failure at promotion (no false ACTIVE)"
monitor_clear
ensure_active A
for flag in pf_disabled filter_fail; do
	touch "$SBX/B/$flag"
	ctl A failover > "$SBX/fo.out"
	info "failover with B's $flag: $(head -c 330 "$SBX/fo.out")"
	ck "$flag: the failover is reported as failed" 'grep -q "\"error\"" "$SBX/fo.out"'
	wait_for 30 "A active again" '[ "$(st A state)" = active ] && owns A'
	ck "$flag: A took its role back after the peer confirmed it will not become active" '[ "$(st A state)" = active ] && owns A'
	ck "$flag: B is standby and silent, nothing of the shared identity left on B" '[ "$(st B state)" = standby ] && ! owns B && ! has_ip B tst0 $LAN_IP && ! has_ip B tst1 $WAN_IP'
	ck "$flag: B recorded why" 'events B | grep -qi "packet filter"'
	ck "$flag: the client still reaches the shared address" 'jx C ping -c 3 -t 2 $LAN_IP'
	rm -f "$SBX/B/$flag"
	sleep 8
done
ck "no double active" '[ "$(monitor_violations)" -eq 0 ]'
fi

# ---------------------------------------------------------------------------------------------------------------------------
if want 7 && [ "$MODE" = warm ]; then
section "7. warm: standby not ready, and a failed fast path, fall back to the classic takeover"
monitor_clear
ensure_active A
ifconfig "${LB}b" down
wait_for 15 "not ready" '[ "$(ctl B status | jf warm.ready)" = false ]'
ck "B reports not ready with the reason (no link)" '[ "$(ctl B status | jf warm.ready)" = false ] && ctl B status | jf warm.reason | grep -q "no link"'
ck "A sees it in the heartbeat" '[ "$(ctl A status | jf peer_warm)" != ready ]'
ifconfig "${LB}b" up
wait_for 15 "ready again" '[ "$(ctl B status | jf warm.ready)" = true ]'
ck "B is ready again once the link is back" '[ "$(ctl B status | jf warm.ready)" = true ]'
# a fast path that fails after the readiness was established: the reconfigure stub is not used by it, so make the MAC change impossible
# by giving B's tst0 a stale address behind the daemon's back (it is not silent any more at the moment of the takeover)
jx B ifconfig tst0 inet 192.0.2.77/24
ctl A failover > "$SBX/fo.out"
info "failover with a non-silent warm port on B: $(head -c 300 "$SBX/fo.out")"
wait_for 30 "B active" '[ "$(st B state)" = active ]'
ck "the takeover still succeeded (classic path took over)" '[ "$(st B state)" = active ] && owns B'
ck "the event log says the warm takeover was not possible" 'events B | grep -qi "warm takeover not possible"'
jx B ifconfig tst0 inet 192.0.2.77 -alias 2>/dev/null
ck "no double active" '[ "$(monitor_violations)" -eq 0 ]'
fi

# ---------------------------------------------------------------------------------------------------------------------------
if want 8; then
section "8. the peer dies during a hand-over (no rollback, survivor recovers)"
monitor_clear
ensure_active B
sleep 2
( ctl B failover > "$SBX/fo.out" 2>&1 ) &
fp=$!
sleep 0.05
power_off A
t0=$(now)
wait $fp
info "failover while the target dies: $(head -c 420 "$SBX/fo.out")"
ck "the hand-over is reported as unverifiable and B stays standby on purpose" 'grep -q "stays standby on purpose" "$SBX/fo.out" || grep -q "peer did not take over" "$SBX/fo.out"'
wait_for 40 "B active again by the peer-lost rule" '[ "$(st B state)" = active ] && owns B'
t1=$(now)
info "service restored by the survivor after $(php -r "echo round(($t1-$t0)*1000);") ms from the death of the peer"
ck "the survivor serves traffic again" '[ "$(st B state)" = active ] && owns B && jx C ping -c 3 -t 2 $LAN_IP'
ck "no double active" '[ "$(monitor_violations)" -eq 0 ]'
power_on A
sleep 10
ck "the dead node returns as standby" '[ "$(st A state)" = standby ] && owns B && ! owns A'
fi

# ---------------------------------------------------------------------------------------------------------------------------
if want 9; then
section "9. configuration change on the active node is replicated"
monitor_clear
act=A; [ "$(st B state)" = active ] && act=B
sby=$(other $act)
wait_for 30 "sync" '[ "$(ctl '$act' status | jf sync.state)" = in_sync ]'
marker="sandbox-marker-$(date +%s)"
chflags noschg "$SBX/$act/config.xml" 2>/dev/null
sed -i '' "s|</opnsense>|  <$marker>1</$marker>\n</opnsense>|" "$SBX/$act/config.xml"
ctl $act sync-now >/dev/null
wait_for 40 "standby has it" 'grep -q "'$marker'" "$SBX/'$sby'/state/active.xml"'
ck "the standby received the new revision (marker in its saved full configuration)" 'grep -q "'$marker'" "$SBX/'$sby'/state/active.xml"'
wait_for 30 "in sync" '[ "$(ctl '$act' status | jf sync.state)" = in_sync ]'
ck "sync state is in_sync again" '[ "$(ctl '$act' status | jf sync.state)" = in_sync ]'
ck "the standby's config.xml kept its own hostname (node-local part)" 'grep -q "<hostname>node'$sby'</hostname>" "$SBX/'$sby'/config.xml"'
fi

# ---------------------------------------------------------------------------------------------------------------------------
if want 10; then
section "10. loss of communication paths"
act=A; [ "$(st B state)" = active ] && act=B
sby=$(other $act)
eval "hap=\${H$act}; hsp=\${H$sby}; map=\${M$act}; msp=\${M$sby}"
monitor_clear
# a) HA link lost, management alive
ifconfig $SWH deletem "${hap}b"
sleep 8
ck "HA path lost only: both nodes alive on the management path, no role change" '[ "$(st '$act' state)" = active ] && [ "$(st '$sby' state)" = standby ] && [ "$(st '$sby' peer_alive)" = true ]'
ck "the HA path is reported dead and the management path alive" '[ "$(st '$sby' paths)" = "{\"ha\":false,\"mgmt\":true}" ]'
ifconfig $SWH addm "${hap}b"
sleep 5
# b) management lost, HA alive
ifconfig $SWM deletem "${map}b"
sleep 8
ck "management path lost only: no role change" '[ "$(st '$act' state)" = active ] && [ "$(st '$sby' state)" = standby ]'
ifconfig $SWM addm "${map}b"
sleep 5
ck "no double active with a single path lost" '[ "$(monitor_violations)" -eq 0 ]'
# c) the standby is isolated: its own carrier is gone on both paths (it must NOT promote)
ifconfig "${hsp}b" down; ifconfig "${msp}b" down
sleep 9
ck "isolated standby (no carrier on HA and management) does not promote" '[ "$(st '$sby' state)" = standby ] && owns '$act' && ! owns '$sby
info "reason shown to the administrator: $(ctl $sby status | jf auto_blocked)"
ck "the reason is visible (auto_blocked)" '[ -n "$(ctl '$sby' status | jf auto_blocked)" ]'
ifconfig "${hsp}b" up; ifconfig "${msp}b" up
sleep 6
ck "after the carrier is back the cluster is intact" '[ "$(st '$act' state)" = active ] && [ "$(st '$sby' state)" = standby ] && [ "$(st '$sby' peer_alive)" = true ]'
ck "no double active" '[ "$(monitor_violations)" -eq 0 ]'
fi

# ---------------------------------------------------------------------------------------------------------------------------
if want 11; then
section "11. symmetric partition of the two nodes only (the limit of two nodes without an arbiter)"
act=A; [ "$(st B state)" = active ] && act=B
sby=$(other $act)
eval "hap=\${H$act}; hsp=\${H$sby}; map=\${M$act}; msp=\${M$sby}"
monitor_clear
ifconfig $SWH deletem "${hap}b"; ifconfig $SWM deletem "${map}b"   # every cable stays plugged, the two nodes just cannot talk
t0=$(now)
wait_for 30 "standby promotes" '[ "$(st '$sby' state)" = active ]'
t1=$(now)
sleep 3
info "partition: the standby promoted after $(php -r "echo round(($t1-$t0)*1000);") ms; overlap so far: $(monitor_summary)"
ck "EXPECTED LIMIT: without fencing a standby that sees its own carrier up promotes during a full partition (double active)" '[ "$(st '$sby' state)" = active ] && [ "$(st '$act' state)" = active ]'
t2=$(now)
ifconfig $SWH addm "${hap}b"; ifconfig $SWM addm "${map}b"
wait_for 30 "single owner" '{ owns A && ! owns B; } || { owns B && ! owns A; }'
t3=$(now)
info "after the partition healed, single owner after $(php -r "echo round(($t3-$t2)*1000);") ms"
ck "when the paths come back the epoch rule leaves exactly one owner" '{ owns A && ! owns B; } || { owns B && ! owns A; }'
ck "exactly one active node" '[ "$(st A state)" != "$(st B state)" ]'
fi

# ---------------------------------------------------------------------------------------------------------------------------
if want 12; then
section "12. crash of the active node's daemon for 6 s (interfaces still up)"
act=A; [ "$(st B state)" = active ] && act=B
sby=$(other $act)
wait_for 20 "healthy" '[ "$(st '$act' state)" = active ] && [ "$(st '$sby' state)" = standby ]'
sleep 3
monitor_clear
stop_daemon $act
sleep 6
info "after 6 s without the active daemon: $(monitor_summary); standby state: $(st $sby state)"
start_daemon $act; wait_for 20 "daemon" "daemon_up $act"
t0=$(now)
wait_for 30 "single owner" '{ owns A && ! owns B; } || { owns B && ! owns A; }'
t1=$(now)
info "single owner $(php -r "echo round(($t1-$t0)*1000);") ms after the daemon came back; $(monitor_summary)"
ck "after the daemon is back exactly one node owns the identity" '{ owns A && ! owns B; } || { owns B && ! owns A; }'
ck "exactly one active node" '[ "$(st A state)" != "$(st B state)" ]'
fi

# ---------------------------------------------------------------------------------------------------------------------------
if want 13; then
section "13. packet loss on the peer link (no needless failover)"
act=A; [ "$(st B state)" = active ] && act=B
sby=$(other $act)
wait_for 20 "healthy" '[ "$(st '$act' state)" = active ] && [ "$(st '$sby' state)" = standby ] && [ "$(st '$act' peer_alive)" = true ]'
sleep 3
monitor_clear
printf 'block drop in quick proto tcp from any to any port 7690 probability 20%%\n' > "$SBX/loss.rules"
jx $sby pfctl -e >/dev/null 2>&1
jx $sby pfctl -f "$SBX/loss.rules" 2>/dev/null
sleep 25
ck "20% loss on both heartbeat paths for 25 s: no failover, roles unchanged" '[ "$(st '$act' state)" = active ] && [ "$(st '$sby' state)" = standby ] && owns '$act' && ! owns '$sby
jx $sby pfctl -F rules >/dev/null 2>&1; jx $sby pfctl -d >/dev/null 2>&1
sleep 4
ck "no double active" '[ "$(monitor_violations)" -eq 0 ]'
fi

# ---------------------------------------------------------------------------------------------------------------------------
if want 14; then
section "14. frozen peer: a promote request delivered 95 s late (SIGSTOP / SIGCONT)"
act=A; [ "$(st B state)" = active ] && act=B
sby=$(other $act)
wait_for 20 "healthy" '[ "$(st '$act' state)" = active ] && [ "$(st '$sby' state)" = standby ] && [ "$(st '$act' peer_alive)" = true ]'
sleep 3
monitor_clear
jx $sby pkill -STOP -f "hasyncd daemon"
( ctl $act failover > "$SBX/fo.out" 2>&1 ) &
fp=$!
sleep 95
info "after 95 s the old active node says: $(ctl $act status | jf failover_in_progress) in progress, state $(st $act state)"
jx $sby pkill -CONT -f "hasyncd daemon"
sleep 50
wait $fp 2>/dev/null
info "result of the failover command: $(head -c 400 "$SBX/fo.out")"
info "events on $act: $(events $act | head -c 900)"
ck "never two nodes owning the shared identity during the whole freeze and the late delivery" '[ "$(monitor_violations)" -eq 0 ]'
ck "exactly one active node at the end" '[ "$(st A state)" != "$(st B state)" ] && { owns A || owns B; } && ! { owns A && owns B; }'
info "final roles: A=$(st A state) B=$(st B state)   $(monitor_summary)"
fi

monitor_stop
section "final checks"
ck "no daemon panic or fatal error in the logs" '! grep -qE "panic:|fatal error:|goroutine [0-9]+ \[" "$SBX/A/daemon.log" "$SBX/B/daemon.log"'
ck "the machine's real interfaces were never touched (the host still has its own ixl/igb/lagg set)" 'ifconfig -l | grep -q "ixl0 ixl1 igb0 igb1"'
summary
