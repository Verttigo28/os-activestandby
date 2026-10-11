# os-activestandby: operations guide

## Concepts
- **Active** node: owns the shared IPs and MACs, runs IPsec/dpinger/services, is the **configuration source**. Edit configuration here only.
- **Standby** node: all data interfaces down, no addresses, IPsec/dpinger stopped. It holds the full config in `/conf/activestandby/active.xml` and a
  "standby view" in `/conf/config.xml` (data interfaces without `<enable>`), so OPNsense can never bring them up by itself.
- **Primary / secondary** = role chosen at pairing. The primary is active first and wins elections and ties. After a failover the secondary is active, and
  config changes made there replicate back.
- **The standby is read-only.** Its `/conf/config.xml` carries the immutable flag (`schg`), so a change made through the GUI or API of a standby never
  reaches disk (OPNsense reports "saved" but the write fails). Only the daemon lifts the flag for its own writes; the active node is never locked.
  For updates that must write the config (migrations), run `hasyncd ctl unlock` on the standby first (30 minutes, then it relocks; `hasyncd ctl lock` relocks now).
- Node-local, never replicated: hostname, the HA link and management interface definitions, the plugin's own settings, `/conf/activestandby/*`.
- Replicated: everything else in `config.xml` (rules, NAT, VPN, users, certs, VLANs, LAGGs, assignments, ...), plus the list of installed `os-*` plugins.

## Setup (both nodes, identical hardware, same OPNsense and plugin versions)
1. HA link: one dedicated cable, static IPv4 on each node (e.g. 10.99.0.1/24 and 10.99.0.2/24). Optional management interface on a shared management network.
2. System > Active/Standby HA > **Setup wizard** on both nodes: pick interfaces, choose role, pair (standby first: it shows address and token).
3. Pairing refuses nodes that differ in OPNsense version, kernel, NIC names or plugin versions.
4. Data interfaces must have static addresses. The wizard warns about DHCP/PPPoE, CARP VIPs and the built-in High Availability sync.

## What happens when
| Event | Behaviour |
|---|---|
| Active node lost (power, crash, reboot) | Standby promotes after ~3 s of silence on **all** heartbeat paths, total ~6 s. |
| Standby cannot prove it is not the isolated one (its HA/management carrier is down) | **No** automatic promotion; event "held back"; promote manually if the peer is really down. |
| Both nodes up, nobody active (cold start) | Primary promotes ~5 s after the daemons start, secondary only after ~30 s. |
| A rebooted node | Always boots into standby (before interfaces are configured), then follows the rules above. No preemption: failback is manual. |
| Both nodes active (split-brain) | Higher epoch wins (tie: primary); the other demotes within ~3 s. |
| Config save on the active node | Pushed as a numbered revision within ~1 s; standby merges, reloads pf only, acks. On any error it rolls back and shows the error. |
| Standby has no WAN | Plugin installs go through a restricted proxy on the active node (HA link only, peer only, repository hosts only); the standby clock follows the active node. |

## Manual operations (Status and Pairing page, or API)
- **Failover now**: graceful switchover from either node (~3 s). **Force this node active/standby**: override, risk of split-brain.
- **Full resync**: push the complete config again. A damaged saved config on the standby triggers this automatically.
- **Break pair**: both nodes become standalone. A former standby keeps its disabled data interfaces: restore a config backup on it before use.

## Updating one node (tested: OPNsense 26.7 to 26.7.6_3, 86 packages incl. strongSwan 6.0.7 to 6.1.0)

What happens, in order:
1. **Update the standby first.** On it: `hasyncd ctl unlock` (30 minutes, so migrations can write the config), then update through the active node's proxy,
   e.g. `env HTTP_PROXY=http://<active HA IP>:7691 HTTPS_PROXY=http://<active HA IP>:7691 pkg upgrade -y` (use `opnsense-update` the same way when a new base/kernel
   release exists; `pkg` and `fetch` honour the proxy variables), then reboot it. It comes back as a silent, locked standby.
