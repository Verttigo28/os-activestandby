# Changelog

All notable changes. Versions follow `vX.Y.Z` tags; the package version comes from the tag.

## 0.9.0
Found by running the daemons against each other on a real FreeBSD 15 machine (`lab/sandbox`) and by measuring the NICs of the target hardware.

### Failover on a failed network card (behaviour change)
- **A dead card or a LAGG that lost all its ports now causes a failover on VLAN-on-LAGG and on physical NICs.** Before, the active node only switched when the standby *read* link on its own port; a standby keeps its ports down, so a VLAN on a LAGG (which follows its parent) or a NIC that drops its link when administratively down always read "no link" and the switch was refused. The standby now tells the active node which links it could not read (`links_unknown`); only a link it read and found down stops the switch. The two-switches-in-30-minutes limit and the 5 minute cool-down still apply.
- Pre-flight warns when no interface, service or probe target is monitored (a dead card then causes no failover). `deploy/apply-settings.sh` takes `MONITOR_IFS` and `PROBE_TARGETS`; the help of *Monitored interfaces* says how to use it with a LAGG.

### Recovery time and late requests
- A promote request is only valid for 10 s (the peer refuses it 5 s later), so a request that sat in a frozen peer's socket cannot be executed after the sender has moved on. The wait for the answer is 45 s (was 90 s), and it ends early when nobody answers and no heartbeat arrives on any path. A peer that dies during a hand-over now leaves the survivor without service for about 18 s (was 54 s), a peer frozen for 70 s about 48 s (was about 91 s).

### A frozen daemon
- **A daemon that was frozen (SIGSTOP, a paused or migrated VM) no longer promotes the moment it wakes up.** It woke with a heartbeat clock saying "the peer has been silent for 95 s", promoted at once (1.2 s of double active in the sandbox) and won the arbitration. Its loop now notices that it did not run for 3 s and restarts the detection from the wake-up.

### Fixes
- The pfsync supervision read one setting per line; FreeBSD 15 prints `syncdev`, `syncpeer`, `maxupd` on one line, so it re-applied pfsync every 10 s and filled the event log. Fixed and tested against the real output.
- Warm standby: a VLAN on a LAGG (or a bridge, or another VLAN) is never warm, an all-zero MAC is never taken for a hardware MAC, and addresses, prefixes and MTU of the plan are validated in Go as well.
- The environment overrides used by the sandbox (`HASYNC_SCRIPTS`, `HASYNC_PATH`) are only honoured for directories owned by root and not writable by others.
- `deploy/apply-settings.sh` URL-encodes every value and honours exported settings.

### Measured on the target hardware (Supermicro, Intel X710 and I210, FreeBSD 15.1)
- **The warm standby gains nothing on these NICs.** Changing the MAC with the link up makes the link drop for about 0.74 s (X710) to 0.9-1.9 s (I210), as much as an administrative down and up (0.8 s and 1.6 s). The X710 keeps its link while administratively down (`link_active_on_if_down`), so the classic standby does not renegotiate either. Keep the classic mode; details in `docs/fast-ha.md`.

### Tools
- `lab/sandbox`: two real daemons on one FreeBSD machine (vnet jails, epair, bridges as switches), 15 scenarios with a double-active detector (pairing, graceful and automatic failover, split-brain, pf failure, partitions, frozen peer, packet loss, dead card). `lab/warm-probe.sh` measures the NIC behaviour the warm standby depends on.

## 0.8.2
Release pipeline fix: publishing the signed pkg repository to `gh-pages` failed because the publish step listed `.git` in `exclude_assets`, which the action applies to its own clone of the branch (it deleted the repository it had just cloned). No daemon or plugin change.

## 0.8.1
Superseded by 0.8.2 (the attempted fix was not the cause).

## 0.8.0
Warm standby (experimental, opt-in; see `docs/fast-ha.md`).

- New setting *Standby mode*: *Classic* (default, unchanged) or *Warm standby*. In warm mode the standby keeps the links of its eligible interfaces (static IPv4, shared MAC, plain NIC or VLAN) up with its own MAC, no address and IPv6 off; a takeover sets the shared MAC and the addresses natively and verifies them, any problem falls back to the classic takeover before traffic starts. A stand down gives the own MAC back first, then removes the addresses; the link is never taken down.
- Readiness: `warm` in the status, `peer_warm` from the heartbeat, pre-flight checks and a GUI banner say whether a fast takeover is possible and why not; it never blocks a failover.
- `ifaces.php plan` (native plan of the interfaces), `hwmacs.json` (permanent MAC memory).
- Tested against a model of two hosts and a switch (never two ports with the shared MAC); **not tested on OPNsense or hardware**, procedure in `docs/validation.md` section H.

