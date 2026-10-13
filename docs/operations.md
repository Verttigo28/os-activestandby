# Operations

## Daily use

- **Edit the configuration on the active node only.** It replicates to the standby within about a second. The standby's configuration is locked (immutable `config.xml`): changes made there do not reach disk, and a banner in the status bar says "STANDBY" on every page.
- The *Status* page shows both nodes, sync state, failover protection and recent events. On a standby it shows only a single button, **Make this firewall the active one**; if the active node does not answer, an emergency option appears.
- Node-local and never replicated: hostname, the HA link and management interface definitions, whether the service is enabled and its port, `/conf/activestandby/*`. Everything else in `config.xml` is replicated, plus the list of installed `os-*` plugins, DHCP leases, firewall states and GUI sessions.
- Plugin settings are **shared**: change them on the active node and the standby adopts them within seconds (it restarts its daemon once). Per node: whether the service is enabled, the HA and management interface choice and the peer port.

## What happens when

| Event | Behaviour |
|---|---|
| Planned failover (button, health switch, preemption, upgrade) | about 0.7 s of lost traffic in the lab, 1.3-1.9 s for the whole operation |
| Active node loses power or crashes | the standby promotes after the detection time (default 3 s, 1-30) of silence on **all** heartbeat paths, plus about 0.5 s: about 1.5 s with a 1 s detection time |
| Standby cannot prove it is not the isolated node (its management/HA carrier is down) | **no** automatic promotion; event "held back"; promote manually if the peer is really down |
| Both nodes up, nobody active (cold start) | the primary promotes about 5 s after the daemons start, the secondary only after about 30 s |
| A node reboots | always boots as standby, then follows the rules above; failback is manual unless *preemption* is on |
| Both nodes active (split-brain) | higher epoch wins (tie: primary); the other demotes as soon as two heartbeats confirm it (about 1 s), also in maintenance mode and with automatic failover off |
| Configuration saved on the active node | pushed as a numbered revision within about a second; the standby merges, reloads pf only, acknowledges; on any error it rolls back and shows the error |
| Standby without WAN | plugin installs go through a restricted proxy on the active node; the standby clock follows the active node |

Real NICs add link-up time (typically 1-3 s for copper) that the lab cannot show.

## Warm standby (experimental)

Setting *Standby mode* (shared, default *Classic*). In *Warm standby* the standby keeps the links of its eligible shared interfaces up, with its own MAC address, no IP address and IPv6 off, so a takeover does not wait for a link to come up. Read [warm standby](fast-ha.md) first: it has never run on hardware, needs a switch and NIC drivers that accept the MAC change, and falls back to the classic takeover for interfaces that do not qualify and whenever anything looks wrong.

Check *Pre-flight* after switching it on: the standby must report *ready*, and the interfaces that keep the classic takeover are listed with the reason. To go back, set the mode to *Classic*; the standby takes the warm ports down again within a few seconds.


## Hot standby (experimental)

