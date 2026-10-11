# Phase 0 findings (Hyper-V lab, OPNsense 26.7 / FreeBSD 15.1, PHP 8.5, Python 3.13)

Lab: `opn-a` / `opn-b`, 4 vCPU / 4 GB, NICs hn0 WAN (`ha-wan`), hn1 LAN (`ha-lan`), hn2 HA (`ha-link`),
hn3 MGMT (Default Switch, SSH). Tools in `lab/tools` (`hv.ps1` console, `opn.ps1` SSH). Lab root password is lab-only.

## Confirmed
| # | Question | Result |
|---|---|---|
| 1 | Does `<spoofmac>` give both nodes identical MACs? | **Yes.** B (own hw MAC 0b:0b:xx) came up with A's MACs on WAN and LAN purely from replicated config.xml. |
| 2 | Is there a hook on config save? | **Yes.** `Config::save()` logs a syslog `config-event`; configd action `event.config_changed` runs `trigger_config_changed_events.py`, which runs `rc.syshook config` (batched). Dir `/usr/local/etc/rc.syshook.d/config/` does not exist yet, so the plugin creates it (`50-activestandby`). |
| 3 | **R1: does a standby with the active's config transmit at boot?** | **Yes, confirmed.** B sent a gratuitous ARP (who-has 192.168.1.1 tell 192.168.1.1) and IPv6 NS with A's IP+MAC on LAN and WAN ~17 s after reboot. On a real switch this moves the MAC to the standby's port. Must be prevented. |
| 4 | Mitigation: standby-local config view with `<enable>` removed on data interfaces | **Works.** After reboot hn0/hn1 had no UP flag, no address, hardware MAC, and A received **zero** frames from them. HA and MGMT came up normally. |
| 5 | Promote cost | `configctl interface reconfigure wan` + `lan`: **1.3 s** for both; MAC from spoofmac applied, IPs assigned, UP. (`configctl interface reconfigure` without argument is a no-op.) Services, routes, filter reload, strongSwan, GARP are extra; total budget 10 s is realistic. |
| 6 | Versions | OPNsense 26.7, FreeBSD 15.1-RELEASE-p1, strongSwan 6.0.7, dpinger 3.4, py313-vici. |
| 7 | OPNsense core already has XMLRPC HA (`rc.filter_synchronize`, `ha_reconfigure_backup`) | Unused and not touched by us; it is CARP/VIP oriented. Do not enable alongside. |

## Design change decided by these results
The standby never sees the master's `<enable>1</enable>` on data interfaces. Sync applies master config, then the
**standby-view transform** removes `<enable>` from every non-excluded interface (WAN/LAN/OPTx/VLAN/LAGG that are not HA/MGMT).
Promote = restore the active view (`<enable>` back) + `configctl interface reconfigure <if>` per interface + routes + GARP + services.
Demote = apply standby view + `ifconfig <if> down` + flush addresses. Boot role comes from `/conf/activestandby.role`; an `early`
syshook applies the standby view **before** `interfaces_configure()` runs. The 1 s enforcer stays as a safety net for VLAN/LAGG parents
that OPNsense raises on its own.

## Still open (Phase 0 leftovers)
- VLAN/LAGG parent behaviour when only the child is disabled (needs a VLAN in the lab).
- pfsync state install with data interfaces down (do in Phase D lab).
- X710 (`ixl`) carrier/LLDP on admin down and real boot window: run `lab/hw-probe.sh ixl0` on a spare Supermicro and send me the output.