2. **Versions now differ.** Both nodes show "peer runs X, this node runs Y" (status page banner, dashboard widget, event log). Config sync is refused across core
   versions with a clear message and **resumes by itself** once the versions match. Failover keeps working.
3. **Do not edit the configuration during this window.** Edits on the (old) active node cannot be replicated. A graceful failover is therefore **refused** while the
   active node holds changes the peer does not have ("a failover would lose them"). Fix the sync, repeat the edit later, or force it: a forced failover saves the old
   config to `/conf/activestandby/unreplicated-<time>.xml` and logs it, then the new active node's config replaces it.
4. **Fail over to the updated node** (about 6 s; the IPsec tunnel re-established with strongSwan 6.1.0 against a 6.0.7 peer). Then update the other node the same way
   (unlock, upgrade through the new active node's proxy, reboot).
5. **Done when the warning disappears.** After every promote the new active node pushes its complete config, so both nodes converge; stale errors clear themselves.

Rollback: take a hypervisor snapshot or a config backup first. Packages can be reinstalled from the active node's proxy; a node that stays on an older version only stops
receiving config until it is updated.

## Files and logs
- `/var/log/hasyncd.log`, events in the GUI (`/conf/activestandby/events.log`), state in `/conf/activestandby/state.json` (+ `.bak`).
- `hasyncd ctl status|events|failover|promote [--force]|demote|resync|break` on the console.

## Shared MAC choice
By default pairing pins the PRIMARY node's current MAC on every data interface. A locally administered virtual MAC (e.g. 02:00:5e:xx:xx:xx, set in the interface MAC address field before pairing or by editing `spoofmac`) is cleaner: it does not clash with the primary's hardware address if that node is replaced, and it works on virtual switches that never relearn a vendor MAC owned by another port. On virtual NICs whose driver does not reprogram its receive filter, enable **Promiscuous mode on data interfaces** in Settings.

## Known limitations
- IPv4 only for gratuitous ARP; IPv6 neighbour announcements are not sent yet.
- Hiding the core "High Availability" menu is cosmetic; the pages stay reachable by URL.
- Behaviour of the Intel X710 (`ixl`) on admin-down and the real boot window must be confirmed with `lab/hw-probe.sh` before production.
- Tested with a real IKEv2/PSK site-to-site tunnel to a third OPNsense node (lab/tools/ipsec-lab.ps1): re-established through the new active node 5.9 s after a graceful failover and 6.2 s after a hard power-off of the active node (peer replaces the old IKE SA). dpinger with a monitored WAN gateway also follows the active role (stopped on the standby, restarted on promote). Not yet tested: traffic flowing through the tunnel during failover, peers other than OPNsense/strongSwan.

## Adding the second firewall with the kit
1. On the primary: Setup wizard, step 1 (HA link), step 2 "Primary", step 3 "Automatic". Enter the second firewall's hostname and HA link address (and management address or `dhcp`), then download the file.
2. Install OPNsense (same version, same ports) and the plugin on the second machine, cable the HA link, restore the file under System > Configuration > Backups, reboot.
3. The primary keeps trying to reach the HA address and pairs with the new node by itself (the Status page shows "Waiting for the second firewall"). The file is valid for 48 hours, contains secrets and should be deleted afterwards.
The kit is the silent standby view of the primary's config: the second firewall does not touch the shared addresses until it is paired.

Detection and takeover: planned failover 0.65-0.85 s outage, unplanned (power cut) 1.5 s with detection time 1 s; the default detection time is 2 s. 

## Failover timing (lab, Hyper-V, 4 interfaces + VLAN + LAGG)
Manual failover: about 1.1–1.5 s of lost traffic (a 20 ms ping from the PC), 1.3–1.9 s for the whole call. Earlier builds needed about 6 s. Where the time goes on the new active node: virtual devices 0.33 s, interfaces 0.4–0.6 s, pf reload and services 0.4 s; the DNS/DHCP restart (0.6 s) runs afterwards in the background. Real NICs add link-up time (typically 1–3 s for copper) that the lab cannot show. The standby also waits while the peer reports it is promoting (heartbeat `busy`).
