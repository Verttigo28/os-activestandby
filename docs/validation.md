# Validation

What was tested, how, and what still needs hardware. **PASS** = verified in the Hyper-V lab (OPNsense 26.7, two cluster nodes and one remote IPsec peer); **HW** = needs the real Supermicro/X710 pair; **N/A** = out of scope by decision (IPv6). Timing numbers are lab numbers: real NICs add link-up time.

## A. Core HA behaviour
| # | Must have | Status | How it was tested |
|---|---|---|---|
| 1 | One IP per interface, same IP and MAC on both nodes, no CARP VIPs | PASS | Both nodes show identical MAC+IP on WAN/LAN/VLAN after promote; frames from a booting standby captured on the active node: none. |
| 2 | Standby keeps all data interfaces down, no addresses, only HA (+mgmt) up | PASS | Standby view of config.xml, boot hook, 1 s enforcer; verified after clean reboot, hard power-off, failed promote. |
| 3 | Automatic failover under 10 s | PASS | Peer power-off: new active ~6 s; TCP-through-VPN survived with 10 s worst case incl. IPsec re-negotiation. |
| 4 | Graceful switchover and manual failback | PASS | ~3-4 s from either node (GUI button and API); rollback if the peer cannot take over (seen working when a VLAN device was missing). |
| 5 | Rebooted node never comes up active next to its peer | PASS | Boots standby before interfaces are configured; election rules after boot (primary 5 s, secondary 30 s). |
| 6 | Split-brain protection | PASS | Forced dual-active: higher epoch wins, loser demotes in ~3 s. Isolated standby (no carrier on HA and mgmt) refuses to promote. |
| 7 | Second heartbeat path (mgmt) optional | PASS | HA-only and HA+mgmt paths both tested; HA-only needs carrier up (or explicit override). |
| 8 | Crash safety of HA state | PASS | State fsynced + backup copy; unreadable state fails safe to silent standby; damaged saved config self-heals by resync. |

## B. Configuration sync
| # | Must have | Status | How it was tested |
|---|---|---|---|
| 9 | Full config.xml replicated incl. interfaces, VLANs, rules, NAT, VPN, users, certs, dashboards | PASS | Aliases, rules, users/API keys, IPsec connections, gateways, VLAN + interface assignment, dashboard layout all arrived on the standby. |
| 10 | Node-local exclusions (hostname, HA/mgmt interface, plugin settings) | PASS | Standby kept its own HA IP across syncs and promotes. |
| 11 | Numbered revisions, ack, rollback on standby failure, error in GUI | PASS | Broken merge step: files byte-identical afterwards, error on both nodes, automatic retry and recovery. |
| 12 | Standby never brings LAN/WAN up while applying | PASS | Restricted apply (pf reload only) + enforcer; checked after every sync in the lab. |
| 13 | Sync status in GUI: in sync / N behind / failed + last error; full resync | PASS | Status page, dashboard widget, API. |
| 14 | Standby is read-only (no config edits through GUI/API) | PASS | config.xml immutable on standby; edit attempt does not persist; sync and promote still work; `ctl unlock` for updates. |
| 15 | Interfaces added later get the shared MAC automatically | PASS | New VLAN interface pinned and synced without manual steps (found and fixed during this test run). |
| 16 | Virtual devices (VLAN/LAGG/bridge/...) exist on the node that becomes active | PASS | Promote creates them (OPNsense PHP functions, not the GUI-todo based configd actions) before bringing interfaces up. VLAN and LAGG (failover proto, MAC shared with the first member) failover tested; bridge/GRE/GIF use the same helper, not separately tested. |
| 17 | A later config apply on the active node cannot undo the shared MAC | PASS | `rc.reload_all` and per-interface reconfigure on the active node: MACs unchanged. |

## C. Services and traffic
| # | Must have | Status | How it was tested |
|---|---|---|---|
| 18 | strongSwan stopped on standby, started on promote | PASS | Both directions, also if the boot sequence starts it on a standby (enforcer stops it). |
| 19 | IPsec tunnel re-established after failover | PASS | Real IKEv2/PSK tunnel to a third OPNsense: 5.9 s (graceful), 6.2 s (power-off). |
| 20 | **Traffic flowing through the VPN during failover** | PASS | LAN client behind the cluster, long-lived TCP stream across cluster + tunnel: graceful 5 s, failback 6 s, power-off 10 s silence; **0 sequence breaks, session never reset**. |
| 21 | Established firewall sessions survive (pfsync) | PASS | States for forwarded flows exist on the standby; the TCP stream above crosses failovers without resets. |
| 22 | dpinger/gateway monitoring only on the active node | PASS | Running on active, absent on standby, restarted on promote; no alarms on standby. |
| 23 | Standby updates/plugins/time without WAN | PASS | Plugin replicated through the restricted proxy; proxy refuses other hosts and non-peers; `fetch`/`pkg` honour it; standby clock follows the active node. |
| 24 | Package list replicated before applying config | PASS | `os-helloworld` installed on the active node appeared on the standby in ~6 s. |

