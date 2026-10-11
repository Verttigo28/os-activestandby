#!/bin/sh
# Phase 0 hardware probe for the Supermicro / Intel X710 (ixl) nodes.
# Run as root on a spare OPNsense node (NOT in production traffic) with the data NIC cabled to a switch:
#   sh hw-probe.sh ixl0
# It only touches the NIC named on the command line. Send the output back.
IF="${1:?usage: sh hw-probe.sh <data-nic e.g. ixl0>}"
echo "== $(date) $(uname -a)"; opnsense-version
echo "== driver / sysctl"; sysctl dev.${IF%[0-9]*}.${IF#${IF%%[0-9]*}} 2>/dev/null | grep -E 'fw_version|link|fc|fw_lldp' | head
echo "== BEFORE: $IF"; ifconfig -m "$IF" | grep -E 'flags|status|media'
echo "== admin down"; ifconfig "$IF" down; sleep 5
ifconfig "$IF" | grep -E 'flags|status|media'
echo ">>> NOW LOOK AT THE SWITCH PORT: is link still UP? (the key question; report it)"
sleep 20
echo "== admin up"; ifconfig "$IF" up; sleep 5
ifconfig "$IF" | grep -E 'flags|status|media'
echo "== LLDP/FW-LLDP (ixl firmware LLDP agent can transmit while the port is admin down)"
sysctl dev.${IF%[0-9]*}.${IF#${IF%%[0-9]*}}.fw_lldp 2>/dev/null
echo "== frames transmitted while admin down? (tx counters should not move)"
netstat -I "$IF" -b | head -2
ifconfig "$IF" down; sleep 10; netstat -I "$IF" -b | head -2
echo "== restoring"; ifconfig "$IF" up
