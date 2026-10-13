# os-activestandby

**Active/standby high availability for OPNsense, in the style of SonicWall HA.**
One IP and one MAC per interface, shared by two firewalls. The standby sits silent and takes over in **0.1-0.2 s for a planned switch and about 3 s after a crash** in the hot mode (measured, see below).

[![ci](https://github.com/Verttigo28/opn-betterHA/actions/workflows/ci.yml/badge.svg)](https://github.com/Verttigo28/opn-betterHA/actions/workflows/ci.yml)
[![license](https://img.shields.io/badge/license-BSD--2--Clause-blue.svg)](LICENSE)

> Status: validated on **two Supermicro servers (Intel X710) with a real switch** (MikroTik CRS305 with real LACP bonds, 20 VLANs on a LAGG, the production configuration of a pfSense converted to OPNsense), with a client pinging every 20 ms through the switch: failover, crash, dead card, cold start, LACP. Earlier steps: a Hyper-V lab and the real daemons against each other on FreeBSD 15 ([sandbox](lab/sandbox/README.md)). Not yet validated: a production switch (Meraki, Juniper EX4600 and others: see the checklist in [operations](docs/operations.md#switch-checklist)), a power cut with a real PDU. TCP sessions kept across a failover were validated (section J5). Review the [security notes](docs/security.md) before putting it in front of production traffic. The hot standby is experimental.

## Why

OPNsense's built-in HA uses CARP: every interface needs three addresses, the standby keeps answering on its own addresses, and the configuration sync is partial. This plugin takes the model most appliance firewalls use instead:

|  | CARP (built-in) | os-activestandby |
|---|---|---|
| Addresses | virtual IP per interface + one real IP per node | **one IP and one MAC per interface, identical on both nodes** |
| Standby | interfaces up, answers ARP/ping, runs services | **all data interfaces down, IPsec/gateway monitoring stopped** |
| Configuration | selected areas via XMLRPC | **whole configuration**, numbered revisions with rollback on the standby |
| Failover | CARP advertisements | heartbeat on up to two paths, epoch-based split-brain protection |
| Switch view | two MACs per interface | one MAC, moves with the active node |


## Measured failover (two Supermicro servers, Intel X710, real switch)

A client behind the firewalls pings every 20 ms through a MikroTik CRS305; 20 VLANs on a LAGG, the production configuration of a pfSense converted to OPNsense (207 rules, 91 NAT rules). Outage = what the client sees. Details and method: [validation](docs/validation.md) sections J to J3.

| Scenario | Classic, failover LAGG | Classic, LACP | **Hot**, failover LAGG | **Hot**, LACP |
|---|---|---|---|---|
| Promotion (21 interfaces) | 3.1-3.8 s | 2.3-3.2 s | 0.18-0.22 s | 0.18-0.22 s |
| Planned failover | 1.0 s | 12.5 s | 0.12-0.16 s | 0.10-0.18 s |
| Crash of the active node | 4.9 s | 9.6 s | 2.8 s | 3.0 s |
| Dead card (both ports cut) | 5.1 s | - | - | 3.1 s |

For comparison, CARP (from its design, not measured here): about 3 to 6 s for a node failure. See [Warm and hot standby](docs/fast-ha.md#hot-standby-the-takeover-that-adds-addresses-and-nothing-else).

## Features

**Core**
- Shared IP/MAC (generated virtual MAC by default), standby fully silent, config locked on the standby.
- Failover, measured on two Supermicro servers with a real switch (client view, 20 ms probes): **classic mode 1.0 s planned / 4.9 s crash** (failover LAGG), 12.5 s / 9.6 s with an LACP LAGG; **hot mode 0.1-0.2 s planned / about 3 s crash**, LACP included (3 s of it is the detection time). Firewall states follow through pfsync.
- Split-brain protection: two heartbeat paths, epochs, isolation guard, optional fencing hook, boot guard (a rebooted node always starts as standby).
- Whole-config replication with revisions, integrity checks and drift detection. Plugins and DHCP leases follow the active node.
- **GUI logins survive a failover**: sessions are replicated, so you stay logged in on the new active node.

**Operations**
- Setup wizard, and a **secondary kit**: download one file from the primary, restore it on a fresh firewall, and the pair forms by itself.
- Maintenance mode, pre-flight check with dry-run failover, guided rolling upgrade (offered only when updates exist), preemption (optional).
- Health monitoring of the active node (carrier, services, probe targets) with anti ping-pong guards.
- Notifications (syslog, webhook, Monit e-mail), Prometheus metrics, failover history, audit trail of every manual action.

## Quick start

1. Install the package on both machines (same OPNsense version, same hardware):
   ```sh
   pkg add os-activestandby-<version>.pkg
   ```
2. On the primary: **System > Active/Standby HA > Setup wizard**, pick the HA link (a dedicated cable, static IPv4) and a management interface (strongly recommended).
3. Step 3 **Automatic**: enter hostname and HA address of the second firewall, download the kit, restore it on the second firewall (System > Configuration > Backups), reboot it. Pairing happens on its own.

   Or pair manually with a one-time token (wizard on both nodes).
4. Check **Operations > Pre-flight**. Done: edit the configuration on the active node only.

Details: [getting started](docs/getting-started.md), [operations](docs/operations.md).

## Documentation

| | |
|---|---|
| [Getting started](docs/getting-started.md) | requirements, installation, the wizard, the secondary kit |
| [Operations](docs/operations.md) | day-to-day use, failover behaviour, updating, monitoring, troubleshooting |
| [Architecture](docs/architecture.md) | roles and state, failover pipeline, replication, design decisions |
| [Security](docs/security.md) | threat model, controls, hardening, reporting vulnerabilities |
| [Validation](docs/validation.md) | what was tested, how, and what still needs hardware |
| [Audit](docs/audit.md) | failover safety audit: findings, fixes, the limits of a two-node cluster |
| [Sandbox](lab/sandbox/README.md) | two real nodes on one FreeBSD machine: the scenarios, what they prove and what they cannot |
| [Warm and hot standby](docs/fast-ha.md) | experimental modes: the standby keeps its links up (hot: already with the shared MAC, takeover in about 0.2 s, LACP included); design, measurements, comparison with CARP |
| [Building](docs/building.md) | build, CI, releases |
| [Roadmap](docs/roadmap.md) | what is open |
| [Changelog](CHANGELOG.md) | release history |

## Package signing

Releases carry a signed build attestation and SHA-256 checksums. The pkg repository on GitHub Pages is signed with this key (RSA 4096, public key in [keys/activestandby.pub](keys/activestandby.pub)); compare its SHA-256 fingerprint before trusting it:

```
be5ef3e668d8079010f598b062f71dab064c183433faf98b7a30a6d03b6da411
```

Setup: [docs/building.md](docs/building.md#signing).

## Requirements and limits

- OPNsense 26.7 or newer (FreeBSD 15), two machines with identical NIC names, same OPNsense and plugin versions. Pairing refuses anything else.
- Static IPv4 on the shared interfaces. PPPoE and IPv6 failover are not supported, DHCP on the WAN is best effort; see [operations](docs/operations.md#requirements-and-limitations-in-production).
- Not for use together with the built-in CARP/XMLRPC HA.
- Releases carry a signed build attestation and SHA-256 checksums; a signed pkg repository is supported, see [building](docs/building.md#signing).

## Repository layout

```
daemon/    hasyncd, the Go daemon and control tool (standard library only)
src/       files installed under /usr/local: GUI (MVC), configd actions, PHP helpers, rc script, widget
build/     package build (FreeBSD pkg), Windows driver
docs/      documentation
lab/       Hyper-V lab tooling used for the validation (not part of the package)
```

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md).

## License

BSD 2-Clause, see [LICENSE](LICENSE).