## D. Management and safety
| # | Must have | Status | How it was tested |
|---|---|---|---|
| 25 | Setup wizard (interfaces, role, pairing) | PASS | Driven end to end in the real GUI. |
| 26 | Pairing refuses non-identical nodes; token + pinned certificates | PASS | Version/NIC/plugin mismatch refused (extra NIC test); wrong token rejected; stranger certificate rejected; Go unit tests. |
| 27 | Status page, event log, dashboard widget | PASS | GUI and widget verified in Chrome (widget added to the dashboard and persisted). |
| 28 | Built-in CARP/XMLRPC HA menu hidden while enabled | PASS | Menu entry removed; pages remain reachable by URL (accepted). |
| 29 | Documented update and recovery procedures | PASS | docs/OPERATIONS.md, docs/BUILD.md (unlock, update standby first, break pair). |

## E. Not verifiable here
| # | Item | Status |
|---|---|---|
| 30 | Intel X710 (ixl): carrier on admin-down, LLDP, real boot window | **HW** (`lab/hw-probe.sh`) |
| 31 | Gratuitous ARP effect on a real switch MAC table | **HW** |
| 32 | Package build with the OPNsense plugins framework | open (layout follows net/haproxy, see docs/BUILD.md) |
| 33 | Bridge/GRE/GIF devices, LACP LAGG with a real partner, non-OPNsense VPN peers | not tested |
| 34 | IPv6 | N/A |

## Lab-only behaviours worth knowing
- Hyper-V (`hn` driver) only receives unicast frames for the vNIC's configured MAC unless the NIC is promiscuous: enable the **Promiscuous mode on data
  interfaces** setting there. Real NICs reprogram their MAC filter and do not need it. Hyper-V also never relearns a MAC it considers owned by another vNIC,
  so the lab uses locally administered shared MACs (02:00:5e:...) instead of the primary's hardware MAC; this is also a good production choice
  (set the MAC address on the interfaces before pairing, or edit `spoofmac` afterwards).
- OPNsense `reply-to` on a DHCP management interface breaks node-to-node traffic on the same subnet; WAN `blockpriv` must be off for a private-address lab peer.

