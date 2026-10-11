# Roadmap

Open items, most valuable first. Done items live in the [changelog](../CHANGELOG.md).

## Before production

1. **Hardware validation** on the target machines (for example Supermicro with Intel X710) and a real switch: carrier and LLDP behaviour when a port is administratively down, the switch's MAC-table reaction to the gratuitous ARP, boot window, real failover time with link-up, a traffic generator across a failover. Script: `lab/hw-probe.sh`.
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
