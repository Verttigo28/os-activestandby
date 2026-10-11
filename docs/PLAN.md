# os-activestandby — SonicWall-style active/standby HA for OPNsense

Status: **PLAN — awaiting approval. No code written.**
Working name: `os-activestandby` (namespace `OPNsense\ActiveStandby`). Rename freely.

## 0. What exists today

The folder `opn-better-ha` contains only `.idea/` project files. Nothing to reuse.
Everything below is greenfield. Choices I made (yours to override) are tagged **[my call]**.

## 1. Reference layout (opnsense/plugins, e.g. `net/haproxy`)

```
net/haproxy/
  Makefile                      PLUGIN_NAME/VERSION/COMMENT/DEPENDS/MAINTAINER + .include plugins.mk
  pkg-descr
  src/etc/inc/plugins.inc.d/haproxy.inc        legacy hooks (service registration, syslog, etc.)
  src/etc/rc.syshook.d/{start,stop}/50-haproxy boot/shutdown hooks
  src/opnsense/mvc/app/
    controllers/OPNsense/HAProxy/{IndexController.php, Api/*Controller.php, forms/*.xml}
    models/OPNsense/HAProxy/{HAProxy.xml, HAProxy.php, Menu/Menu.xml, ACL/ACL.xml}
    views/OPNsense/HAProxy/*.volt
  src/opnsense/scripts/OPNsense/HAProxy/*      helper scripts called by configd
  src/opnsense/service/conf/actions.d/actions_haproxy.conf   configd actions
  src/opnsense/service/templates/OPNsense/HAProxy/{+TARGETS, *.conf}  config templates
```
(WireGuard is no longer in the plugins repo; it moved into core. HAProxy is the better template.)

### Proposed layout for this plugin

```
opn-better-ha/
  docs/PLAN.md
  Makefile, pkg-descr                       (plugin package; builds with the opnsense plugins framework)
  daemon/                                   Go heartbeat + sync transport daemon ("hasyncd")
    cmd/hasyncd/  internal/{heartbeat,fence,transport,state,proxy}/  go.mod
  src/etc/inc/plugins.inc.d/activestandby.inc     service registration, dpinger/ipsec suppression hooks
  src/etc/rc.syshook.d/{early,start,stop,config}/  boot enforcer, config-save push trigger
  src/etc/rc.d/hasyncd                            rc script for the Go daemon
  src/opnsense/mvc/app/
    controllers/OPNsense/ActiveStandby/
      IndexController.php (Status/Settings/Pairing pages)
      Api/{PairingController,StatusController,SyncController,FailoverController,SettingsController}.php
      forms/{general,pairing}.xml
    models/OPNsense/ActiveStandby/{ActiveStandby.xml,.php,Menu/Menu.xml,ACL/ACL.xml}
    views/OPNsense/ActiveStandby/{status,settings,pairing}.volt
  src/opnsense/scripts/OPNsense/ActiveStandby/
    promote.sh  demote.sh  enforce_standby.sh  garp.py|sh
    sync_apply.py  sync_export.py  pkg_sync.py  pair.py  status.py
  src/opnsense/service/conf/actions.d/actions_activestandby.conf
  src/opnsense/service/templates/OPNsense/ActiveStandby/   hasyncd.conf, rc.conf.d
```

Division of labour: PHP = GUI + REST only. Go daemon = heartbeat, fencing decision, **and the authenticated sync transport** (mTLS, certs generated at pairing, peer-pinned). Python/shell via configd = privileged actions. No SSH/rsync dependency. **[my call]**

## 2. Key design decisions

### 2.1 Identical MACs: use the native `spoofmac` field
Each interface in config.xml already has `<spoofmac>`. At pairing, the plugin writes the master's hardware MAC into `spoofmac` on every data interface (WAN/LAN/VLAN parents/LAGG). That value replicates inside config.xml, so the standby's normal interface code sets the same MAC. A later apply on the master cannot undo it, because it *is* the config (this covers your "later apply can't undo our MAC" gotcha). VLAN children inherit the parent's MAC; LAGG needs its own spoofmac. A verifier in `enforce_standby.sh`/`promote.sh` asserts MAC equality and re-sets it if not. **[my call]**

