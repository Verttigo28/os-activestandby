# os-activestandby

SonicWall-style active/standby high availability for OPNsense 26.7+.

- One IP per interface, **identical IP and MAC on both nodes**. No CARP VIPs.
- The standby keeps every data interface down (no addresses, IPsec/gateway monitoring stopped); only the dedicated HA link and an optional
  management port stay up.
- Automatic failover in about 6 s, split-brain protection (two heartbeat paths, epoch, isolation guard), firewall states via pfsync.
- The active node replicates its **whole** configuration (rules, NAT, VPN, users, certs, VLANs, LAGGs, assignments, plugins) as numbered
  revisions with rollback on the standby.

Docs: [plan and progress](docs/PLAN.md) | [operations](docs/OPERATIONS.md) | [build and install](docs/BUILD.md) |
[phase 0 findings](docs/PHASE0-FINDINGS.md). Lab tooling and notes: [lab/README.md](lab/README.md).