## F. Update and packaging (verified in a second round)
| # | Must have | Status | How it was tested |
|---|---|---|---|
| 35 | Update one node at a time (OPNsense 26.7 -> 26.7.6_3) | PASS | Standby updated through the proxy and rebooted; failover to the updated node; other node updated; versions converged and sync resumed by itself. Snapshots were taken first. |
| 36 | Version mismatch is visible and sync refuses across core versions with a clear message | PASS | Status banner, widget row, event log; refused revision named both versions. |
| 37 | Edits made during the mismatch window cannot be lost silently | PASS | Graceful failover refused while the active node holds unreplicated edits; forced failover saved them to `unreplicated-<time>.xml` and logged it. |
| 38 | After every role change both nodes converge on the new active node's config | PASS | New active forces a full push; found because an old edit survived on the other node after a failover cycle. |
| 39 | VLAN/LAGG devices and member NICs stay down while a standby boots | PASS (functional) | Boot guard detached from the early hook; the daemon's enforcer found nothing to correct on the next boot. Not yet verified at packet level on a real switch. |
| 40 | Installable package: build, install on a clean node, uninstall | PASS | `build/build.ps1` builds `os-activestandby-0.3.0.pkg` (pkg create on a FreeBSD node); `pkg add` on a node that never had the plugin: files, modes, configd actions, settings API live; `pkg delete` leaves nothing behind. |
| 41 | CI that tests, cross-builds and publishes the package on tags | prepared | `.github/workflows/ci.yml`; first run happens when the repository is on GitHub. |
| 42 | Secondary kit: the primary builds the second firewall's config file; restoring it there pairs the nodes automatically | PASS | Lab: break pair, build kit on A, restore on B, reboot B: A paired with B within ~25 s, sync in_sync, pre-flight pass. The kit is the silent standby view with the secondary's own hostname/HA/mgmt address and a 48 h one-time token; the token is removed again on pairing. |
| 43 | Manual failover under 1.5 s outage (was ~6 s) | PASS | Interfaces are configured in parallel (own php process each, parents before VLAN/LAGG), routing/DNS/DHCP restart once, pf reload and services run side by side, DNS/DHCP restart no longer blocks. Measured with a 20 ms ping from the PC: 1.1–1.5 s. |
| 44 | Standby never elects itself while the peer is mid-takeover | PASS | Heartbeat carries a `busy` flag; found when a 4.9 s takeover outlasted the 5 s election timer and caused a double promotion. |
| 45 | Planned failover 0.65–0.85 s of lost traffic; power cut of the active node 1.5 s (detection 1 s + takeover 0.5 s) | PASS (lab) | The old active node tells the peer to promote the moment its interfaces are down, the device script is skipped when the VLAN/LAGG devices already exist, new setting "Failure detection time" (default 2 s, 1–30). Real NIC link-up time comes on top. |
| 46 | Probe targets: the active node pings configured addresses and the roles switch when none answers | built, partly tested | Same guard rails as the other health checks (hold time, standby must have link, anti ping-pong). Only the setting path was exercised in the lab. |
| 47 | Audit trail: every manual action (failover, promote, break, maintenance, kit, upgrade ...) is logged with user and source address | PASS | HA event log and syslog (`activestandby audit ...`). |
| 48 | Hardening: saved full configs 0600, only the five newest "unreplicated" safety copies kept, deploy no longer leaves world-writable script directories | PASS | Found while reviewing permissions on the lab nodes (the lab deploy tar made 777 directories under the root-run script directory; the package itself was already 0755). |
| 49 | Pairing: the hello is authenticated with the token too (nothing about the node is disclosed before), the confirm goes over a pinned connection, per-source lockout, bounded state, server timeouts | PASS | Security review finding (a man-in-the-middle on the HA link could relay the hello and answer the confirm with its own certificate). Unit tests plus a lab pairing through the secondary kit. |
| 50 | Standby banner in the OPNsense status bar on every page | PASS (status class loads and reports) | `ActiveStandbyStatus` reads a world-readable role marker written by the daemon. Not seen in a browser. |
| 51 | Virtual MAC by default (generated `02:xx:...`, setting "Use a virtual MAC address") | PASS (lab) | New shared interfaces get a generated MAC; the standby/new active node uses it from its next promote (the running primary keeps its current MAC until then). |
| 52 | Read-only ACL for status and metrics, audit of settings and service changes, webhook without redirects, package names from the peer validated, update log out of /tmp, hasyncd.json 0600 | PASS | From the security review. Give metrics scrapers the "read-only" privilege. |
| 53 | Guided upgrade only when updates exist: "Check for updates" button and status, the start button is disabled (and the daemon refuses) when OPNsense's last check lists nothing | PASS (lab) | Reads the firmware check result; unchecked state asks for a check first. |
| 54 | Upgrade job robustness: round 2 waits up to a minute for the old node to finish its demote; a handed-over job no longer blocks the next upgrade on the original node | PASS (lab) | Found by review; guided upgrade with failback ran end to end twice. |
| 55 | GUI sessions replicate to the standby, logouts are mirrored, local standby sessions untouched | PASS (files) | Unit tests and a lab check with synthetic session files (copy within 2 s, 0600 root, removed again after deletion on the active node). A real browser login surviving a failover still has to be confirmed by hand. |
| 56 | Races removed (lock-free timestamps), `go test -race` in CI | built | Race detector runs in CI (needs cgo, not available on the lab workstation). |

## G. Failover safety (0.7.0): procedure for two OPNsense nodes

Unit tests with fakes cover the decision logic (`docs/audit.md`). The following must be run on two real or virtual OPNsense nodes before relying on the new behaviour. Never run them against a production pair. Use a client behind the cluster and `lab/failover-bench.sh` (several runs, keep median, P95 and maximum), and watch `hasyncd ctl status`, `hasyncd ctl events` and `hasyncd ctl history` on both nodes.