### 2.2 Node-local exclusion list (XML subtree merge, not string diff)
Push = master config.xml minus excluded subtrees; standby keeps its own copy of those. The HA link and mgmt port **need different IPs per node**, so these are excluded too:
- `system/hostname`, `system/domain` (optional), proxy/NTP/DNS settings used to reach the master
- `interfaces/<ha>`, `interfaces/<mgmt>`, their gateways, dhcp and the plugin's own model (`OPNsense/ActiveStandby`)
- node role, peer address, pairing keys/certs

Everything else replicates (rules, NAT, routes, IPsec, users, certs, VLANs, LAGGs, assignments).

### 2.3 Standby must never raise LAN/WAN — three layers
OPNsense has no clean "veto interface up" hook, so defence in depth:
1. **Apply path**: standby applies via `sync_apply.py`, which writes config and runs a *restricted* reconfigure set (never `interfaces_configure` for data interfaces). The addresses are still configured on the down interfaces, so promote is just `up`.
2. **Enforcer**: `enforce_standby.sh` (ifconfig down every data interface + parents + VLAN/LAGG members) runs from the `config`/`newwanip`/`early` syshooks and every 1 s from `hasyncd` while role=standby.
3. **Boot**: the standby role persists in a local flag file (`/conf/activestandby.role`); an `early` syshook downs data interfaces ASAP.
**Honest risk (R1):** `rc.bootup` configures interfaces in PHP, so there may be a window of seconds at boot where the standby's interfaces come up with the active's IP and MAC. Phase 0 must measure this, and the mitigation (patching or pre-seeding config before netif) depends on the result.

### 2.4 Failover speed
Pre-staged standby (config applied, services configured, addresses set, interfaces down). Promote = `ifconfig up` → re-add routes (down interfaces lose routes) → GARP/NA per IP incl. VLANs/aliases → start strongSwan, dpinger and the services bound to those interfaces. **Target: ≤10 s [my call, tell me yours].** A full reconfigure on promote would be 20–40 s, so I avoid it.

### 2.5 Split-brain: the hard part
Same IP *and* same MAC on both nodes means dual-active is a real outage, and the standby cannot test its own data links (they are admin-down), so "promote only if local link monitoring is healthy" needs refinement:
- Two heartbeat paths: HA link (primary) + mgmt port (secondary). Optional third: a witness (a ping target reachable via mgmt).
- Promotion requires: all peer paths dead for N seconds **and** mgmt path itself healthy (so the standby itself is not the isolated one) **and** HA-link carrier is up locally (distinguishes dead peer from cut cable: if carrier is down on the HA link only while mgmt still reaches the peer, **do not promote**).
- Epoch counter bumped on each promotion, carried in every heartbeat. If both claim active after a partition heals, the lower epoch (tie: configured priority) demotes itself immediately.
- Master does not self-demote because the HA link vanished; it stays active as long as its own data path is fine. A pre-promotion DAD-style ARP probe is **not** usable (same MAC), so I do not rely on it.
- Pre-emption off by default (failback is manual or timed). Failback = same demote/promote in reverse.
- Residual risk: HA link **and** mgmt both cut while master is healthy ⇒ dual-active. Only fencing (IPMI / smart PDU) fully fixes that, which is out of scope but the daemon will have a pluggable "fence command" hook.

### 2.6 Other gotchas
| Gotcha | Handling |
|---|---|
| dpinger alerts on standby | Plugin hook stops dpinger and suppresses gateway-alarm → routing/filter reload while role=standby; started in promote. |
| strongSwan on standby | Stopped by role-change and on `config` apply; started in promote (`swanctl --load-all`), stopped in demote. DPD re-establishes; set `start_action=start`/DPD on our side where we initiate. |
| No WAN on standby | `hasyncd` on master exposes an HTTP forward proxy on the HA IP (allow-listed to OPNsense mirror/pkg hosts) plus NTP/DNS via master. Standby's proxy/NTP/DNS are node-local settings (see 2.2). |
| Version mismatch | Pairing refuses unless `opnsense-version`, kernel, NIC names/count/order and installed `os-*` package versions match. |
| pfsync | Over the HA link with unicast `syncpeer`; independent of CARP. Standby keeps pf loaded so states land. Verify state-to-interface binding on down interfaces in Phase 0. |
| Config revisions | Monotonic revision N on master; standby acks N or returns error; GUI shows in sync / N behind / failed + last error. |
| Rollback on standby | Before apply: snapshot to `/conf/backup`. On failure: restore, re-run enforcer, ack with error. Sync state keeps pending revision. |
| Package replication | Master lists installed `os-*` + versions; standby installs missing via the proxy **before** applying config. Mismatched versions block sync with a clear error. |

