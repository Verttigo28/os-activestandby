# Roadmap

Open items, most valuable first. Done items live in the [changelog](../CHANGELOG.md).

## Before production

1. **Hardware validation, rest of it**: done on two Supermicro machines with X710 and a MikroTik switch (validation section J: failover, crash, dead card, cold start; it found the VLAN MAC defect). Still open: LAGG with two cabled ports and LACP with a real partner, TCP sessions across a failover, a real power cut, the production switch. Script: `lab/hw-probe.sh`.
2. **Standby-side checks**: the standby has no data addresses, so it cannot ping upstream targets itself. Appliance firewalls do, which gives stronger evidence before a takeover.
3. **Verified failover** for bridges, GRE/GIF/VXLAN, LACP with a real switch, and more VPN types (WireGuard and OpenVPN tested like IPsec was).

## Strongly recommended

4. **Role-based permissions** beyond view/full: separate "failover" and "break pair" privileges.
6. **Captive-portal and roaming-VPN session sync.**

## Nice to have

8. Config diff viewer between the nodes, "what will change on failover" preview.
9. Per-service opt-out (keep NTP or DNS running on the standby).
10. Optional encryption of pfsync when the HA link is not a dedicated cable.
11. Two heartbeat cables and an optional third-party witness for the isolation guard.
12. IPv6 neighbour announcements.

## From the failover audit (see `docs/audit.md`)

13. **Fencing in production**: a two-node cluster cannot exclude a double active under a symmetric partition without it. Strongly recommended wherever an IPMI or a smart PDU exists.
14. **Hot standby**: built and measured on two machines (0.9.3). Open: a switch that aggregates LACP dynamically, more than two ports per LAGG, refreshing the standby ruleset from every config sync (so that its first second after a takeover is judged by the current rules), and the same on other NIC families (the transmit watch will say).
15. The warm standby (own MAC) stays as it is; the hot standby replaced the reason to extend it.
16. Epoch on configuration pushes and a nonce on the peer API (defence in depth; the pinned TLS identity already limits both to the peer).