| # | Scenario | How | Expected |
|---|---|---|---|
| G1 | Graceful failover | `hasyncd ctl failover` on the active node, both directions | one node active at the end; no gratuitous overlap seen on the client; history shows the phases |
| G2 | Lost answer to "promote" | start a failover and, for the first 3 s only, drop the HA traffic from the peer to the active node (a temporary pf rule on the peer or a switch ACL, removed by a timer), so the peer receives the request and promotes but its answer is lost | the old node does **not** become active again while the peer is active; event "no confirmation from the peer"; after the peer answers, one active node |
| G3 | Lost request | drop HA traffic towards the peer before a failover | failover reports an error; the peer confirmed it never started; the old node is active again; no second active node |
| G4 | Peer unreachable during the check | cut the HA link and the management path right after the failover starts | the old node stays standby and says why; the standby side promotes by the usual rules (silent peer, isolation guard, fencing) |
| G5 | Split-brain, immediate | with `auto_failover` off, force both nodes active (`hasyncd ctl promote --force=true` on the standby) | the loser demotes within about two heartbeat intervals, also in maintenance mode, also with automatic failover off |
| G6 | pf failure on promotion | on the standby break the ruleset (for example a syntax error in a rule file used by `configctl filter reload`) and fail over | the promotion fails with "packet filter", the standby stays silent (interfaces down), the old node takes its role back after the confirmation |
| G7 | pfsync | on a node run `ifconfig pfsync0 -syncdev` | within 10 s `pfsync_problem` appears, the daemon repairs it, an event records both |
| G8 | Plugin versions | install a plugin on the standby only, or at another version | `package_diff` shows it; nothing is uninstalled automatically |
| G9 | Reboot during a failover | reboot the new active node while promoting | it comes back as standby; the other node follows the election rules; never two active nodes |
| G10 | Loss and latency | `dummynet` or a switch impairing 30-50 % loss for less than the detection time, then more than it | no failover for the short loss; failover after the detection time |
| G11 | Returning node | power the old active node off for an hour, power it on | it boots silent, becomes standby, does not take the role back (unless preemption is on) |

Not covered by software or by these steps: partitions that cut both paths between the nodes while both see their own carrier (needs fencing), switch MAC-table behaviour, link-up time of real NICs (see `docs/fast-ha.md`).

## H. Warm standby (experimental): hardware procedure

Never run on real hardware yet; everything below is open. Use two nodes with the same NIC model as production and the production switch (or a switch of the same family). Set *Standby mode* to *Warm* on the active node, wait for the restart, then run `hasyncd ctl preflight` and `hasyncd ctl status` on both nodes (`warm.ready` true on the standby, `peer_warm: ready` seen from the active node). Repeat every measurement in Classic mode for comparison; keep median, P95 and maximum (`lab/failover-bench.sh`, `hasyncd ctl history` for the phases).

| # | Question | How | Expected / decides |
|---|---|---|---|
| H1 | Does the standby stay silent? | capture on the standby port and on the active node: no ARP/ND/RA/DHCP/LLDP-with-shared-MAC from the standby, only its own MAC | only frames with the NIC's own MAC |
| H2 | Does the link stay up at takeover? | watch the carrier of the standby port and the switch port during a failover | no link-down event; if the driver drops the link when the MAC changes, warm mode gives no gain on that NIC |
| H3 | Does the NIC receive for the new MAC? | ping the shared address right after the takeover, with *Promiscuous mode* off | replies; if not, the driver keeps its receive filter: enable the promiscuous setting or do not use warm mode on it |
| H4 | MAC table | read the switch's MAC table (or watch for flooding) during the takeover and the stand down | the shared MAC moves to the new port within the gratuitous ARP window; no MAC-flap/port-security log |
| H5 | Speed | failover 10+ times each way, Classic and Warm | gain per phase (carrier and interfaces), outage seen by the client |
| H6 | Stand down | after a failover check the old active node: own MAC, no address, link up, IPv6 off | `ifconfig <if>` shows `ether` = permanent MAC, no `inet`, `nd6 options` with IFDISABLED |
| H7 | Fast path failure | block the MAC change (driver that refuses, or `chflags`/policy), or break `ifconfig` on the standby | event "warm takeover not possible", classic takeover completes, no address left behind |
| H8 | Not ready standby | unplug a warm port on the standby, then fail over | `warm.ready` false with "no link", takeover uses the classic path and still works |
| H9 | Permanent MAC memory | reboot the active node, fail over, stand down: check `hwmacs.json` and that the port gets its own MAC back | own MAC restored; on a driver without `hwaddr` the memory is what makes it possible |
| H10 | VLANs | warm VLAN on a warm parent, VLAN with an unassigned parent | VLAN device follows or keeps its MAC as planned; both ports up |
| H11 | OPNsense state after the fast path | GUI interface overview, gateway monitoring, `rc.newwanip`-dependent services (dyndns, IPsec binding) after a warm takeover | decides whether a reconcile step is needed |
| H12 | Split-brain, reboot and loss scenarios of section G | repeat G1-G11 in warm mode | identical results; never two ports with the shared MAC |