## 3. Phases

**Phase 0 — Feasibility spikes (new, ~1–2 days, throwaway).** Fastest way to retire R1 and the biggest unknowns: (a) how `rc.bootup`/interface apply behaves with `spoofmac`, and the boot-time up window; (b) does the `config` syshook fire on save, which version; (c) can interfaces keep addresses while down and what promote costs in seconds; (d) pfsync state install with down interfaces; (e) does admin-down drop carrier on your NICs/hypervisor vNICs.
**Phase A — Skeleton + pairing.** Plugin package, menu/ACL/model, settings + pairing pages, `hasyncd` stub with mTLS, pairing handshake (HA link IPs, cert exchange, compatibility check, MAC spoof write, role assignment), break-pair.
**Phase B — Heartbeat + manual failover.** Go daemon (HA-link + mgmt heartbeat, epoch), `promote.sh`/`demote.sh`/`enforce_standby.sh`, GARP, strongSwan and dpinger control, status API/page, manual failover button.
**Phase C — Config sync.** Hook on save → export with exclusions → revisioned push → standby apply with restricted reconfigure → rollback → status in GUI → full resync action.
**Phase D — pfsync + automatic failover + split-brain protection.** pfsync config on HA link, auto-promote rules from 2.5, epoch resolution, optional witness and fence hook, flap damping.
**Phase E — Packages, proxy/NTP via master, monitoring, status page polish, docs.**

## 4. How you test each phase on two OPNsense VMs

Common lab: two OPNsense VMs, same version, NICs in the same order: `vtnet0 WAN`, `vtnet1 LAN`, `vtnet2 HA` (isolated host-only net), `vtnet3 MGMT`. WAN and LAN on shared bridges with one Linux test client on each (LAN client pings through; "WAN" side has a simple upstream router). Hypervisor must allow MAC changes/promiscuous on the LAN/WAN bridges (Proxmox: no firewall MAC filter; VirtualBox: Allow All). Different vNIC MACs per VM are fine; the plugin makes them identical.
- **0**: scripts run by hand; record timings, boot-window capture with `tcpdump` on the LAN bridge during standby reboot.
- **A**: install the package on both, pair via GUI; verify identical `ifconfig` MACs on data NICs (after master apply), refusal when versions/NICs differ (add a NIC to one VM), break-pair restores independent nodes.
- **B**: standby shows all data NICs `DOWN`; click Failover: client keeps pinging (expect ≤ target loss), `arp -a` on client unchanged, strongSwan flips; failback. Kill `hasyncd` and reboot tests.
- **C**: change a firewall rule/VLAN on master → status "in sync" within seconds; deliberately push a broken config (invalid XML section via test hook) → standby rolls back, GUI shows error and last error text; confirm LAN/WAN never up on standby (`tcpdump` on bridges while syncing).
- **D**: pull HA vNIC only (must not promote), pull mgmt only, pull both, power off master (must promote); partition-heal test for epoch demotion; existing TCP/NAT sessions survive failover.
- **E**: remove a package on the standby → resync installs it through the master proxy; standby `pkg update` and NTP work with no WAN; status page walkthrough.

## 5. Questions before I start (they change the plan)
1. OPNsense version you run and plan to run (affects syshook availability, strongSwan/swanctl, PHP, Python version, Go target)?
2. Failover time you consider acceptable (my default ≤10 s)?
3. Lab hypervisor (Proxmox / VirtualBox / ESXi)?
4. Is the dedicated HA link a single port, or LAGG? And is the mgmt port always present on production nodes (needed for the secondary heartbeat)?
5. OK to add Phase 0 spikes before Phase A?

