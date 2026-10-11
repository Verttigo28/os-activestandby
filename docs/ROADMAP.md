# Roadmap and advice

Ranked by what I would do first. P1 = I would not call it production-ready without it. P2 = strongly recommended. P3 = nice to have.

## P1 — before production
1. **Real hardware validation** (Supermicro/X710 + a real switch): carrier and LLDP when the port is admin-down, gratuitous-ARP effect on the switch MAC table,
   boot window, failover time with real NICs. Script: `lab/hw-probe.sh`. Also try a failover with a real traffic generator.
2. **Upgrade assistant** (DONE v0.4.0) (see "What happens when you update one node" in docs/OPERATIONS.md): a guided, ordered procedure in the GUI
   (unlock standby, update through the proxy, reboot, verify, fail over, repeat), plus a **maintenance mode** that pauses automatic failover and shows a
   "config changes now are not replicated to the other node" warning. Today the procedure is documented but manual.
3. **Active-node health monitoring** (DONE v0.4.0): demote (or fail over) when the ACTIVE node itself is unhealthy while still answering heartbeats: WAN gateway down while
   the standby's WAN link is up, no carrier on a data interface, filter or IPsec service dead. Needs a way for the standby to verify its own links before
   taking over (carrier read on admin-down NICs is driver dependent, see item 1).
4. **Notifications** (DONE v0.4.0): syslog, e-mail (monit) and webhook on promote/demote, split-brain, sync failure, standby unreachable. The event log exists; nothing alerts yet.
5. **Pre-flight and self-test** (DONE v0.4.0): a button that verifies both nodes (same version/plugins, HA link MTU/latency, mgmt path, sync health, pf rule on the HA link, clock
   skew) and a **dry-run failover** that checks the standby could take over (devices creatable, interfaces enabled in the saved config, packages present)
   without moving traffic.

## P2 — strongly recommended
6. **DHCP lease and captive-portal/session sync** (Kea/dnsmasq leases, captive portal sessions, OpenVPN/IPsec roadwarrior state): today only firewall
   states (pfsync) and the configuration follow the active node.
7. **Fencing hook** (DONE v0.4.0): run an operator script (IPMI, smart PDU) before promoting when the peer is silent, so "both paths dead but the peer is alive" cannot
   become a real split-brain.
8. **Preemption option** (DONE v0.4.0) (primary takes over again after a configurable delay, optionally only inside a maintenance window), default off as today.
9. **Role-based permissions**: separate privileges for "view", "failover", "break pair"; audit entry for every manual action (who, when, from where).
10. **Metrics** (DONE v0.4.0): Prometheus/telegraf endpoint (state, epoch, heartbeat RTT per path, sync revision lag, last failover duration), and a failover history table.
11. **Config drift detector** (DONE v0.4.0): periodic hash comparison of the replicated parts so a silently diverged standby is flagged long before a failover needs it.

## P3 — nice to have
12. Config diff viewer between the nodes' saved configurations; "what will change on failover" preview.
13. (DONE v0.4.2) Selectable **virtual MAC** at pairing (generate a locally administered MAC per interface instead of copying the primary's hardware MAC).
14. Per-service opt-out (e.g. keep an NTP or DNS service running on the standby), and a stricter option to stop everything except the HA stack.
15. Bridge/GRE/GIF/VXLAN and LACP-with-switch failover verification, more VPN types (WireGuard and OpenVPN tested the way IPsec was).
16. Optional encryption/authentication of pfsync (IPsec on the HA link) when the link is not a dedicated cable.
17. Two heartbeat paths over two physical cables, and an optional third-party witness (a ping target) for the isolation guard.
18. IPv6 neighbour announcements (explicitly out of scope today).

## Design choices worth revisiting
- The standby is read-only through an immutable `config.xml`; OPNsense still says "saved" on the standby. A banner or API-level refusal on standby pages would be friendlier.
- Config sync is whole-file and active-to-standby. That keeps behaviour simple and predictable; a merge strategy for edits made on both nodes is deliberately not offered.
- The shared MAC defaults to the primary's hardware MAC; a virtual MAC (item 13) is the safer default for production.
