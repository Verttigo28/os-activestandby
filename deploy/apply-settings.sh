#!/bin/sh
# Apply the Active/Standby plugin settings to THIS firewall through the OPNsense API (the same thing as the Settings tab).
# Run it on the firewall after installing the plugin. It does not pair the nodes: use the wizard for that (docs/getting-started.md).
#
#   1. System > Access > Users > (your admin user) > API keys: create a key, keep key and secret.
#   2. Edit the values below (or export them), then:
#        API_KEY=... API_SECRET=... HA_IF=opt1 MGMT_IF=opt2 sh apply-settings.sh        # apply and restart the service
#        API_KEY=... API_SECRET=... sh apply-settings.sh show                           # print the current settings
#
# HA_IF / MGMT_IF are the interface KEYS of OPNsense (lan, opt1, opt2, ...: Interfaces > Assignments), not device names.
# The HA interface needs a static IPv4 address first (for example 10.99.0.1/24 on one node, 10.99.0.2/24 on the other).
# Credentials are passed to curl on stdin, never on the command line.

HOST="${HOST:-127.0.0.1}"
HA_IF="${HA_IF:-}"                # the dedicated cable to the other firewall (required)
MGMT_IF="${MGMT_IF:-}"            # management interface, stays up on the standby; empty = none (weaker split-brain protection)

# --- recommended values for a Supermicro with real (Intel) NICs; shared settings are copied to the standby from the active node
ENABLED="${ENABLED:-1}"                         # this firewall runs the HA service
AUTO_FAILOVER="${AUTO_FAILOVER:-1}"                   # the standby takes over by itself
DETECT_TIME="${DETECT_TIME:-3}"                     # seconds of silence before the peer counts as dead (1-30; 3 is the safe default)
HA_MODE="${HA_MODE:-classic}"                   # classic | warm (experimental: validate with lab/warm-probe.sh and docs/validation.md section H first)
VIRTUAL_MAC="${VIRTUAL_MAC:-1}"                     # generated shared MAC (02:...) instead of the primary's hardware MAC
FORCE_PROMISC="${FORCE_PROMISC:-0}"                   # only for virtual NICs; real NICs reprogram their receive filter
PREEMPT="${PREEMPT:-0}"                         # 1: the primary takes the active role back by itself after a failback delay
SYNC_SESSIONS="${SYNC_SESSIONS:-1}"                   # GUI logins survive a failover
HEALTH_SERVICES="${HEALTH_SERVICES:-0}"                 # 1: restart/switch when pf or IPsec fail on the active node
MGMT_PROBE_TARGETS="${MGMT_PROBE_TARGETS:-}"             # optional: IPv4 of the management switch, pinged from the management address on both nodes
MONITOR_IFS="${MONITOR_IFS:-}"                   # comma separated interface KEYS whose loss of link on the active node causes a failover, e.g. wan,lan,opt1 (empty: none, a dead NIC then causes no failover)
PROBE_TARGETS="${PROBE_TARGETS:-}"                  # optional: IPv4 addresses (the upstream gateway) the active node must reach; none answering causes a failover (catches "link up but no traffic")
# fencing is optional: FENCE_COMMAND="" (an absolute path to a root-owned executable, see docs/security.md)
FENCE_COMMAND="${FENCE_COMMAND:-}"

API="https://$HOST/api/activestandby"
creds() { printf 'user = "%s:%s"\n' "${API_KEY:?set API_KEY}" "${API_SECRET:?set API_SECRET}"; }
get() { creds | curl -sk -K - "$API$1"; }
post() { # path, then curl arguments (the data)
	p=$1; shift
	creds | curl -sk -K - -X POST "$@" "$API$p"
}

if [ "$1" = "show" ]; then
	get /settings/get
	echo
	exit 0
fi

[ -n "$HA_IF" ] || { echo "set HA_IF to the interface key of the HA link (for example opt1)"; exit 2; }
[ "$HA_IF" != "$MGMT_IF" ] || { echo "HA and management interface must differ"; exit 2; }
case "$HA_MODE" in classic | warm) ;; *) echo "HA_MODE must be classic or warm"; exit 2 ;; esac

# every value is URL-encoded by curl: a space or an ampersand in a value cannot add or change another setting
set --
add() { set -- "$@" --data-urlencode "activestandby[general][$1]=$2"; }
add enabled "$ENABLED"; add ha_interface "$HA_IF"; add mgmt_interface "$MGMT_IF"
add auto_failover "$AUTO_FAILOVER"; add detect_time "$DETECT_TIME"; add ha_mode "$HA_MODE"
add virtual_mac "$VIRTUAL_MAC"; add force_promisc "$FORCE_PROMISC"; add preempt "$PREEMPT"
add sync_sessions "$SYNC_SESSIONS"; add health_services "$HEALTH_SERVICES"
add mgmt_probe_targets "$MGMT_PROBE_TARGETS"; add fence_command "$FENCE_COMMAND"
add monitor_interfaces "$MONITOR_IFS"; add probe_targets "$PROBE_TARGETS"

echo "== saving the settings"
out=$(post /settings/set "$@")
echo "$out"
case "$out" in *'"saved"'*) ;; *) echo "NOT saved (see the validation messages above)"; exit 1 ;; esac

echo "== applying (renders the daemon configuration and starts the service)"
post /service/reconfigure -d "_=1"
echo
if [ -z "$MONITOR_IFS" ] && [ -z "$PROBE_TARGETS" ] && [ "$HEALTH_SERVICES" != 1 ]; then
	echo "!! nothing is monitored (MONITOR_IFS, PROBE_TARGETS, HEALTH_SERVICES are empty): a dead network card on the active node will NOT cause a failover."
fi
echo "== next: Status page, then the wizard (Setup wizard) to pair with the second firewall."