Setting *Standby mode* = *Hot standby*. The standby keeps the LAGG, its ports and the VLANs up, already carrying the shared MAC, with no address and IPv6 off; a takeover adds the addresses (about 0.2 s) and the packet filter of the standby already holds the ruleset of the active node (the *ruleset mirror*). Read [Warm and hot standby](fast-ha.md#hot-standby-the-takeover-that-adds-addresses-and-nothing-else) first.

**Before switching it on** (both firewalls get the same mode: the setting is replicated from the active node)
1. The [switch checklist](#switch-checklist) is done: edge ports, no port security, **one static port-channel per firewall** if the LAGG is LACP.
2. LACP LAGG: choose *Hot standby with an LACP LAGG*. *Off* keeps the classic takeover for an interface that is directly on the LAGG (safe on any switch); *Shared* is the fast one and needs the static port-channels (Meraki aggregates and Juniper `ae` interfaces are static in that sense; a switch that aggregates by itself from the partner system id is not).
3. Pre-flight must pass before a failover test: *hot standby: ready ... up and silent* on the standby and *standby ruleset: the standby holds the ruleset of this node*. The interfaces that keep the classic takeover are listed with the reason (DHCP, PPPoE, an MTU, a bridge, ...).

**Switching on** (in a maintenance window the first time): set the mode on the active node, wait for the standby to report *ready*, look at the switch (the shared MAC stays on the port of the active node; the standby's ports are up and send only LLDP from their own MAC), then run a *Failover now* with a client pinging every 20 ms: the outage should be a fraction of a second. Do the same for a crash (power off the active node) once.

**What the daemon watches for you.** On the standby, the transmit counter of every silent interface: one that transmits is shut down and the standby reports *not ready* (event log: "transmitted N packet(s) while it should be silent"); the next takeover is then classic. The packet filter of the standby must have its default deny. The standby also checks, before and after every takeover step, that each interface is in the state the plan says, and falls back to the classic takeover on any difference.

**Going back**: set the mode to *Classic*; the standby takes its ports down again within a few seconds. Nothing else has to be undone.

**Choosing the detection time**: see [fast-ha](fast-ha.md#detection-time-of-1-second). 1-2 s is reasonable with the hot standby on physical machines with a dedicated HA cable; keep 3 s or more on virtual machines.

## Maintenance mode

**Operations > Maintenance mode** pauses automatic failover, preemption and health-driven switching on both nodes for 1-24 h chosen in the GUI (default 4 h, it switches itself off; the console and API accept up to 72 h). Split-brain protection stays on. Use it for planned work.

## Pre-flight and dry run

**Operations > Pre-flight** checks both nodes (versions and plugins, HA link, management path, sync health, rule on the HA link, clock skew, saved config) and dry-runs a takeover on the standby (devices creatable, interfaces enabled in the saved config, packages present) without moving traffic. Results are *pass*, *warn* or *fail*.

## Updating

**Operations > Upgrade assistant** updates both nodes without an outage: it updates and reboots the standby, fails over to it, updates and reboots the other node and optionally fails back. It is offered only when OPNsense reports updates (use **Check for updates**), switches maintenance mode on for the duration and stops, leaving maintenance mode on, if anything goes wrong. Do not edit the configuration while it runs.

By hand:

1. On the standby: `hasyncd ctl unlock` (30 minutes, so migrations can write the config), update through the active node's proxy, e.g. `env HTTP_PROXY=http://<active HA IP>:7691 HTTPS_PROXY=http://<active HA IP>:7691 pkg upgrade -y` (`opnsense-update` the same way for a new base/kernel), reboot. It comes back as a silent, locked standby.
2. While the versions differ, both nodes show a banner; config sync is refused across core versions and resumes by itself when they match. Failover keeps working. A graceful failover is refused while the active node holds changes the peer lacks; forcing it saves the old config to `/conf/activestandby/unreplicated-<time>.xml`.
3. Fail over to the updated node and update the other one the same way.

Always keep a config backup or hypervisor snapshot first.

## Preemption, health, probes, fencing

Settings tab (some under *advanced*):

- **Preemption**: the primary takes the active role back after a delay, using the normal graceful failover with its guard. Off by default.
- **Health monitoring**: carrier of chosen interfaces (confirmed by the standby's link before switching), packet filter and IPsec services (restarted first), **probe targets** (the active node pings addresses you choose; a switch happens only if none answers). A hold time applies, no switching for 30 s after a promote, at most two health switches per 30 minutes and a 5-minute cool-down.
- **Management probe targets**: optional addresses (the management switch or router) that both nodes ping from their management address. They give the standby, whose data interfaces are down, proof that its own network side is alive: a standby that cannot reach them does not take over, and an active node does not hand over for a health problem when the standby cannot either. Use addresses that answer ping.
- **Failure detection time**: lower is faster and riskier on a flaky link. Same value on both nodes.
- **Fencing command**: a root-owned absolute path run before an automatic promotion when the peer went silent. Failure blocks the promotion.
- **Never take over by itself without fencing** (off by default): the standby refuses an automatic takeover caused by a silent peer unless a fencing command is configured and succeeds. Use it where a double active node is worse than a longer outage; a real crash of the active firewall then needs a manual takeover when no fencing exists. A deliberate failover is not affected.
- **GUI logins across a failover** and **virtual MAC** are on by default.

### Which failures cause a failover

The heartbeat only proves that the other node's daemon and the HA link work. A broken network card on the active node leaves the heartbeat intact, so it is detected only by what is monitored:

| What breaks on the active node | Detected by | Failover |
|---|---|---|
| Power loss, crash, hang of the whole node | silence on every heartbeat path | yes, after the detection time, if the standby can show it is not the isolated one (and the fencing command succeeds, if configured) |
| A monitored interface loses its link: a pulled cable, a dead card, **all ports of a LAGG** (the VLANs on it lose their carrier too) | *Monitored interfaces* | yes, after the hold time (default 6 s), unless the standby **reads** no link on its own port either |
| One port of a LAGG fails but the LAGG keeps another one | nothing: the LAGG is still up | no, by design |
| The card has link but passes no traffic, or the upstream is down | *Probe targets* (the upstream gateway, for example) | yes, if configured |
| Packet filter or IPsec stopped | *Health: services* | yes, after one restart attempt, if enabled |
| An interface that is not in *Monitored interfaces* | nothing | **no**: with nothing selected a dead card causes no failover. Pre-flight warns about it |
| Only the HA link or only the management path is lost | the other path still carries the heartbeat | no |
| Both paths between the nodes lost while each sees its own links up | each node sees a silent peer | the standby promotes: a double active until a path returns (use a fencing command if this matters, see the audit) |

The standby keeps its data ports down, so it usually **cannot read** its link; that is not treated as "no link". Only a link it could read and found down stops the switch (the outage would only move). Switches are limited to two in 30 minutes with a 5 minute cool-down, so a flapping card cannot make the roles ping-pong.

For a layout where every network is a VLAN on a LAGG, select the VLAN interfaces (WAN, LAN, ...) as monitored interfaces.

## Monitoring

- **Notifications**: always to syslog; webhook (JSON) at a chosen level; e-mail through Monit by adding a custom service that runs `/usr/local/opnsense/scripts/OPNsense/ActiveStandby/monit_check.sh` with the test "status != 0" (exit 0 healthy, 1 warning, 2 critical).
- **Prometheus**: scrape `/api/activestandby/metrics/index` on each node with an API key (basic auth). Give the key the privilege *System: Active/Standby HA (read-only status and metrics)*.
- **Failover history** is on the Operations tab; the dashboard widget shows state and sync.
- **Audit**: manual actions appear in the event log and syslog as `AUDIT <action>:by:<user>:from:<ip>`.

## Manual operations

- **Failover now**: graceful switch from either node. Forcing a role is console-only (`hasyncd ctl promote --force`, `demote`): it risks split-brain. The only exception is the emergency takeover offered on a standby whose active node is silent.
- A full resync (`hasyncd ctl resync`) pushes the complete config again; a damaged saved config on the standby triggers it automatically.
- **Break pair**: both nodes become standalone. A former standby keeps its disabled data interfaces: restore a backup (`/conf/backup/`) or `/conf/activestandby/active.xml` and review the addresses before connecting it.
- Console: `hasyncd ctl status|events|failover|promote [--force]|demote|resync|break|maintenance on|off|preflight|history|health|metrics|upgrade start|unlock|lock` (forced promote, demote and resync are console-only).

## Files and logs

- `/var/log/hasyncd.log`, events in the GUI (`/conf/activestandby/events.log`), state in `/conf/activestandby/state.json` (+ `.bak`).

## Troubleshooting

| Symptom | Check |
|---|---|
| *Waiting for the second firewall* never ends | HA cable and addresses; the plugin installed and enabled on the second node; the kit file restored on it; firewall rule on the HA link; Pre-flight on the primary |
| Pairing refused with a list of differences | OPNsense version, kernel, NIC names and plugin versions must match |
| *peer silent but automatic promotion held back* | this node cannot prove it is not the isolated one: fix its HA/management link, or promote manually if the peer is down |
| Failover error *the peer did not confirm the takeover ... stays standby on purpose* | the answer of the peer was lost and its state could not be verified; the old node deliberately did not take the role back (it could create two active nodes). Check the peer (`hasyncd ctl status` on it): if it is active, nothing to do; if nobody is active, promote one node manually |
| Status shows *degraded*, *pfsync_problem* or *package_diff* | degraded: the node is active but something failed during the takeover (read the event log); pfsync: states are not being replicated, sessions may not survive a failover; package_diff: the nodes have different plugins (nothing is removed automatically) |
| Sync *behind*/*failed* | read the error in Status; *Full resync*; versions must match |
| After failover traffic fails on Hyper-V or other virtual NICs | enable *Promiscuous mode on data interfaces*; use virtual (locally administered) MACs |
| ARP works and the interface looks fine, but no unicast packet reaches a VLAN (ping to the firewall or through it fails) | a VLAN with a MAC different from its parent only receives with promiscuous mode on the parent (seen on the Intel X710). 0.9.1 makes a VLAN share its parent's MAC and sets promiscuous mode when it does not; check `ifconfig vlanN` against its parent, and enable *Promiscuous mode on data interfaces* on older versions |
| Still asked to log in after a failover | *Keep GUI logins across a failover* must be on, on the active node, and the nodes must serve the same certificate (it is part of the replicated configuration) |
| Standby GUI says "saved" but nothing changes | expected: the standby's configuration is locked |

## Requirements and limitations in production

**Network**
- **Add a management interface.** Over a direct HA cable alone, the standby cannot tell "the other firewall lost power" from "my own cable is unplugged", so it does **not** take over by itself after a power loss unless you allow that under Settings > Safety (risk: both firewalls active if only the cable is cut). A management interface, ideally with *management probe targets*, removes the ambiguity. The wizard and the pre-flight warn about it.
- The HA link should be a dedicated NIC with the same MTU on both sides. A management or HA interface may be a VLAN on a shared trunk (its parent NIC is kept up on the standby).
- **Switch:** one LAG per firewall (or LACP fast), no port security or sticky MAC (the shared MAC moves between ports at a failover), no MAC-flap protection on the data ports, spanning-tree edge (portfast) on them (with RSTP auto-detection a port whose link returns waits about 3 s before it forwards: measured as 3-4 s of extra outage after every takeover). After a takeover the link of a real NIC needs one to a few seconds to come up: the takeover waits for the carrier (up to 8 s) before it announces the MAC, and repeats the announcement for a few seconds.
- **Detection time:** the default is 3 s. Use 5 s or more on production hardware under real load; 1-2 s is for dedicated links and quiet firewalls.

**Supported and not supported**
- Static IPv4 on the shared interfaces. VLANs and LAGG are supported (lab-tested), bridges and tunnel devices are not verified.
- **PPPoE is not supported**: the old active firewall keeps its session while the new one dials, and many providers refuse a second login. **DHCP on the WAN is best effort** (the lease is per node; the new active firewall asks again). **IPv6 is not supported for failover** (no neighbour announcements, router-advertisement and prefix-delegation state is per node).
- OpenVPN instances, WireGuard and IPsec follow the active role. Services that bind to interface addresses (NTP, captive portal, Suricata, HAProxy, Dynamic DNS) are restarted after a takeover when they are installed. Since 1.0.1 every other service that OPNsense lists as enabled and that is not running is started too (SNMP, node exporter, LLDP, ...), except IPsec, OpenVPN, WireGuard, the gateway monitor, DHCP and DNS, which have their own handling.
- Do not restore a configuration backup on the active firewall that lacks this plugin's section: the demote then refuses (it would cut the HA link). Restore the settings first. To change the standby's configuration use `hasyncd ctl unlock`.
- Monit's default checks will alert on the standby (IPsec and the gateway monitor are stopped by design): exclude them there.
- A single firewall that reboots (for example after a power cut) is down for the whole boot, plus 10 s (primary) or 30 s (secondary) before it promotes.
- Hiding the core "High Availability" menu is cosmetic; the pages stay reachable by URL.

**Before production, on the real hardware** (see `lab/hw-probe.sh`): link behaviour of the `ixl` NICs when the standby takes them down (does the switch see link loss?), a failover with a traffic generator, a power cut of the active firewall, a cold start of both firewalls together, a VLAN-specific shared MAC with promiscuous mode off (done, see section J of the validation: it found a defect), and a promote with your real number of VLANs (done: 4.4-4.9 s with 21 interfaces).

## Switch checklist

The measurements in [validation](validation.md) were made with a MikroTik CRS305 (RouterOS 7.12). What follows is the list to check on the switch of a real installation; the points for Juniper EX4600 and Meraki MS come from the general behaviour of those platforms and **have not been tested with this plugin**: validate them on your equipment (a graceful failover with a client pinging every 20 ms is enough to see a problem).

**All switches, all modes**
- **Edge / portfast on the ports of the firewalls** (RSTP: `edge`; Junos: `edge` under `protocols rstp interface`; Meraki: RSTP on with the port as an edge port, and BPDU guard only if no switch is ever cabled there). Without it a port whose link comes back waits about 3 s before forwarding: measured as 3-4 s of extra outage after every takeover.
- **No port security, sticky MAC, MAC limit or MAC-move limit** on those ports: the shared MAC moves from one port to the other at every failover.
- **Dynamic ARP inspection / DHCP snooping**: the ports of the firewalls must be *trusted*, or the gratuitous ARP that announces the takeover is dropped.
- **Storm control** on broadcast must leave room for a few gratuitous ARP per interface (20 VLANs: a burst of about 60 frames).
- No MAC aging shorter than a few minutes on those VLANs (an unknown unicast is flooded to both firewalls; the standby does not answer, but the traffic doubles).

**LACP** (one LAG per firewall, never one LAG across both)
- **Juniper EX4600**: one `ae` interface per firewall (`aeX { aggregated-ether-options { lacp { active; periodic fast; } } }`, member interfaces `gigether-options { 802.3ad aeX; }`), the two `ae` independent, not part of one MC-LAG or ESI-LAG. Each `ae` forms its own bundle, so both partners reporting the same system id (the shared MAC, *Hot standby with an LACP LAGG = Shared*) is no problem there.
- **Meraki MS**: aggregate the ports of each firewall as its own link aggregation in the dashboard (LACP is what Meraki aggregates use), one aggregate per firewall. Check that no "access policy / sticky MAC allow list" is applied to those ports.
- **Not supported with *Shared***: a switch that forms aggregates by itself from the partner system id (some dynamic LACP implementations, bonding on a Linux switch side) would merge the two firewalls into one LAG. Keep *Off* there: the LACP LAGG then uses the classic takeover.
- Use the fast LACP rate on both sides (`periodic fast` / the *fast timeout* of the LAGG): a classic takeover with LACP waits for the bundle (12.5 s measured with a 1 s rate on both sides).

**What to look at when it does not behave**: the host table of the switch for the shared MAC during a failover (it must move to the other port within the announcement), the link log of the ports (a port that bounces at a takeover is a classic-mode port or a NIC that re-initialises), the LACP state of both bundles.

## Production pilot checklist

For a first installation in a small data centre. Keep the **classic mode** until every line is done once; the hot standby is opt-in afterwards.

1. Two machines with the same NIC names, the same OPNsense (26.7 or newer) and the same plugins; HA link on a dedicated cable (a direct cable, not a switch), a management interface on a network that does not depend on the data switch.
2. The [switch checklist](#switch-checklist) done on the ports of both firewalls (edge ports, no port security, trusted ARP, one static port-channel per firewall for LACP).
3. Install the package on both, run the setup wizard, pair; *Pre-flight* must show no failure and you must have read every warning (monitored interfaces, probe targets, fencing, management probe).
4. Set **Monitored interfaces** (a dead card or LAGG does not cause a failover otherwise) and, ideally, probe targets such as the upstream gateway.
5. In a maintenance window with a client pinging every 20 ms: *Failover now* both ways, then power off the active node (a real power cut, not only a shutdown), then unplug one data cable of the active node. Expect, in the classic mode, 1-5 s (LACP: up to 12 s); read the history (`hasyncd ctl history`) for the phases.
6. Check that the standby takes over after a cold start of both machines and after one machine alone; that a configuration change on the active node shows *in sync*; that the standby's GUI is reachable on its management address.
7. Only then, optionally, the hot standby (section above), with the same tests again, and a shorter detection time if you want it (see [fast-ha](fast-ha.md#detection-time-of-1-second)).
8. Keep a recent configuration backup of both nodes, `hasyncd ctl break` and the break-pair procedure at hand, and watch the event log during the first weeks ("peer lost", "health failure", "transmitted ... while silent", "ruleset mirror").
