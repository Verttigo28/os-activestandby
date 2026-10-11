# Changelog

All notable changes. Versions follow `vX.Y.Z` tags; the package version comes from the tag.

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