## I. Sandbox: the real daemons against each other on one FreeBSD machine (0.9.0)

Run on a Supermicro machine (OPNsense 26.7, FreeBSD 15.1) with `lab/sandbox`: two vnet jails are the nodes, `epair` interfaces and bridges the switches, a third jail the client; the daemons are the FreeBSD build of this repository, paired over mutual TLS; OPNsense's PHP and `configctl`/`pfctl`/`pluginctl` are replaced by stubs and shims (pf and the filter reload can be made to fail, a carrier can be made to disappear). A detector samples both nodes every 100 ms; two consecutive samples with the shared MAC and address on both nodes, interface up, count as a double active. **PASS** below means: ran in both the classic and the warm mode unless stated, final results.

| # | Scenario | Result | Measured |
|---|---|---|---|
| 1 | Pairing, steady state | PASS | A master and active, B standby; heartbeats alive on both paths; configuration in sync; the standby holds the full configuration and its own hostname; classic: its data ports are down; warm: up, own MAC, no address, IPv6 off, `ready`; the standby sends no frame on its data interface; the data switch learned the shared MAC on A's port |
| 2 | Graceful failover both ways, client pinging at 20 ms | PASS | command 95-117 ms, **2-3 pings lost (40-60 ms)**; the shared MAC moves to the other port of the switch; WAN too; warm: the promotion used the native path |
| 3 | Daemon restarts | PASS | standby restart: no role change; active restart within a second: no failover |
| 4 | Power cut of the active node, then its return | PASS | the standby serves traffic about 3.3 s after the cut (detection 3.25 s), **client outage 2.8 s**; the returning node comes back as standby and does not take the role back; configuration back in sync |
| 5 | Split-brain forced on purpose | PASS | resolved in 1.5-1.8 s, overlap 1.4-1.5 s, the loser reports it |
| 6 | pf not enabled / filter reload failing at promotion | PASS | the promotion is refused, the node returns to standby with nothing of the shared identity left, the old active node takes its role back **after the peer's confirmation**; the reason is in the event log |
| 7 | Warm: standby with no link, port not silent at takeover | PASS (warm) | `not ready (no link)` seen by both nodes; a port that is no longer silent at takeover makes the fast path refuse, the classic path completes |
| 8 | The peer dies during a hand-over | PASS | the old node does not roll back; the survivor serves traffic again after **18.3-18.6 s** (54 s before 0.9.0) |
| 9 | Configuration change on the active node | PASS | replicated, in sync again, the standby keeps its own hostname |
| 10 | HA path lost, management path lost, standby that cannot prove it is not isolated | PASS | no role change with one path lost; the isolated standby does not promote and says why |
| 11 | Both paths between the nodes cut, links up (symmetric partition) | PASS (documents the limit) | **the standby promotes after 2.9-3.5 s: about 3 s of double active**, one owner 1.5 s after the paths return. This is the case that needs fencing |
| 12 | The active daemon dead for 6 s, interfaces still up | PASS | the standby kept still (anti-flapping hold after the previous change); one owner 18 ms after the daemon returned |
| 13 | 20 % packet loss on both heartbeat paths for 25 s | PASS | no failover |
| 14 | A peer frozen for 70 s (SIGSTOP), then released | PASS | never two owners; the old active node serves traffic again after about 48 s (45 s reply timeout); the late request never promotes the peer; a woken daemon no longer promotes on the silence it slept through (found here, fixed in 0.9.0) |
| 15 | A monitored interface of the active node loses its carrier | PASS | the roles switch after **4.7 s**; when the standby cannot read its own port (classic) the switch happens anyway and says so; when it reads no link either (warm) it does not |

Totals of the last runs: classic 24/24 on the changed sections after 89/90 in the full run, warm 23/23 on section 15 after 98/104 in the full run (every failure was a defect of the scenario, understood and fixed; two real defects of the daemon were found by this battery, see [audit](audit.md)). Unit tests on the machine itself: 93 pass natively on FreeBSD, `go test -race` passes, `staticcheck` and `go vet` are clean.

What this does **not** cover: a real NIC driver, a real switch (no CAM aging, no port security), OPNsense's own PHP, LACP with a real partner, timing on hardware. Everything in sections G and H that needs hardware is still open.
