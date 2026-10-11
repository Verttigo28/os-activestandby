# Changelog

All notable changes. Versions follow `vX.Y.Z` tags; the package version comes from the tag.

## 0.5.6
Security release (full audit of the daemon, the GUI and scripts, the CI and supply chain):
- **Loose umask** (high): the daemon could inherit umask 0 from whatever started it, so the PHP helpers it runs created world-writable files such as `php.ini`, `monitrc` and `dnsmasq.conf`. The daemon now fixes its umask, creates its control socket under a strict umask, and the lab deploy no longer leaves 777 directories.
- **Secrets on the command line** (high): the kit token and the pairing token are no longer visible in `ps` (environment variable and a root-only file).
- **Settings could corrupt the daemon configuration** (medium): every value is rendered with `tojson`, all text fields have anchored masks (no trailing-newline bypass), the daemon validates ports and device names and reports a broken file.
- **Peer API hardening** (medium): once paired only the peer's addresses are served, connections are capped and must handshake quickly; peer-controlled data is bounded (clock steps limited to 5 minutes, maintenance stamps validated and capped, event flooding rate-limited, notification processes capped, job hand-off restricted to its one legitimate form, request sizes reduced, one package install at a time).
- **Symlink attacks** (medium): privileged writes use exclusive creation and refuse planted links; session files and the update status file are only read when they are regular files; the fencing command and its directories are checked without following links.
- **GUI**: operator and administrator privileges are separate (kit download, settings, setup and break pair need the administrator privilege), a GET can no longer write a false audit line, raw helper output is never shown to the browser, the wizard's HA-link firewall rules are limited to the peer API, ping and pfsync from the HA segment, the webhook cannot target loopback or link-local addresses.
- **Supply chain**: all actions pinned to commit SHAs, the signing key reaches only the FreeBSD build step, the package job builds from its own checkout, tag and version strings are validated, the release notes carry the signing key fingerprint, credentials of the lab helper no longer appear on command lines.

## 0.5.5
- The pkg repository is signed (key fingerprint in the README); releases are published together with a signed build attestation.

## 0.5.4
- **Shared settings**: the plugin settings (timing, health, probes, fencing, notifications, preemption, virtual MAC, session sync) are now replicated from the active node. Only whether the service is enabled, the HA and management interface choice and the peer port stay per node. The standby applies a change by itself (renders its daemon configuration and restarts the daemon once, about a second) and no longer needs unlocking to be configured.

## 0.5.3
- **Management probe targets** (new setting): both nodes ping, for example, the management switch from their management address. A standby that cannot reach them will not take over (it may be the isolated one), an active node does not hand over for a health problem when the standby cannot reach them either, the result is in the heartbeat, the status details and the pre-flight.
- CI: staticcheck now runs with the Go version that compiles the code (the previous run failed on a toolchain mismatch).

## 0.5.2
- The status page now also reloads when the answers stop and come back (failover outage) or when the new node answers without your login; the poll runs every 3 s and never loops.
- Removed "Force this node active", "Force this node standby" and "Full resync" from the Status page: *Failover now* is the one button (the emergency takeover stays on a standby whose active node is silent). The matching demote API was removed.
- CI runs staticcheck; dead code removed.

## 0.5.1
- Both firewalls are named in the status tiles, the details and the dashboard widget, with who is active and who is standby, primary or secondary.
- The page reloads by itself when the firewall answering the shared address changes (failover) or this one changes role, so it never shows the old node's buttons.

## 0.5.0

### Added
- GUI logins survive a failover: sessions are replicated to the standby (setting *Keep GUI logins across a failover*, on by default).
- New status tiles, a standby-only view with a single takeover button, a status-bar notice on the standby.
- CI lints PHP, XML, JavaScript and shell; unit tests run with the race detector; releases carry SHA-256 checksums.
- Security policy, contributing guide, architecture and security documentation.

### Changed
- Source split into smaller files (`promote.go`, `demote.go`, `enforce.go`, `devices.go`, `boot.go`); documentation restructured.

## 0.4.3
- The guided upgrade is offered only when OPNsense reports updates; *Check for updates* button.
- Upgrade job fixes: round 2 waits for the old node's demote; a handed-over job no longer blocks the next upgrade.
- The primary no longer gathers node information every few seconds while waiting for a kit-built secondary.

## 0.4.2
- **Security:** pairing hello is token-authenticated and the confirm step is pinned to the certificate that answered (a man-in-the-middle on the HA link could otherwise get its certificate pinned); per-source lockout, bounded state, server timeouts; read-only GUI privilege; audit of settings and service changes; webhook without redirects; package names from the peer validated; update log moved out of `/tmp`; `hasyncd.json` 0600.
- Promote hardening: VLAN parents and LAGG members come back up, disabled and late-created (PPPoE) interfaces handled, a failed promote returns to the standby view, one switchover at a time, no health switching for 30 s after a promote.
- Virtual MAC by default; probe targets; audit trail of manual actions.

## 0.4.1
- Failover speed: planned about 0.7 s, power cut about 1.5 s (was about 6 s). Parallel interface bring-up, one DNS/DHCP restart, overlapped hand-over, configurable detection time.

## 0.4.0
- Maintenance mode, health monitoring, notifications (syslog, webhook, Monit), pre-flight with dry-run failover, DHCP lease sync, fencing hook, preemption, Prometheus metrics, failover history, drift detection, guided rolling upgrade, secondary kit with automatic pairing.

## 0.3.0 and earlier
- Shared IP and MAC with a silent standby, whole-config replication with revisions and rollback, Go daemon with mutual-TLS pairing, split-brain protection, package replication, installable package, CI.
