# Must-have features and their test status

Legend: **PASS** = verified in the Hyper-V lab (OPNsense 26.7, 2 cluster nodes + 1 remote IPsec peer); **HW** = needs the real Supermicro/X710 pair;
**N/A** = out of scope by decision. IPv6 is out of scope. Evidence is in docs/PLAN.md (progress log) and docs/OPERATIONS.md.

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