---
## Progress log
- **Phase 0** done: docs/PHASE0-FINDINGS.md. **Phase A** done (pairing, mTLS, compat check, spoofmac pinning).
- **Phase B** done in the lab (Hyper-V, 26.7): heartbeat 500 ms/3 s dead, `promote`/`demote`/`failover`, standby view,
  boot-into-standby hook, enforcer, GARP via bpf. Measured: graceful failover 3.0-3.2 s end to end, forced promote 2.4 s,
  rebooted active node returns as silent standby. Bug found and fixed on the way: enforcer raced with promote (now takes the ops lock).
  Not yet done in B: B-side `active.xml` comes from sync in Phase C (lab seeded by hand); epoch is not yet adopted from the peer's heartbeat (Phase D).

## Added requirements (from the user, 2026-10-11)
1. **Automatic promotion when the active node reboots/dies** -> Phase D (peer-lost rules + boot election: a paired node boots standby, then
   promotes if the peer is not active within a role-dependent delay; the master role wins ties).
2. **Setup wizard** (new Phase A2, before C): step 1 choose HA link interface and optional MGMT interface (dropdowns, IP check, same subnet
   check against the peer), step 2 role (this node = active/standby), step 3 pairing token exchange, step 4 summary + first sync.
3. **Replace OPNsense's built-in HA flow for this setup**: hide/redirect System > High Availability (CARP/XMLRPC) when active/standby is
   enabled, warn if CARP VIPs or the core sync target are configured, and remove the "sync to backup" steps from the core menu.

- **Phase C** done (lab): numbered revisions active->standby, merge keeps node-local parts (hostname, HA/MGMT interface, plugin settings),
  standby view derived, pf reloaded only, rollback on failure, auto retry, GUI "in sync / N behind / FAILED + error", full resync,
  direction flips with the active role, damaged active.xml self-heals via resync request in the heartbeat.
- **Phase D** done (lab): two heartbeat paths (HA + mgmt), auto-promote when the peer is silent on ALL paths for 3 s and this node is not
  isolated (mgmt carrier up, or HA-only with carrier up; override option), epoch-based split-brain resolution, cold-start election
  (master 5 s / secondary 30 s), crash-safe state (fsync + backup copy, fail-safe to silent standby when state is unreadable).
  Measured: peer power-off -> new active in ~6 s (3 s detection + 2.4 s promote); split-brain resolved in ~3 s; isolated standby refuses to
  promote and does so as soon as its links return.
- Core "System > High Availability" menu is hidden while the plugin is enabled (dynamic Menu.php).

### Known gaps / untested
- IPsec and dpinger start/stop go through `pluginctl -s`; not exercised yet (no tunnel configured in the lab). Gratuitous ARP sent via bpf but not yet seen on a real switch.
- X710 behaviour (carrier on admin-down, boot window on real hardware) still needs `lab/hw-probe.sh` output.
- The core HA page is only hidden, not blocked by ACL; wizard (A2) and Phase E (packages, update/NTP via master, monitoring polish) remain.
- Hard power-off in Hyper-V lost minutes of un-synced file writes (UFS); state and config paths are now fsynced, plugin files are not.

- **Wizard (A2)** done and driven end to end in the real GUI (Chrome): interface pick, pre-flight check (CARP VIPs, built-in HA sync, static IPs,
  HA rule), save + start daemon, role, pairing; plus the standby banner and an event log table on the status page.
- **pfsync** done: configured by the daemon on the HA link (node-local); states created on the active node's LAN/WAN appear on the standby although its
  data interfaces are down.
- **Phase E** done (lab): package replication through the restricted update proxy (os-helloworld installed on the standby in ~6 s, proxy refuses other
  hosts and non-peers), standby clock follows the active node (5 min skew corrected in <1 s), persistent event log in the GUI, docs/OPERATIONS.md.
- Remaining before production: X710 probe, IPsec/dpinger with a real tunnel, IPv6 announcements, real-switch GARP check, ACL-level blocking of the core HA pages.
- IPsec follows the active role (found and fixed: the service is `strongswan` not `ipsec`; configd actions are used). The standby enforcer also stops
  charon/dpinger/openvpn if the boot sequence starts them on a standby. Still untested: a real tunnel re-establishing after failover, dpinger with gateways.
- **Read-only standby** (user finding: config could be edited on the secondary): `config.xml` is immutable (schg) on a standby; daemon unlocks only for
  its own writes, always unlocks on active; verified: change on standby does not persist, sync through the lock works, lock moves on failover, locked
  standby reboots cleanly (GUI 200, silent interfaces). `hasyncd ctl unlock|lock` for updates. Residual: OPNsense still says "saved" on the standby;
  root on the standby console can clear the flag (out of scope).
