#!/bin/sh
# Build the sandbox: bridges (switches), three vnet jails (node A, node B, client C), the stub scripts and the daemons.
#   sh up.sh [classic|warm]        needs $SBX/hasyncd (FreeBSD build of this repository) and the plugin's ifaces.php / merge_config.php
# Everything lives in vnet jails and under $SBX; the machine's own interfaces and configuration are never touched.
. "$(dirname "$0")/lib.sh"
MODE="${1:-classic}"
case "$MODE" in classic | warm) ;; *) echo "mode: classic | warm"; exit 2 ;; esac
HERE="$(cd "$(dirname "$0")" && pwd)"
[ -x "$SBX/hasyncd" ] || { echo "missing $SBX/hasyncd"; exit 1; }
PLUGIN=/usr/local/opnsense/scripts/OPNsense/ActiveStandby
[ -f "$SBX/scripts/ifaces.php" ] || [ -f "$PLUGIN/ifaces.php" ] || { echo "ifaces.php not found"; exit 1; }

sh "$HERE/down.sh" quiet clean   # always start from a clean state: an old pairing would make "prepare" fail
kldload -n if_epair if_bridge
# the machine's own pf must not filter the traffic between the sandbox ports (restored by down.sh; no other bridge exists on the machine)
[ -f "$SBX/pfil.save" ] || sysctl -n net.link.bridge.pfil_member > "$SBX/pfil.save"
sysctl net.link.bridge.pfil_member=0 >/dev/null
mkdir -p "$SBX/scripts" "$SBX/shims" "$SBX/A" "$SBX/B" "$SBX/C"
cp "$HERE"/scripts/*.php "$SBX/scripts/"
cp "$HERE"/shims/* "$SBX/shims/"
# the two helpers that only read XML are used as they are; ifaces.php can be overridden by a newer copy in $SBX
[ -f "$SBX/ifaces.php.new" ] && cp "$SBX/ifaces.php.new" "$SBX/scripts/ifaces.php" || cp "$PLUGIN/ifaces.php" "$SBX/scripts/ifaces.php"
cp "$PLUGIN/merge_config.php" "$SBX/scripts/merge_config.php"
chmod +x "$SBX"/scripts/*.php "$SBX"/shims/*
[ -e /var/run/activestandby.state ] || touch "$SBX/marker-created"

newepair() { e=$(ifconfig epair create); echo "${e%a}"; }
newbridge() { b=$(ifconfig bridge create); ifconfig "$b" up; echo "$b"; }
SWD=$(newbridge); SWH=$(newbridge); SWM=$(newbridge)
{ echo "SWD=$SWD"; echo "SWH=$SWH"; echo "SWM=$SWM"; } > "$SBX/net.env"

# one epair per node and segment: the "a" end goes into the jail, the "b" end plugs into the bridge
LA=$(newepair); LB=$(newepair); LC=$(newepair)   # data LAN
HA=$(newepair); HB=$(newepair)                    # HA link
MA=$(newepair); MB=$(newepair)                    # management
WA=$(newepair); WB=$(newepair)                    # second data interface (no partner needed: the b end just stays up)
{ echo "LA=$LA LB=$LB LC=$LC HA=$HA HB=$HB MA=$MA MB=$MB WA=$WA WB=$WB"; } >> "$SBX/net.env"
for p in $LA $LB $LC; do ifconfig $SWD addm "${p}b" up; ifconfig "${p}b" up; done
for p in $HA $HB; do ifconfig $SWH addm "${p}b" up; ifconfig "${p}b" up; done
for p in $MA $MB; do ifconfig $SWM addm "${p}b" up; ifconfig "${p}b" up; done
for p in $WA $WB; do ifconfig "${p}b" up; done

jail -c name=hasynA path=/ host.hostname=nodeA vnet vnet.interface="${LA}a,${WA}a,${HA}a,${MA}a" allow.raw_sockets allow.chflags persist || exit 1
jail -c name=hasynB path=/ host.hostname=nodeB vnet vnet.interface="${LB}a,${WB}a,${HB}a,${MB}a" allow.raw_sockets allow.chflags persist || exit 1
jail -c name=hasynC path=/ host.hostname=client vnet vnet.interface="${LC}a" allow.raw_sockets persist || exit 1

setup_node() { # node lan wan ha mg haip mgip
	n=$1
	jx "$n" ifconfig lo0 127.0.0.1/8 up
	jx "$n" ifconfig "${2}a" name tst0
	jx "$n" ifconfig "${3}a" name tst1
	jx "$n" ifconfig "${4}a" name tsh0
	jx "$n" ifconfig "${5}a" name tsm0
	for i in tst0 tst1 tsh0 tsm0; do jx "$n" ifconfig $i up; done
	jx "$n" ifconfig tsh0 "$6/24"
	jx "$n" ifconfig tsm0 "$7/24"
}
setup_node A $LA $WA $HA $MA $HA_A $MG_A
setup_node B $LB $WB $HB $MB $HA_B $MG_B
jx C ifconfig lo0 127.0.0.1/8 up
jx C ifconfig "${LC}a" name tst0
jx C ifconfig tst0 "$CLIENT_IP/24" up
# node A starts as the node that serves traffic: shared MACs and addresses on its data interfaces
jx A ifconfig tst0 link $SHARED_LAN_MAC; jx A ifconfig tst0 inet $LAN_IP/24
jx A ifconfig tst1 link $SHARED_WAN_MAC; jx A ifconfig tst1 inet $WAN_IP/24

render_config() { # node hostname haip mgip
	cat > "$SBX/$1/config.xml" <<XML
<?xml version="1.0"?>
<opnsense>
  <system><hostname>$2</hostname><domain>sandbox.test</domain></system>
  <interfaces>
    <lan><if>tst0</if><enable>1</enable><descr>LAN</descr><ipaddr>$LAN_IP</ipaddr><subnet>24</subnet><spoofmac>$SHARED_LAN_MAC</spoofmac></lan>
    <wan><if>tst1</if><enable>1</enable><descr>WAN</descr><ipaddr>$WAN_IP</ipaddr><subnet>24</subnet><spoofmac>$SHARED_WAN_MAC</spoofmac></wan>
    <opt1><if>tsh0</if><enable>1</enable><descr>HA</descr><ipaddr>$3</ipaddr><subnet>24</subnet></opt1>
    <opt2><if>tsm0</if><enable>1</enable><descr>MGMT</descr><ipaddr>$4</ipaddr><subnet>24</subnet></opt2>
  </interfaces>
  <OPNsense><ActiveStandby><general>
    <enabled>1</enabled><ha_interface>opt1</ha_interface><mgmt_interface>opt2</mgmt_interface><ha_mode>$MODE</ha_mode>
  </general></ActiveStandby></OPNsense>
  <filler>$(printf 'sandbox configuration %.0s' 1 2 3 4 5 6 7 8)</filler>
</opnsense>
XML
}
render_config A nodeA $HA_A $MG_A
render_config B nodeB $HA_B $MG_B
for n in A B; do
	cat > "$SBX/$n/hasyncd.json" <<JSON
{"listen_port":7690,"ha_device":"tsh0","mgmt_device":"tsm0","auto_failover":true,"detect_time":3,"sync_sessions":false,"ha_mode":"$MODE","notify_level":"error"}
JSON
	chmod 600 "$SBX/$n/hasyncd.json"
done
echo "$MODE" > "$SBX/mode"

start_daemon A
start_daemon B
wait_for 20 "both daemons" sh -c ". $HERE/lib.sh; daemon_up A && daemon_up B" || { echo "daemons did not start"; tail -5 "$SBX/A/daemon.log" "$SBX/B/daemon.log"; exit 1; }
echo "sandbox up (mode $MODE): bridges $SWD $SWH $SWM, jails hasynA hasynB hasynC"
