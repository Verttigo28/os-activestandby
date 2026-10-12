# Warm standby (experimental)

**Status: implemented, opt-in, never run on real hardware.** The setting *Standby mode* = *Warm standby* turns it on; the default is *Classic* and nothing changes unless an administrator chooses it. It was built from the code and tested against a model of `ifconfig` and a switch (see the test list below), not on OPNsense, a NIC driver or a switch. Expect to tune it with real measurements (`docs/validation.md` section H). The rule behind every decision: *no speed-up may add any split-brain risk*; where that could not be shown, the classic path is kept.

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

## Where the time goes (what to measure)

| Stage | Cost | Source |
|---|---|---|
| Detection | `detect_time` (default 3 s, minimum 1 s); about 8 heartbeats per period, minimum interval 150 ms | `heartbeat.go` |
| Decision | isolation checks, optional fencing command (up to 30 s) | `autofail.go`, `fence.go` |
| Interfaces | classic: PHP per interface plus up to 8 s of carrier wait; warm: native `ifconfig`, verified | `promote.go`, `warm.go` |
| ARP | gratuitous ARP once the identity is set, repeated for a few seconds | `garp_*.go` |
| pf and services | `filter reload` and VPN start in parallel; DHCP/DNS in the background | `promote.go`, `pf.go` |

Each promotion records its phases (offset in ms) and, for an automatic one, `detect_ms` in `hasyncd ctl history`; a promotion that used the fast path has the phase "shared identity taken natively". `lab/failover-bench.sh` measures the outage a client sees. **No benchmark results exist yet and none are claimed**; the detection time is configurable from 1 s but the heartbeat (a TLS request per beat) is not meant for sub-300 ms detection, so no such target is promised.

## Next steps with real data

1. Run section H of `docs/validation.md` on the target pair (Classic and Warm, several runs each) and compare median, P95 and maximum.
2. From the phases decide what still costs time: if it is routes or `filter reload`, run them natively or in parallel; if the switch learns slowly, add more announcements or a different frame.
3. Decide whether OPNsense's interface state needs a reconcile step after the fast path, and what it may safely run without a link flap.
4. Extend to the interfaces that fall back today (LAGG, dynamic addressing) only if the numbers justify the added risk.
