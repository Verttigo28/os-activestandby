# Sandbox: two nodes on one machine

A FreeBSD/OPNsense machine is enough to run the real daemons against each other. Two vnet jails are the cluster nodes, a third
one is a client, three bridges are the switches (data, HA link, management). The jails share the file system but have their own
network stack, so **the machine's real interfaces are invisible to them**; the daemons run with `HASYNC_SCRIPTS` (stub scripts,
no OPNsense PHP) and `HASYNC_PATH` (shims for `configctl`, `pfctl`, `pluginctl`, `pkill`, `service`, `daemon`, `shutdown`, `date`),
so the real configuration and services are untouched. The shims log what was asked and can be told to fail (`pf_disabled`,
`filter_fail`, `iface_fail_<key>` files in `$SBX/<node>/`).

What it proves: the real pairing over mTLS, heartbeats on two paths, configuration sync, the transition protocol, split-brain,
the election and "peer lost" rules, the classic and the warm takeover against a real kernel, bridge MAC learning, gratuitous
ARP through bpf, and a double-active detector over the whole run. What it does **not** prove: anything about a real NIC driver,
a real switch, OPNsense's own PHP (stubbed) or timing on hardware (an epair has no link negotiation).

    # on the machine, as root: put the FreeBSD build at /root/sbx/hasyncd, then
    sh up.sh classic      # or: warm
    sh scenarios.sh       # prints PASS/FAIL per check and a result line
    sh down.sh            # or: sh down.sh clean
