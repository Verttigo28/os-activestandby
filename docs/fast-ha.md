# Warm standby (experimental)

**Status: implemented, opt-in, tested against a model and in the sandbox; on real NICs the link drops at the MAC change, so it brings no gain on the Intel X710 and I210 that were measured (see below).** The setting *Standby mode* = *Warm standby* turns it on; the default is *Classic* and nothing changes unless an administrator chooses it. It was built from the code and tested against a model of `ifconfig` and a switch (see the test list below), not on OPNsense, a NIC driver or a switch. Expect to tune it with real measurements (`docs/validation.md` section H). The rule behind every decision: *no speed-up may add any split-brain risk*; where that could not be shown, the classic path is kept.

## What it does

Classic standby: every data interface is administratively down; a takeover brings the link up (a real NIC renegotiates it with the switch, up to 8 s of `waitCarrier`), runs the PHP interface code per interface, and only then announces the MAC.

Warm standby, for the interfaces that qualify:

| | Standby (warm) | Active |
|---|---|---|
| Link | **up**, never renegotiated | up |
| MAC | the NIC's **own** hardware MAC | the shared MAC (`spoofmac`) |
| IPv4 / IPv6 | none; IPv6 disabled on the device | configured |
| Answers ARP/ND, routes | no (no address, nothing to answer for) | yes |
| Promiscuous | off | as configured (*Promiscuous mode on data interfaces*) |

**Takeover** (`warmActivate`, native `ifconfig`, a few ms): verify the port really is in the warm state (up, own MAC, no address); set the shared MAC; enable IPv6; MTU; add the address, the IP aliases and the static IPv6 address; bring it up; read it back and check that the MAC changed and the address is there. Then the usual steps follow unchanged: gratuitous ARP, routes, `filter reload` (verified), services. Any problem **before traffic starts reverts the ports to the silent state and runs the classic takeover for everything**.

**Stand down** (`silenceCmds`): the own MAC comes back **first**, so the NIC stops accepting frames for the shared MAC at once; then promiscuous mode off, then every address removed, then IPv6 disabled. The link is never taken down. The peer is only told to promote after this is done (same hook as in classic mode), and the transition protocol of `docs/audit.md` keeps the order *stand down, then take over*.

Interfaces that do not qualify keep the classic takeover, in the same promotion: DHCP/PPPoE/dynamic IPv4, no shared MAC, LAGG, bridge, GRE/GIF/VXLAN/QinQ, a virtual IP that is not an IP alias (proxy ARP, CARP), a static IPv6 that is not a literal, or a VLAN whose assigned parent does not qualify. The reason of each is shown in the pre-flight and the status.

## Readiness

Every second the standby evaluates whether a fast takeover is possible (`warmReadiness`) and keeps its warm ports silent. It is **not ready**, with the reason, when: a warm port has no link; a port's permanent MAC is unknown (it carries the shared one and `ifconfig` does not print the permanent one, see below; such a port is taken down instead of left on the shared MAC); no interface qualifies; the saved configuration of the active node is missing or damaged; configuration drift is detected; no plan could be made. The result is in the status (`warm`), in the heartbeat (the active node sees the standby's state as `peer_warm`), in the pre-flight and as a banner in the GUI. A takeover on a standby that is not ready simply uses the classic path: readiness never blocks a failover.

The permanent MAC is `hwaddr` as printed by `ifconfig` (it appears when it differs from the current one), else the current MAC if it is not the shared one. It is remembered in `/conf/activestandby/hwmacs.json`, and an active node refreshes it every 30 s while it is still visible.

## Setting and compatibility

*Standby mode* is a shared setting (copied from the active firewall; the daemon restarts once to apply it). Unknown values mean classic. A configuration migration is not needed: the field is new with the default *classic*.

## Tested (model, not hardware)

`daemon/warm_test.go` runs the real code against a model of two hosts and a switch that watches the invariant that the shared MAC is never configured on two ports at once:

* command order of takeover and stand down (own MAC first, link never down, up last), parsing of `ifconfig`, plan handling, VLAN parents, HA/management devices excluded;
* readiness (silences leftovers, no link, missing configuration), classic mode touches nothing;
* a complete A to B takeover and back, with no moment where two ports hold the shared MAC;
* a driver that ignores the MAC change: the fast path fails, the ports are silent again, nothing is left configured;
* a port that is not silent at takeover is refused; an unknown permanent MAC falls back to classic.

## Not tested, not implemented