- **Real IPsec test passed** (third VM `opn-peer`, IKEv2/PSK, 192.168.1.0/24 <-> 192.168.50.0/24, lab/tools/new-peer.ps1 + ipsec-lab.ps1):
  tunnel re-established through the new active node in 5.9 s (graceful failover) and 6.2 s (hard power-off of the active node); strongSwan stopped on
  the standby; rejoining node came back as a silent, locked standby with config in sync.
- Lab lessons: OPNsense `reply-to` on a DHCP management interface breaks node-to-node traffic on the same subnet (set disablereplyto on the lab allow
  rule); WAN `blockpriv` must be off when the lab peer uses a private address; hard power-offs in Hyper-V lose recent un-synced file writes (run `sync` first).
- dpinger with a real monitored WAN gateway verified: running only on the active node, restarted on each promote, absent on the standby (no gateway alarms there).
- **Status at end of autonomous session**: all planned phases (0, A, A2 wizard, B, C, D incl. pfsync, E) implemented and verified in the Hyper-V lab incl. a real IPsec tunnel.
  Remaining before production: X710/real-switch checks (lab/hw-probe.sh), packaging through the plugins build framework (docs/BUILD.md), IPv6 announcements,
  traffic-through-tunnel during failover, other VPN peers.

## Must-have verification round (user request: widget + must-have list + tests)
- Dashboard widget `ActiveStandby.js` (+ Metadata XML) added; verified in the real dashboard (role/state, peer, heartbeat links, sync, last event; persists).
- docs/FEATURES.md lists every must-have with status; all lab-testable items PASS. Bugs found and fixed during the round:
  1. Interfaces/VLANs added after pairing never got a shared MAC -> the sync engine now pins MACs before every push.
  2. Promote on a node that never created the VLAN/LAGG devices failed (correctly refused and rolled back) -> promote now creates virtual devices with
     OPNsense's own PHP functions (`create_devices.php`); the configd "configure" actions only handle GUI-flagged items.
  3. Virtual NIC drivers that keep their hardware RX filter (Hyper-V `hn`) drop frames for the shared MAC -> optional "Promiscuous mode on data
     interfaces" setting (lab on, real NICs off).
- Traffic through the VPN during failover: long-lived TCP stream (LAN client behind the cluster -> tunnel -> peer): graceful 5 s, failback 6 s, hard power-off
  10 s of silence, no sequence breaks, session never reset.
- Lab lessons: Hyper-V never relearns a MAC it considers owned by another vNIC -> use locally administered shared MACs (02:00:5e:...); `fetch` and `pkg` honour
  HTTP(S)_PROXY, so standby updates go through the active node's proxy.

## Update-one-node round (user question: "what happens in an update of one node")
- Real test: standby B updated 26.7 -> 26.7.6_3 (86 packages incl. strongSwan 6.0.7 -> 6.1.0) through the active node's proxy, rebooted; failover to B
  (5.9 s, tunnel up with strongSwan 6.1.0 against the peer's 6.0.7); A updated the same way; versions converged and sync resumed by itself.
- Found and fixed: (1) no visible version mismatch -> heartbeat carries OS + plugin version, warning in GUI/widget/events; (2) edits made on the old active node
  during the mismatch window were silently lost at failover -> graceful failover refused while unreplicated edits exist (force available), forced failover saves
  them to `unreplicated-<time>.xml`; (3) after a role change the nodes could diverge (stale "acked" hash) -> the new active node forces a full push, stale errors clear;
  (4) VLAN/LAGG devices and member NICs came up during standby boot until the daemon started -> detached boot guard from the early hook.
- Packaging: `build/build.ps1` + `build/build-pkg.sh` build `os-activestandby-<ver>.pkg` with pkg(8); installed on a clean node and removed again without leftovers.
  GitHub: LICENSE (BSD-2-Clause), CI workflow (test, FreeBSD cross-build, .pkg + release on tags), lab credentials moved out of the tracked files, local git repo
  initialised (nothing committed or pushed). docs/ROADMAP.md holds the feature advice.
