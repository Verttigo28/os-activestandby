# Operations

## Daily use

- **Edit the configuration on the active node only.** It replicates to the standby within about a second. The standby's configuration is locked (immutable `config.xml`): changes made there do not reach disk, and a banner in the status bar says "STANDBY" on every page.
- The *Status* page shows both nodes, sync state, failover protection and recent events. On a standby it shows only a single button, **Make this firewall the active one**; if the active node does not answer, an emergency option appears.
- Node-local and never replicated: hostname, the HA link and management interface definitions, this plugin's settings, `/conf/activestandby/*`. Everything else in `config.xml` is replicated, plus the list of installed `os-*` plugins, DHCP leases, firewall states and GUI sessions.
- Plugin settings are per node. Change them on the active node; make a node active first if you need to edit them on the other one.

## What happens when

| Event | Behaviour |
|---|---|
| Planned failover (button, health switch, preemption, upgrade) | about 0.7 s of lost traffic in the lab, 1.3-1.9 s for the whole operation |
| Active node loses power or crashes | the standby promotes after the detection time (default 2 s, 1-30) of silence on **all** heartbeat paths, plus about 0.5 s: about 1.5 s with a 1 s detection time |
| Standby cannot prove it is not the isolated node (its management/HA carrier is down) | **no** automatic promotion; event "held back"; promote manually if the peer is really down |
| Both nodes up, nobody active (cold start) | the primary promotes about 5 s after the daemons start, the secondary only after about 30 s |
| A node reboots | always boots as standby, then follows the rules above; failback is manual unless *preemption* is on |
| Both nodes active (split-brain) | higher epoch wins (tie: primary); the other demotes within about a second |
| Configuration saved on the active node | pushed as a numbered revision within about a second; the standby merges, reloads pf only, acknowledges; on any error it rolls back and shows the error |
| Standby without WAN | plugin installs go through a restricted proxy on the active node; the standby clock follows the active node |

Real NICs add link-up time (typically 1-3 s for copper) that the lab cannot show.

## Maintenance mode

**Operations > Maintenance mode** pauses automatic failover, preemption and health-driven switching on both nodes for up to 24 h (default 4 h, it switches itself off). Split-brain protection stays on. Use it for planned work.

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
- **Failure detection time**: lower is faster and riskier on a flaky link. Same value on both nodes.
- **Fencing command**: a root-owned absolute path run before an automatic promotion when the peer went silent. Failure blocks the promotion.
- **GUI logins across a failover** and **virtual MAC** are on by default.

## Monitoring

- **Notifications**: always to syslog; webhook (JSON) at a chosen level; e-mail through Monit by adding a custom service that runs `/usr/local/opnsense/scripts/OPNsense/ActiveStandby/monit_check.sh` with the test "status != 0" (exit 0 healthy, 1 warning, 2 critical).
- **Prometheus**: scrape `/api/activestandby/metrics/index` on each node with an API key (basic auth). Give the key the read-only privilege.
- **Failover history** is on the Operations tab; the dashboard widget shows state and sync.
- **Audit**: manual actions appear in the event log and syslog as `AUDIT <action>:by:<user>:from:<ip>`.

## Manual operations

- **Failover now**: graceful switch from either node. Forcing a role is console-only (`hasyncd ctl promote --force`, `demote`): it risks split-brain.
- A full resync (`hasyncd ctl resync`) pushes the complete config again; a damaged saved config on the standby triggers it automatically.
- **Break pair**: both nodes become standalone. A former standby keeps its disabled data interfaces: restore a backup (`/conf/backup/`) or `/conf/activestandby/active.xml` and review the addresses before connecting it.
- Console: `hasyncd ctl status|events|failover|promote [--force]|demote|resync|break|maintenance on|off|preflight|history|health|metrics|upgrade start|unlock|lock` (forced promote, demote and resync are console-only)`.

## Files and logs

- `/var/log/hasyncd.log`, events in the GUI (`/conf/activestandby/events.log`), state in `/conf/activestandby/state.json` (+ `.bak`).

## Troubleshooting

| Symptom | Check |
|---|---|
| *Waiting for the second firewall* never ends | HA cable and addresses; the plugin installed and enabled on the second node; the kit file restored on it; firewall rule on the HA link; Pre-flight on the primary |
| Pairing refused with a list of differences | OPNsense version, kernel, NIC names and plugin versions must match |
| *peer silent but automatic promotion held back* | this node cannot prove it is not the isolated one: fix its HA/management link, or promote manually if the peer is down |
| Sync *behind*/*failed* | read the error in Status; *Full resync*; versions must match |
| After failover traffic fails on Hyper-V or other virtual NICs | enable *Promiscuous mode on data interfaces*; use virtual (locally administered) MACs |
| Still asked to log in after a failover | *Keep GUI logins across a failover* must be on, on the active node, and the nodes must serve the same certificate (it is part of the replicated configuration) |
| Standby GUI says "saved" but nothing changes | expected: the standby's configuration is locked |

## Known limitations

- IPv4 only for gratuitous ARP; IPv6 neighbour announcements are not sent.
- Hiding the core "High Availability" menu is cosmetic; the pages stay reachable by URL.
- Intel X710 (`ixl`) behaviour on admin-down and the real boot window must be confirmed with `lab/hw-probe.sh` before production.
- Verified with a real IKEv2/PSK site-to-site tunnel to a third OPNsense node (the tunnel re-establishes through the new active node and carries traffic) and with dpinger following the active role. Not verified: peers other than OPNsense/strongSwan.
