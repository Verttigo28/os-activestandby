# Sandbox: two nodes on one machine

A FreeBSD/OPNsense machine is enough to run the real daemons against each other. Two vnet jails are the cluster nodes, a third
one is a client, three bridges are the switches (data, HA link, management). The jails share the file system but have their own
network stack, so **the machine's real interfaces are invisible to them**. The daemons run with `HASYNC_SCRIPTS` (stub scripts,
no OPNsense PHP) and `HASYNC_PATH` (shims for `configctl`, `pfctl`, `pluginctl`, `pkill`, `service`, `daemon`, `shutdown`,
`date`, `ifconfig`), so the real configuration and services are untouched. The shims log what was asked and can be told to fail
with flag files in `$SBX/<node>/`: `pf_disabled`, `filter_fail`, `iface_fail_<key>`, and `nocarrier_<dev>` (the shimmed `ifconfig`
then reports "no carrier" for that device: an `epair` never loses its carrier, this is how a dead card or a pulled cable is
simulated). Both overrides are only honoured for directories that root owns and nobody else can write to.

**Run it on a test machine only.** It needs root, creates jails and interfaces, and sets one global sysctl
(`net.link.bridge.pfil_member=0`, so that the machine's own pf does not filter the bridged traffic; `down.sh` restores it).

## What it proves

The real pairing over mutual TLS, heartbeats on two paths, configuration replication, the transition protocol, split-brain
resolution, the election and "peer lost" rules, a promotion refused when pf does not work, a frozen peer and late requests,
partitions, packet loss, a dead monitored interface, the classic and the warm takeover against a real kernel, bridge MAC
learning, gratuitous ARP through bpf, and a **double-active detector** over the whole run: both nodes are sampled every 100 ms
and two consecutive samples with the shared identity (MAC and address, interface up) on both nodes are a violation.

## What it does not prove

Anything about a real NIC driver (an `epair` has no link negotiation), a real switch (no CAM aging, no port security), OPNsense's
own PHP (replaced by stubs), LACP with a real partner, or timing on hardware. Use `../warm-probe.sh` and `../hw-probe.sh` for the
NIC, and the hardware lists in `docs/validation.md` for the rest.

## Use

    # on the machine, as root: put the FreeBSD build of hasyncd at /root/sbx/hasyncd (and a newer ifaces.php at /root/sbx/ifaces.php.new if any), then
    sh up.sh classic            # or: warm
    sh scenarios.sh classic     # PASS/FAIL per check, a result line at the end; add section numbers to run only some: scenarios.sh classic 2 4
    sh run-all.sh               # both modes, one log per mode (about 15 minutes each)
    sh down.sh                  # or: sh down.sh clean

| # | Scenario |
|---|---|
| 1 | pairing, steady state, a silent standby |
| 2 | graceful failover both ways with client traffic |
| 3 | daemon restarts (standby, active) |
| 4 | power cut of the active node, then its return |
| 5 | split-brain forced on purpose |
| 6 | pf failure at promotion (`pf_disabled`, `filter_fail`) |
| 7 | warm: standby not ready, non-silent port at takeover |
| 8 | the peer dies during a hand-over |
| 9 | configuration replicated |
| 10 | HA path lost, management lost, standby that cannot prove it is not isolated |
| 11 | symmetric partition of the two nodes (the documented limit) |
| 12 | crash of the active daemon for 6 s |
| 13 | 20 % packet loss on the peer link |
| 14 | a peer frozen for 70 s, its late request |
| 15 | a monitored interface of the active node loses its carrier |
