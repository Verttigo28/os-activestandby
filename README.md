# os-activestandby

**Active/standby high availability for OPNsense, in the style of SonicWall HA.**
One IP and one MAC per interface, shared by two firewalls. The standby sits silent with its data interfaces down and takes over in about a second.

[![ci](https://github.com/Verttigo28/opn-betterHA/actions/workflows/ci.yml/badge.svg)](https://github.com/Verttigo28/opn-betterHA/actions/workflows/ci.yml)
[![license](https://img.shields.io/badge/license-BSD--2--Clause-blue.svg)](LICENSE)

> Status: validated in a Hyper-V lab (OPNsense 26.7). Not yet validated on physical NICs and switches, see [validation](docs/validation.md). Review the [security notes](docs/security.md) before putting it in front of production traffic.

## Why

OPNsense's built-in HA uses CARP: every interface needs three addresses, the standby keeps answering on its own addresses, and the configuration sync is partial. This plugin takes the model most appliance firewalls use instead:

|  | CARP (built-in) | os-activestandby |
|---|---|---|
| Addresses | virtual IP per interface + one real IP per node | **one IP and one MAC per interface, identical on both nodes** |
| Standby | interfaces up, answers ARP/ping, runs services | **all data interfaces down, IPsec/gateway monitoring stopped** |
| Configuration | selected areas via XMLRPC | **whole configuration**, numbered revisions with rollback on the standby |
| Failover | CARP advertisements | heartbeat on up to two paths, epoch-based split-brain protection |
| Switch view | two MACs per interface | one MAC, moves with the active node |

## Features

**Core**
- Shared IP/MAC (generated virtual MAC by default), standby fully silent, config locked on the standby.
- Failover: **about 0.7 s of lost traffic for a planned switch, 1.5 s when the active node loses power** (lab, detection time 1 s). Firewall states follow through pfsync.
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
| [Warm standby](docs/fast-ha.md) | experimental mode: standby keeps its links up; design, limits, what is untested |
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