| | |
|---|---|
| Implemented, **not tested on OPNsense** | the plan extractor in `ifaces.php` (no PHP available where it was written; CI lints it), the integration in promote/demote/enforcer/pre-flight, the GUI setting and banner |
| Unknown until measured | whether the driver (ixl, igb, vmx, hn, virtio) changes the MAC with the link up without dropping it or the receive filter; whether the switch learns the shared MAC from the gratuitous ARP quickly enough or needs more; port-security (two MACs on the port group); hypervisor MAC spoofing rules; VLAN devices following their parent's MAC; `ifdisabled` behaviour per driver |
| Not done by design | the routes step (`system_routing_configure`), `filter reload` and the services still run as in classic mode after the addresses are in; OPNsense's own interface state (files under `/tmp`, rc events such as `rc.newwanip`) is **not** refreshed on the fast path |
| Not implemented | LAGG/LACP, bridges and tunnels, dynamic addressing, CARP, a pf ruleset preloaded on the standby (OPNsense omits interfaces without an address, so the ruleset has to be generated after the addresses exist), IPv6 router advertisements, reconciling OPNsense's interface state after the fast path |

## Measured on the target hardware: the warm standby does not pay off on these NICs

Supermicro machine, Intel X710 (`ixl`, firmware 9.54) and I210 (`igb`), FreeBSD 15.1, measured with `lab/warm-probe.sh` and `lab/hw-probe.sh` (one machine, a link partner on the port, the machine's own view of the carrier; the switch side was not observed):

| Measurement | X710 (`ixl1`, 10G DAC) | I210 (`igb0`) |
|---|---|---|
| Administrative down, then up: time until the link is active again | about 0.8 s | about 1.6 s |
| **Changing the MAC address with the link up: link lost for** | **about 0.74 s** | about 0.9 to 1.9 s |
| `ifconfig up` on an interface that is already up | 0 ms | 0 ms |
| Link while administratively down | **stays active** (`dev.ixl.N.link_active_on_if_down=1`) | goes down |
| Frames sent while administratively down | none | not measured |

What it means:

* **Setting the shared MAC costs the same link drop as bringing the interface up.** The driver re-initialises the port when its MAC changes. A warm standby that holds its own MAC and takes the shared one at takeover therefore still drops the link for about three quarters of a second on these NICs: the saving over the classic takeover is only the PHP and the interface code, not the link.
* On the X710 the classic standby does not renegotiate at all (the link stays active while the port is down), so the "up to 8 s of carrier wait" that motivated the warm standby does not apply to this hardware.
* **Recommendation: keep the classic mode here.** The warm standby stays in the code, off by default, for hardware where a measurement shows a gain; with a LAGG (as in the production layout of this machine) it does not apply anyway.

The variant that avoids the bounce, a standby that already holds the shared MAC while staying completely silent, is the hot standby below. The worry that the firmware LLDP agent of the X710 would transmit with the shared MAC was measured and does not hold: it uses the permanent MAC of the port.

## Hot standby: the takeover that adds addresses and nothing else

The warm standby above takes the shared MAC at the takeover, and on these NICs that costs a link event. The **hot standby** (setting *Standby mode* = Hot) moves that cost to the moment the node becomes standby, when nobody waits for it:

| | Standby (silent) | Takeover | Stand down |
|---|---|---|---|
| Classic | ports down | bring up the links, PHP per interface (3.1-3.8 s with 21 interfaces) | ports down |
| Warm | ports up, **own** MAC, no address | MAC change (link drop on the X710), addresses | MAC back, addresses off |
| **Hot** | LAGG, its ports and the VLANs **up, shared MAC, no address, IPv6 off** | **addresses only (176-222 ms for 21 interfaces), announce** | addresses off |

**Why a standby with the shared MAC is silent.** A port with a MAC and no address sends nothing with it: no ARP, no ND, no MLD (IPv6 is switched off *before* the port comes up). Measured with the switch as observer: LAGG and both X710 ports up with the shared MAC for 45 s, 2381 frames, all LLDP from the firmware agent with the **permanent** MAC of the port; none with the shared MAC; the switch kept the shared MAC on the active node's port. Because another NIC may behave differently, the daemon **watches the software transmit counter** of every silent interface of the standby: one that transmits is shut down at once, the standby reports "not ready" and the takeover is classic. It also checks that the packet filter of the standby has its default deny (without it a frame for the shared MAC would be routed). An LACP LAGG is exempt from the transmit watch (it sends LACPDUs, link-local frames that a bridge neither forwards nor learns); its VLANs are not.

**What qualifies.** Interfaces with a static IPv4 address (IP aliases and a static IPv6 address are fine), a shared MAC, on a NIC, a VLAN or a LAGG (failover, load balance, round robin, LACP with the setting below). DHCP, PPPoE, an MTU, bridges, GRE/GIF and other virtual IP types keep the classic takeover (the plan says why, in the pre-flight and the status). Any problem at the takeover (an interface not in the silent state, a command that fails, an address that does not appear) undoes what was done and uses the classic path.

**LACP.** The standby's LAGG runs LACP with the **shared MAC as system id**, so the takeover finds its bundle already established. This needs a switch with **one static port-channel per firewall**; a switch that aggregates dynamically by the partner system id would merge both firewalls into one LAG. Setting *Hot standby with an LACP LAGG*: *Off* (default) keeps the classic takeover for an interface directly on an LACP LAGG and for the VLANs on it, *Shared* is the fast one. VLANs on an LACP LAGG that is **not** an interface are hot in both cases (the LAGG keeps its own MAC and runs promiscuous so that it accepts the VLAN MAC). Tested against two RouterOS 802.3ad bonds (1 s LACP rate): two separate bonds, both with the shared MAC as partner system id, both fully active.

**Measured** (two Supermicro machines, X710, MikroTik switch; two ports per firewall; the client pings every 20 ms through the switch; [validation](validation.md) section J3):

| Scenario | Classic, failover LAGG | Classic, **LACP** | **Hot**, failover LAGG | **Hot, LACP** (shared) |
|---|---|---|---|---|
| Promotion (21 interfaces) | 3.1-3.8 s | 2.3-3.2 s | **176-222 ms** | **176-222 ms** |
| Graceful failover, client view | 1.0 s | **12.5 s** | **0.12-0.16 s** | **0.10-0.18 s** |
| Crash of the active node | 4.9 s | 9.6 s | **2.8 s** | **3.0 s** (3 s of it is the detection time) |
| Dead card (both ports cut) | 5.1 s | not measured | not measured | **3.1 s** (2 s hold) |

**The ruleset of the standby (the mirror).** A standby generates its own packet filter ruleset from its standby view of the configuration: no rule for the data interfaces (68 rules here, against 227 on the active node). Established connections pass anyway on the states pfsync replicated, but a **new** connection is dropped until the takeover has generated and loaded the real ruleset, about 1 s later: measured with a client opening a TCP connection every 40 ms, 21 connections in the first 0.85 s of a takeover needed the client's SYN retransmit (1 s) to get through. The hot standby therefore **mirrors the ruleset of the active node**: the active node sends what it has loaded (`/tmp/rules.debug`) to the standby over the pinned TLS connection; the standby checks it (default deny present, at most 8 MiB, no binary content, `pfctl -n` here), replaces the node-specific `set hostid` and `set limit` lines by its own (two nodes with one pf hostid would make the pfsync state ids collide), loads it, and reports the hash in its heartbeat so that the active node sends again when they differ (a change of rules on the active node reaches the standby within 3 s, measured: +1 rule, then -1). If the standby no longer hears its peer after the load, it puts its own ruleset back and keeps the mirror off for ten minutes. Result with the mirror: 2-3 connections delayed in the first 0.08 s of a takeover instead of 21 in the first 0.85 s. The pre-flight shows whether the standby holds the ruleset of the active node.

**Limits.** The mode is experimental and off by default: validate it on your hardware first (`lab/`, section J3 of the validation), in particular that the NIC does not transmit with the OS MAC while silent (the daemon will tell you). Rules that depend on an address of the interface (dynamic address expansions) are inactive on the standby until the takeover gives it the addresses (pf then expands them by itself): the standby holds 187 of the 227 rules of the active node here.

### Detection time of 1 second

`detect_time` (Settings, Failover) defaults to 3 s and goes down to 1 s. What was measured with 1 s on the pair above (hot standby, LACP): a crash of the active node is served again after **1.2 s** (3.0 s with the default). A false failover did not happen under 80 s of full CPU load on both nodes, filter reloads in a loop on both and a ping flood on the HA link. What a shorter time costs, and what was fixed for it:

- **Any stall of the active node's daemon longer than the detection time makes the standby take over**, by design (it cannot tell a stall from a death). Measured: a daemon frozen for 1.6 s was replaced (a short split-brain overlap, resolved in the usual way), one frozen for 0.7 s was not. With the hot standby a needless takeover costs 0.1-0.2 s of traffic, with the classic one 1-5 s; the anti-flapping hold (10 s) and the two-switches-in-30-minutes limit still apply.
- **A frozen standby daemon woke up and promoted itself** with a 1 s detection (found by this test: the daemon that was frozen for 1.5 s took the silence it slept through for a dead peer, because the freeze guard still waited for 3 s). The guard now follows the detection time (never above it, never above 3 s). Frozen for 1.5 s and for 4 s: no promotion.
- A heartbeat is a request on a TCP connection: one lost packet delays a beat by the retransmission time of the stack (some hundred ms), and there are about 7 beats per second on each path. With two independent paths (HA link and management) both must be silent for the whole time, so one lossy path is not enough; but two paths through the same switch are not independent.
- Risks that do not change: a symmetric partition of both paths still produces two active nodes until it is resolved (fencing is the only complete answer), and a node that is merely starved (a virtual machine that is paused, a host overloaded) looks dead after the detection time.

**Recommendation**: 1-2 s on physical machines with a dedicated direct HA cable and a management path through another switch, the hot standby, and a watched event log for "peer lost" events during the first weeks; keep 3 s or more for virtual machines, a shared HA network or a classic standby (where a needless takeover costs seconds).


### Compared with CARP (from the design of both, not measured here)

CARP gives each shared address a virtual MAC (00:00:5e:00:01:VHID) and an advertisement every `advbase` seconds; the backup takes over after about `3 x advbase + advskew/256` seconds without advertisement (3-4 s at advbase 1, advskew 0), then OPNsense runs its CARP status scripts, which reconfigure services (typically a few more seconds). So expect roughly **3 to 6 s** for a node failure, and nothing faster than the advertisement timers whatever the hardware. It needs one virtual IP per subnet plus a real address on each node (three addresses per subnet), the configuration synchronisation goes through XML-RPC, and the states through pfsync as here. This plugin keeps a single address per interface and the same MAC on both nodes (no client has to learn anything), detects with its own heartbeat (3 s by default, 1 s minimum) and, in hot mode, takes over in about 0.2 s once the failure is detected: **a crash is served again after about 3 s, a manual or health-driven switch after 0.1-0.2 s**.

## Where the time goes (what to measure)

| Stage | Cost | Source |
|---|---|---|
| Detection | `detect_time` (default 3 s, minimum 1 s); about 8 heartbeats per period, minimum interval 150 ms | `heartbeat.go` |
| Decision | isolation checks, optional fencing command (up to 30 s) | `autofail.go`, `fence.go` |
| Interfaces | classic: PHP per interface plus up to 8 s of carrier wait; warm: native `ifconfig`, verified | `promote.go`, `warm.go` |
| ARP | gratuitous ARP once the identity is set, repeated for a few seconds | `garp_*.go` |
| pf and services | `filter reload` and VPN start in parallel; DHCP/DNS in the background | `promote.go`, `pf.go` |

Each promotion records its phases (offset in ms) and, for an automatic one, `detect_ms` in `hasyncd ctl history`; a promotion that used the fast path has the phase "shared identity taken natively". `lab/failover-bench.sh` measures the outage a client sees. **Measured on the pair of Supermicro machines with 20 VLANs on a LAGG (classic mode, [validation](validation.md) section J):** detection 3.4 s, promotion 4.4-4.9 s (LAGG 0.7-1.2 s because the X710 re-initialises when its MAC changes, 20 VLANs 2.3 s more, routes 0.3 s, pf reload and services 0.8 s), client outage 5.9 s graceful, 8.4 s after a crash, 11.9 s after a dead card (6 s health hold). The detection time is configurable from 1 s but the heartbeat (a TLS request per beat) is not meant for sub-300 ms detection, so no such target is promised.

## Next steps with real data

0. **What is left in the classic promotion** (measured, section J2): the LAGG 0.5-1.8 s (the X710 re-initialises when its MAC changes, which no software can avoid), routes 0.2-0.7 s (now overlapped with the carrier wait), the ruleset 0.7-1.5 s (now overlapped with the interfaces), the VLANs 0.06-0.14 s (native). The hot standby (below) removes the LAGG time by keeping the LAGG up with the shared MAC.
1. Run section H of `docs/validation.md` on the target pair (Classic and Warm, several runs each) and compare median, P95 and maximum.
2. From the phases decide what still costs time: if it is routes or `filter reload`, run them natively or in parallel; if the switch learns slowly, add more announcements or a different frame.
3. Decide whether OPNsense's interface state needs a reconcile step after the fast path, and what it may safely run without a link flap.
4. Extend to the interfaces that fall back today (LAGG, dynamic addressing) only if the numbers justify the added risk.