## 0.7.0
Failover safety release (audit in `docs/audit.md`).

### Safety fixes (behaviour changes)
- **No more rollback on a lost answer.** A graceful failover used to take its role back whenever the peer's reply to "promote" failed, which created two active nodes if the peer had in fact promoted. Hand-overs now carry a transition id and the sender's epoch; the old node takes its role back only after the peer confirmed that it did not and now cannot take over. If the outcome cannot be verified the old node stays standby and reports an error (nothing serves traffic until the election, the "peer lost" rule or an operator decides). The peer refuses stale, duplicated and cancelled promote requests. Peer API: new `POST /v1/abort`; `/v1/promote` takes `tid` and `epoch` (older daemons simply ignore them).
- **Split-brain is resolved immediately.** It is no longer delayed by the 10 s anti-flapping hold, and it is also resolved while maintenance mode is on or **automatic failover is switched off** (a double active is never wanted). A peer that reports "busy" is excused for at most one minute.
- **A promotion is not reported as done when pf is not working.** The ruleset reload is retried and verified (`pfctl -si`); on failure the node goes back to its standby view and reports the error instead of staying "active" without a firewall. Failed static routes mark the node *degraded*.

### Supervision
- Status: `degraded`, `pfsync_problem`, `package_diff`, `failover_in_progress`, shown in the status page banner. pfsync is verified every 10 s and repaired when missing; plugin differences between the nodes (missing, other version, extra) are reported.
- History: every promotion records its phases and, for automatic ones, how long the peer had been silent.

### Tests and docs
- New failover/election/split-brain tests with a fake system and a lossy peer link; `docs/audit.md` (findings, limits of two-node clusters), `docs/fast-ha.md` (analysis of the failover time and the design of a warm standby, not built), validation section G, `lab/failover-bench.sh`.

## 0.6.0
Production-readiness and usability release (review of every feature, setting and screen).

### Production fixes
- A takeover now waits for the link of real NICs (carrier, up to 8 s) before it announces the MAC, repeats the announcement for a few seconds and only then starts gateway monitoring.
- OpenVPN server/client interfaces (`ovpns`, `ovpnc`) no longer make a takeover fail. An HA or management VLAN/LAGG that shares its trunk with data VLANs is no longer cut on the standby.
- A boot that could not write the standby view (disk full, read-only config) is still guarded: every paired node comes up silent. A demote refuses when the saved configuration lacks the HA settings (restored backup) instead of cutting the HA link.
- When the HA link is down but the management path is alive, switchovers and hand-overs use the management path instead of failing after the interfaces went down.
- The state file no longer blocks heartbeats while it is written to disk (a slow disk could cause a false failover); the standby no longer re-reads its saved configuration on every heartbeat. Default detection time is now 3 s.
- Services that bind to interface addresses (NTP, captive portal, Suricata, HAProxy, Dynamic DNS) are restarted after a takeover when installed; WireGuard is kept down on the standby.
- Newly pinned shared MACs are applied on the running node (the pattern that finds the interface keys was broken).
- The wizard and the pre-flight warn when there is no management interface (no automatic takeover after a power loss over a direct cable). Documented requirements and limitations (PPPoE, IPv6, switch settings).

### Usability
- Settings grouped under headings with clearer names and help, shared versus per-firewall settings marked, monitored interfaces chosen from a list, stricter IP validation, warnings on risky settings.
- Wizard: confirmation before a firewall becomes the secondary and before the kit download, no preselected role, automatic checks, remembers the saved choice, countdown and cancel for the pairing window; hidden once paired.
- Status page: clear state when the daemon is not running or the firewall does not answer, no reload that throws away the wizard or unsaved settings, a maintenance switch on the standby page, double-click protection on failover and save, clearer confirmations, full certificate fingerprint.
- Operations: feedback for maintenance mode, readable pre-flight (failures first), confirmation for cancelling an upgrade, real polling for the update check, empty states.
- Event log readable in the dark theme; status-bar notice also flags maintenance mode, failed sync, a silent peer, drift and version differences; dashboard widget no longer leaves stale rows and reports a stopped daemon.
- Separate operator and administrator privileges are documented under their exact names.

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
