# Fast HA / warm standby: analysis and status

**Status: not implemented.** This page records what the code says about where failover time goes, what was done towards a faster takeover, and why the central part (keeping the standby's links up with its own MAC and moving the shared identity at takeover) is **not** built yet. The deciding rule: *no speed-up may add any split-brain risk*, and an identity transfer cannot be proven safe without two physical machines and a real switch.

## Where the time goes today

Stage costs read from the code; lab totals are in `docs/validation.md` row 45 (planned failover 0.65-0.85 s of lost traffic, power cut of the active node about 1.5 s, in Hyper-V; real NICs add the link-up time).

| Stage | Cost | Source |
|---|---|---|
| Detection | `detect_time` (default 3 s, minimum 1 s); about 8 heartbeats per period, minimum interval 150 ms, 1 s timeout per beat | `heartbeat.go` |
| Decision | isolation checks (an `ifconfig` call), optional fencing command (up to 30 s) | `autofail.go`, `fence.go` |
| Virtual devices | normally nothing (they stay on the standby); `create_devices.php` only when the definitions changed | `promote.go` |
| Interface reconfigure | one PHP process per interface, parallel per tier (NIC, LAGG, VLAN); a fraction of a second each | `promote.go` |
| Carrier | `waitCarrier` up to 8 s. An administratively down NIC renegotiates its link on a real switch: the dominant term on hardware, invisible in Hyper-V | `devices.go` |
| ARP | gratuitous ARP once the links are up, repeated for a few seconds | `garp_*.go` |
| pf and services | `configctl filter reload` and IPsec/OpenVPN/WireGuard start in parallel; DHCP/DNS in the background | `promote.go`, `pf.go` |

## What exists now (measure before optimising)

* Every promotion records its **phases** (offset in ms from its start: configuration restored, devices ready, interfaces reconfigured, links up, ARP sent, pf and services, ACTIVE) and, for an automatic one, **`detect_ms`**, the silence of the peer when the decision was taken. Both are in `hasyncd ctl history`.
* `lab/failover-bench.sh` measures the outage a client sees (longest run of lost echo requests at a 20 ms interval) over repeated runs and prints median, P95 and maximum.
* The detection time is configurable from 1 s. The heartbeat transport (a TLS request per beat) is not meant for sub-300 ms detection, so a "below 300 ms" target is **not** claimed.

Run on real hardware, these give the numbers the task asks for (last heartbeat, detection, decision, activation, first packet). **No benchmark results exist yet and none are claimed.**

## Design of a warm standby (proposal, not built)

Goal: the standby keeps its NICs and VLANs up and its ruleset loaded, so a takeover is only "take the shared MAC, add the addresses, announce".

1. **Own MAC while standby.** Each node keeps its factory MAC on the data NICs; the shared MAC is set (`ifconfig <if> link <mac>`) at promotion. Two ports must never carry the shared MAC at the same time (MAC flapping in the switch's CAM table). The strict order *demote, then promote* is what the transition protocol of `docs/audit.md` guarantees.
2. **Silent standby.** Links up without any IP address, IPv6 router advertisements and neighbour discovery off, no DHCP client, no ARP or ND for the shared addresses, forwarding off, ruleset for the production interfaces loaded.
3. **Preconditions.** Each must hold or the standby is reported *not ready* and the mode is refused: configuration in sync (`AppliedSHA` equal to the active node's), same OPNsense and plugin versions, pfsync up, ruleset loaded, links up, no drift, switch port-security allowing two MACs for the port group.
4. **Fast path, then slow path.** Fast: re-check the preconditions, set the MAC, add addresses and routes, send ARP, verify forwarding with a probe. Slow, afterwards: service restarts, DHCP/DNS, IPsec re-negotiation, extended checks.
5. **Failure handling as in `docs/audit.md`.** No promotion on ambiguity, no automatic rollback, and a standby that is not prepared falls back to the classic path.

## Why it is not built blind

* The saving is almost entirely the link renegotiation (up to 8 s of `waitCarrier`), and it depends on the NIC driver (ixl on the target hardware), the switch (CAM aging, port-security, LLDP) and the hypervisor (MAC spoofing). None of this is observable in the lab VMs.
* A wrong identity change creates exactly the failure the cluster exists to prevent: two ports with one MAC, or an address answered by the standby. It needs the hardware tests of `docs/validation.md` section G before the code, not after.
* The classic mode is unchanged and remains the only mode.

## Next steps

1. Run `lab/hw-probe.sh` and the benchmark on the target pair to get the real stage costs (the phases are already recorded).
2. Decide from those numbers whether the remaining cost is the link (then a warm standby is worth building) or something cheaper (PHP reconfigure, pf reload) that can be fixed without touching the identity.
3. Build the readiness state (precondition 3) first: it is useful in classic mode as well.
