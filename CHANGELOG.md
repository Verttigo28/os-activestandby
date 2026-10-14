# Changelog

All notable changes. Versions follow `vX.Y.Z` tags; the package version comes from the tag.

## 1.0.5
An independent audit of 11372fc, then two independent reviews of the whole repository; see [audit](docs/audit.md#eleventh-round-105-the-audit-of-11372fc). Each finding was checked in the code before it was acted on; the HA findings were reproduced by a test that failed before the fix.
### Fixed
- **The upgrade assistant did not update the nodes** (reported from the lab: updates still pending on a node after the "finished" job, nodes on different plugin versions). The update of the standby was `opnsense-update` followed by `pkg upgrade`, which never replaces the kernel and the base system, and "success" was the exit code of the last command. It now runs the update of the firmware page of OPNsense (`launcher.sh update`: packages, base system, kernel, reboot) through the proxy of the active node, reads its outcome from its progress file (`***DONE***`, `***REBOOT***`, "Partial update failure"), accepts that the standby reboots by itself, and **never ends a round on the word of a log**: the standby runs the update check again and the round fails if anything is still pending (`r1-verify`, `r2-verify`).
- **Two nodes on two plugin versions**: the plugin is not in the OPNsense repositories, so no firmware update ever touched it. The older node now fetches the package of the newer one over the pinned mutual-TLS connection (`pkg create` on the sender, `pkg add -f` on the receiver after checking name, origin and architecture of the package): button "Synchronize plugin version" (Operations), `hasyncd ctl plugin-sync`, and a last step of the guided upgrade (before the optional failback) that does not end the job until both nodes run the same version. The GUI shows both versions. A peer older than this function answers 404: the message says to install the package on it once with `pkg add`.
- **The daemon of the active node dead or hung while its system still forwards** (killed with its supervisor, `SIGSTOP`, stopped by hand): the standby took the silence for a dead peer and both nodes held the shared IP and MAC. Before a takeover after a silence the standby now pings the peer system on the HA link and on management: an answer means the machine runs, and without a fencing command the standby does not take over (it says why). **Verified on the two Supermicro servers**: with the daemon of the active node frozen for 20 s the standby refused; a real crash of the whole machine (`reboot -q`) was still taken over (detection 4.5 s) and the crashed node came back as standby.
- **Plugin installation against a promotion**: a promotion that started between the role check of an installation and its registration found nothing to stop (reproduced by a test). The check and the registration now happen under the lock the promotion takes; a second installation is refused instead of waiting behind one that does not end.
- **Hand-over between two standby nodes**: each asked the other and held its failover lock while waiting, a loop that blocked the election of both. Only an active node answers a hand-over, and a standby asks only an active peer.
- **Boot**: a paired node that is "active" on disk at the first start of a boot (the standby path of the boot failed: full or read-only `/conf`) goes to standby; the boot no longer copies `config.xml` over `active.xml` while a demotion is recorded; the role is saved before the standby view (boot, demotion, recovery), so a power loss in between cannot turn the saved configuration into the view.
- **Demotion**: a node that cannot save its configuration (`/conf` full) still stands down (a split-brain loser stayed active); the role is saved as soon as the devices are down; the background work of the last promotion no longer delays taking the identity down.
- A missing TLS identity on a paired node is treated as replaced (the node goes to standby); the epoch of a takeover is above the highest leader epoch known; maintenance changes are compared as times; the fencing command runs in its own process group; a configuration apply checks pf after the reload; the window in which an update may write the configuration of a standby survives a restart of the daemon.
- **Different HA or management interface keys on the two nodes** are refused at pairing and at each sync, instead of putting the active node's HA rules on another interface of the standby (which cut its HA link).
- The package install script leaves an existing maintenance window alone and closes only the one it opened; it lives in `build/pkg-scripts` and is checked by ShellCheck.
- **The maintenance window of a plugin installation stayed open for an hour** when the installing session ended, or when the daemon that ran the installation (plugin synchronisation) was restarted by it: the closing of the window ran in the process group of the install. It is detached (`daemon(8)`) now. Found on the lab, verified there with an ssh session that ended right after `pkg add`.
### CI
- staticcheck and govulncheck also for FreeBSD (the binary that ships); ShellCheck covers the syshooks and the package scripts.
### Tests
- Deterministic interleavings of an installation and a promotion (both orders, an installation that ignores its cancellation); a dead daemon on a live system with and without fencing; a hand-over between two standbys; the first start of a boot; the takeover epoch; maintenance ordering; the unlock window across a restart; different interface keys.

## 1.0.4
A third pass over the audit brief (`todo/version-1.x.md`), on the points left open in 1.0.3; see [audit](docs/audit.md#ninth-round-104-the-points-left-open).
### Fixed
- **Audit of ba21e4d** (an external one, then two independent reviews of the whole repository; see [audit](docs/audit.md#tenth-round-104-the-audit-of-ba21e4d)):
  - a late heartbeat of the PREVIOUS run of the peer daemon was taken after the new one; the incarnation now only goes up (saved, independent of the clock);
  - a transition that could not be recorded ran anyway: the record is now also kept in `/var/run`, and a promotion that cannot be recorded anywhere is refused;
  - a settings file that is not valid JSON was half used (`fence_required` could silently become false): the last valid settings are used, or automatic failover is off, and the problem is shown;
  - a damaged TLS identity was replaced and the node kept its role while its peer could no longer hear it: the identity is kept twice and restored, damaged files are kept aside, and a node whose identity really had to be replaced goes to standby;
  - a plugin installation could run next to a takeover: it is cancelled by the promotion and killed with its children;
  - **upgrading the plugin left the old daemon running** (pkg does not run the deinstall scripts of the old version, checked on the lab): it is now restarted, inside a maintenance window so that the peer does not take over during the restart;
  - the guided upgrade: the update of the standby is no longer stopped after 2 minutes (a regression of the command bound, found by the review), its status survives a restart of the standby daemon, the running phase has a deadline, and maintenance mode no longer blocks the election of a pair where nobody is active;
  - with different HA or management interface keys on the two nodes, the active node's HA interface arrived on the standby as a data interface (its address came up there at a takeover);
  - commands killed on timeout take their children with them; pfsync commands are bounded; MAC pinning never runs on a standby nor during a role change; the enforcer no longer holds the operations lock to probe services; stored deadlines keep the monotonic clock; the update proxy no longer leaks connections;
  - GUI: the wizard's pfsync rule used a lowercase protocol key where the firewall's keys are upper case (not verified on a GUI run), and a refused rule was skipped silently: the key is fixed and every refusal is reported; the administrator privilege covers the plugin page; an error in a dialog is escaped; the kit refuses impossible HA addresses; the deploy script checks TLS for a remote firewall.
- **A role change that the daemon did not finish is detected at its restart** (write-ahead record `transition.json`, [architecture](docs/architecture.md#commit-points-and-recovery-of-a-role-change)). Before, a daemon killed in the middle of a demotion came back believing it was active with its interfaces down, and one killed in the middle of a promotion left the full configuration in `config.xml`. Now the node goes back to standby (standby view, HA services stopped, devices down) before anything acts on its role. A takeover whose commit cannot be saved keeps the record: a restart brings it back to standby.
- **A demotion that fails after the data interfaces went down** leaves the node standby (it could stay "active" with nothing up).
- **Heartbeats out of order**: a heartbeat that arrives after a newer one (the other path, a late answer, a duplicate) no longer overwrites the state of the peer and no longer counts as a proof of life. Each daemon sends its incarnation with its sequence number.
- **Every external command is bounded in time** (2 minutes): a hung `configctl` or helper no longer holds the operations lock, and with it every role change, for ever.
- The TLS identity is written durably (a damaged one made the node silently create another and the peer refuse it); the standby view is created exclusively (no planted link followed).
### Changed
- **Times in the GUI are in the time zone of the firewall** (events, failover history, end of the maintenance mode, widget), with the zone in the column header, e.g. "CEST, UTC+02:00", and the offset of each date (summer/winter time). Stored times stay in UTC. A change of the zone is followed without restarting the daemon.
- The peer API routes are declared in one table: every route requires the pinned peer certificate, every route that changes something POST.
- Status and metrics: an unfinished transition, heartbeats out of order, the protocol, `fence_required`.
### Tests
- Recovery of interrupted promotions and demotions (including a record that cannot be read, a disk that refuses, a standby view that cannot be written); latency on the scale of the protocol (an answer after the requester gave up, a request after its validity) with the network-identity invariant; no goroutine left after 40 lossy failovers; heartbeat ordering; a hanging command; authorization of every peer endpoint; local times with summer and winter time.
### Documentation
- Commit points and recovery of a role change, error categories ([architecture](docs/architecture.md)); the frames a standby may send in each mode ([fast-ha](docs/fast-ha.md#what-a-standby-may-send-per-mode)).

## 1.0.3
Release of the work of 1.0.2 (the tag v1.0.2 was never built: its workflow was invalid) plus the findings of a second audit; see [audit](docs/audit.md#eighth-round-103-an-external-audit-of-102).
### Added
- **Protocol version** in the heartbeat and on the configuration push. A node whose peer speaks a newer protocol refuses a configuration or ruleset push and a graceful failover and says which node to update; the pre-flight fails; the automatic takeover and the split-brain resolution are never blocked by it.
- **Leadership generation on `/v1/config`**: `X-Epoch`, checked against the highest epoch accepted from this peer (`leader_epoch` in the state file, reset when a pair is formed). Plugin 1.0.1 and older senders are still accepted (rolling upgrade).
### Fixed
- The enforcer no longer returns silently when a standby cannot read its saved configuration: it falls back to the configuration in use and reports it; the rollback of a failed configuration push reports its own failures.
- CI: valid workflow, `govulncheck` v1.8.0, `staticcheck` 2026.2.2, Go 1.27.2, checksums of the daemon binary verified; two test races fixed.
### Tests
- The property test now checks the network identity (what the fake interface actions did) and runs 40 seeds of 300 steps; protocol and leadership tests.

## 1.0.2
### Fixed and hardened (an independent comparison with the audit brief, see [audit](docs/audit.md#seventh-round-102-gap-analysis-against-the-audit-brief))
- **Role file.** The role is now changed on a copy, checked and saved, and only then published; a failed write (disk full, I/O error, fsync, rename) leaves memory and disk unchanged and returns the error. A node that goes down or takes over follows the machine even if the disk refuses (`UpdateSafe`): the failure is logged and the node is shown as degraded. A state file that is valid JSON but not a state (`{}`, `null`, unknown role) counts as damaged. `Get` returns a deep copy.
- **`/v1/promote`**: `tid`, `epoch` and `exp` are required and strictly parsed; implausible epochs and validities are refused; the promote reason now carries the transition id and the epoch.
- **`/v1/config`**: revision and checksum headers are required and strictly parsed.
- **Services after a takeover** are verified; a failure marks the node degraded.
- **New option `fence_required`** (Settings > Safety, off by default): no automatic takeover caused by a silent peer without a fencing command that succeeds.
- **Go 1.27.2**: `govulncheck` found nine standard-library vulnerabilities (net/http, HTTP/2, crypto/tls) in the Go 1.27.0 that built every earlier release; all fixed in 1.27.2, which now builds the daemon (see [security](docs/security.md)).
- **CI**: ShellCheck is blocking and covers every script (all findings fixed or justified); `govulncheck`, `go mod verify`, a reproducible-build check, a package smoke test, attestation and checksum of the daemon binary.
- **Documentation**: repository name in the install and verification commands, file names, build section, certificate rotation, pfsync exposure, the limit without fencing at the top of the README.
### Tests
- Fault injection for the role file, a table test of malformed promote requests, header tests for the configuration push, a fencing policy table, a seeded property test of the role protocol (at most one active node, epochs never decrease), packet loss of 10/30/50 % with duplicates and late delivery.

## 1.0.1
### Fixed
- **Plugin services were not started on the node that becomes active.** Only NTP, captive portal, Suricata, HAProxy and Dynamic DNS were restarted after a takeover. A service that is enabled in the replicated configuration but was never started on the standby (net-snmp, node exporter, LLDP, ...) stayed stopped on the new active node: found on the test pair after replicating the settings of a pfSense, where none of the three ran after the first failover. After a takeover every service listed by `pluginctl -S` that is enabled and not running is now started (IPsec, OpenVPN, WireGuard, the gateway monitor, DHCP and DNS keep their own handling). The standby had not always regenerated the templates of the plugins either (`/etc/rc.conf.d/snmpd` still said `NO`, so starting the service did nothing): they are regenerated first. Not present in 1.0.0.
- **Pre-flight "standby silence" is aware of the hot standby.** In hot mode the data devices are up on purpose with the shared MAC; the check reported them as a failure. It now requires, for a hot standby, up + no address + IPv6 off + the shared MAC (the transmit watch and the default-deny check remain the guarantee).

## 1.0.0
First release meant for production use (starting with a small data centre). Everything below was measured on two Supermicro machines with Intel X710, a real switch and real LACP bonds ([validation](docs/validation.md) sections J to J4); the hot standby stays experimental and off by default, the classic mode is the default and the reference.

### Ruleset mirror (hot standby)
- **The standby holds the packet filter ruleset of the active node.** Without it a new connection during a takeover waited about 1 s for the ruleset to be generated (21 connections needed the client's SYN retransmit in the first 0.85 s); with it 2-3 in the first 0.08 s. The active node sends what it has loaded over the pinned TLS connection; the standby checks it (default deny, size, `pfctl -n`), keeps its own pf hostid and limits, loads it, reports its hash in the heartbeat; a change of rules on the active node reaches it within 3 s; if the standby no longer hears its peer after a load it puts its own ruleset back and keeps the mirror off for ten minutes. Pre-flight and status show it.

### Detection time of 1 second
- **A frozen standby daemon no longer promotes itself with a short detection time.** With `detect_time` 1 a daemon frozen for 1.5 s (a paused VM, a starved host) took the silence it slept through for a dead peer, because the freeze guard still waited 3 s. The guard now follows the detection time. Measured with 1 s: crash served after 1.2 s, no false failover under 80 s of full CPU load on both nodes plus filter reloads plus a ping flood on the HA link; a frozen active daemon (1.6 s) makes the standby take over, as designed. The trade-off and the recommendation are in [fast-ha](docs/fast-ha.md#detection-time-of-1-second).

### Review before 1.0.0 (an independent read of everything since 0.9.1, all findings fixed)
- **A device that transmitted while silent is latched down** for ten minutes: the first version of the transmit watch brought it back up at the next pass of the enforcer, so a NIC that leaks every few seconds was up with the shared MAC and "ready" most of the time. The watch also runs when another readiness problem exists, and **fails closed** (an unreadable packet filter or counter is a problem, not a pass).
- The ruleset mirror: the role is checked again after the operations lock was obtained (the old active node could still be sending during a switchover and its ruleset replaced the new active node's); the "peer still heard" check no longer holds the lock (a promotion must not wait for it); a rejected or failed send backs off (3 s doubling to 5 min) and is not made towards a peer that does not run the warm or hot mode; after a failed load the standby no longer claims to hold a mirror; the file read by the active node must be a regular file owned by root that nobody else can write; a ruleset with `include` or `load anchor` is refused.
- A failed promotion waits for the background ruleset generation and the routes before it restores the standby view (it could be overwritten by a ruleset generated from the full configuration).
- The heartbeat limit is 64 KiB (was 4 KiB) and the readiness text in it is capped: with many VLANs a long list of problems could have made the peer look dead.
- Device names of LAGG members and parents are checked before they reach `ifconfig`; the link hold never exceeds the configured hold.

### TCP sessions across a failover (hardware)
- A long-lived TCP session through the firewalls (iBGP between two VRFs of the switch, keepalive 3 s, hold 9 s) survives a graceful failover and a crash of the active node; flushing the pf states on the new active node resets it, so the test does detect a missing state ([validation](docs/validation.md) section J5).

### Documentation
- Switch checklist (edge ports, no port security, trusted ARP, storm control, one static port-channel per firewall for LACP, points for Juniper EX4600 and Meraki MS, not tested with the plugin), the procedure to switch the hot standby on and back, the comparison with CARP, the measured tables in the README.

### Tests
- Native FreeBSD: `go test -race`, `staticcheck`, `go vet`, PHP lint of the changed scripts; unit tests for the mirror (checks, hostid, rollback, busy), the freeze guard, the transmit watch, the LACP exemption.

## 0.9.3
Measured on the two Supermicro machines with the MikroTik switch, with LACP bonds ([validation](docs/validation.md) section J3).

### Hot standby (new mode, experimental, off by default)
- **The standby keeps the LAGG, its ports and the VLANs up, already carrying the shared MAC, with no address and IPv6 off; the takeover only adds the addresses.** 21 interfaces in 176-222 ms with no link event. Client view: graceful failover **0.10-0.18 s**, crash of the active node 2.8-3.0 s (3 s of it is the detection), dead card 3.1 s. With an LACP LAGG on real 802.3ad bonds the classic takeover took **12.5 s** (graceful) and 9.6 s (crash).
- Works with failover, load-balance, round-robin and **LACP** LAGGs, NICs and VLANs. *Hot standby with an LACP LAGG*: *Off* (default, classic takeover for an interface directly on the LAGG), *Shared* (the standby's LACP bundle stays established with the shared MAC as system id; needs one static port-channel per firewall, a dynamically aggregating switch would merge both firewalls). VLANs on an LACP LAGG that is not an interface are hot in both cases.
- Safety: the daemon watches the software transmit counter of every silent interface of the standby and shuts down one that transmits (LACPDUs of an LACP LAGG excepted), checks that the standby's packet filter has its default deny, verifies every interface before and after the takeover, undoes and falls back to the classic takeover on any doubt.
- Measured on the X710: a standby with the shared MAC and no address sends nothing with it (the firmware LLDP agent uses the permanent MAC of the port).

### Fixes found on the way
- The pre-flight and the status name the mode (warm or hot) and the interfaces that keep the classic takeover with the reason.

## 0.9.2
Measured on the two Supermicro machines with the MikroTik switch as client (`docs/validation.md` section J2).

### A faster takeover
- **VLANs are brought up with one `ifconfig` each instead of the PHP interface code** (setting *Fast interface takeover*, on by default). For a VLAN with a static IPv4 address and IP aliases OPNsense's code does nothing else (it skips the hardware offload settings for VLAN devices), so 20 VLANs now take 0.06-0.14 s instead of 2.3 s. The physical NICs and the LAGG, DHCP/IPv6/MTU interfaces, anything with bridges, GRE/GIF or static ARP entries in the configuration, and any VLAN that does not come up as planned keep using the stock code. The plan comes from `ifaces.php plan` and is checked again in Go before it reaches `ifconfig`.
- **The ruleset is generated while the interfaces come up** instead of after them; the authoritative reload after routes and gateways is unchanged (and a failed promotion puts the ruleset of the standby view back). Routes and gateway monitoring run during the carrier wait.
- **A monitored link that lost its carrier counts after a third of the hold time, at least 2 s** (was the whole 6 s); services and probes keep the full hold. While a problem is pending the health loop runs every 0.5 s instead of 2 s (with a 2 s hold the 2 s tick was a coin flip between 2 and 4 s).
- Measured: promotion 3.1-3.8 s with 21 interfaces, graceful failover 1.0 s seen by a client, crash of the active node 4.9 s, dead card 5.1 s (0.9.1: 4.4-4.9 s, 5.9 s, 8.4 s, 11.9 s).

### Operations
- The data ports of the switch must be **edge/portfast**: with RSTP auto-detection a port whose link returns waits about 3 s, which was 3-4 s of extra outage after every takeover in the lab.
- The help of *Promiscuous mode on data interfaces* no longer says that real NICs never need it.

## 0.9.1
Found on real hardware (two Supermicro machines, Intel X710, a MikroTik switch as traffic generator, 20 VLANs on a LAGG, the production configuration).

### A VLAN with its own MAC received nothing (fix)
- **On the X710 a VLAN whose MAC differs from its parent's gets no unicast frame** (ARP works, so everything looked fine and every ping was lost); the vlan(4) driver does not put the child's MAC into the NIC's receive filter. The pairing gave every VLAN a MAC of its own. Now a VLAN on an assigned parent **shares the parent's MAC**; a VLAN on a parent that is not assigned gets its own MAC and OPNsense's `promisc` option; and after every takeover the daemon sets promiscuous mode on any VLAN whose MAC differs from its parent's (this also repairs pairs made with 0.9.0).

### Takeover twice as fast
- The promotion configured each interface with OPNsense's *reload* option, which restarts routing, IPsec, DHCP and Unbound **once per interface** (0.9 s each). It now configures the interface only; routes and DNS already ran once afterwards. With 21 interfaces: promotion **4.4-4.9 s instead of 10.1 s**, a crash of the active node is served again after 8.4 s, a dead card after 11.9 s, a graceful failover after 5.9 s (client view, measured at 20 ms). The phases of a promotion now show each tier ("tier 2: 20 interface(s) configured").

### Fixes
- A promotion that ran inside the automatic loop (it takes seconds) made the next tick report "this daemon did not run for 10s (frozen, paused or starved)" and restart the detection. The tick is now stamped when the step ends.

### Validation
- `docs/validation.md` section J: pairing, graceful failover, crash, dead card and cold start of both nodes, measured with the switch as client.

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
