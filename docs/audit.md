# Audit of the failover logic (0.7.0)

Scope: the daemon (`daemon/`) with emphasis on role transitions, split-brain, promotion, pfsync and package replication. Every statement below was checked against the code, not against the documentation. **Proven** = reproduced by a Go test with the system actions replaced by fakes; **not tested on OPNsense** = implemented and compiled for FreeBSD, but never run on a real firewall.

## Findings

| Severity | Component | Problem | Cause | Correction | Validation |
|---|---|---|---|---|---|
| P0 | `heartbeat.go` `failover` | **Split-brain on rollback.** A graceful failover demoted the node, asked the peer to promote and, on any error (timeout, lost reply), promoted itself again. If the peer had promoted and only the answer was lost, both nodes owned the same IP/MAC (the 90 s wait made this likely). | The rollback was decided on "no answer", not on knowledge of the peer's state. | Transactional hand-over (`transition.go`): unique transition id and sender epoch on `/v1/promote`, idempotent execution on the peer, a `/v1/abort` handshake that answers atomically with the peer's role. Rollback only when the peer confirms it did not and now cannot take over; any doubt keeps this node standby. The peer also holds back its own election for 45 s after a cancellation. | Proven: lost reply, lost request, delayed/duplicated request, peer promote failure, peer still promoting, unreachable peer, ambiguous answers (`failover_test.go`). |
| P0 | `autofail.go` `autoStep` | **Split-brain resolution delayed or skipped.** It sat behind the 10 s anti-flapping hold (`minHold`) and behind the "automatic failover" switch, and a stuck `busy` flag of the peer excused a double active forever. | One early return guarded everything. | Resolution moved in front of all guards (`splitBrainStep`): not subject to `minHold`, maintenance mode or the auto-failover switch; confirmation = two heartbeats (2 × interval, at least 300 ms); `busy` excuses the peer for at most 1 minute; a failing stand-down is retried every 2 s. | Proven (`failover_test.go`). |
| P0 | `heartbeat.go` `handlePromote` | **Stale and duplicated commands were obeyed** (no epoch check, no idempotence): a delayed request could promote a node that had been told otherwise. | No transaction identity. | Requests from an older epoch are refused; a transition id is executed at most once; an id cancelled by its sender can never start. | Proven. |
| P1 | `promote.go` | **False ACTIVE.** The result of `configctl filter reload` was only logged; a node could report ACTIVE with its interfaces up and pf not loaded (or disabled, which means no filtering). | Errors ignored. | The reload is retried once and verified with `pfctl -si`; on failure the promotion fails, the node returns to its standby view, stops what it started and reports the error. Failed static routes mark the node *degraded* (status, event log). | Helpers proven; the end-to-end path is **not tested on OPNsense**. |
| P1 | `pfsync.go` | pfsync was configured once at start; a failure (for example the HA address not there yet) was only logged, never retried, never shown. Session continuity was silently not guaranteed. | No supervision. | A loop verifies `pfsync0` (up, sync device, peer) every 10 s, repairs it and reports the reason in the status and as an event. What is verified is the configuration and the link flag, **not** that the state tables are identical. | Parser proven; repair loop **not tested on OPNsense**. |
| P1 | `packages.go` | After an install, plugins still at another version only produced a log line and the sync was reported as fully successful. Plugins installed only on the standby were not reported. | No conformity report. | `packageDiff` and status field `package_diff`: plugins missing, at another version, or extra (never uninstalled automatically). It does not block the configuration sync (a failed install still does). | Proven. |
| P1 | `ctl.go` | The status hid conditions an administrator needs. | — | The status now has `degraded`, `pfsync_problem`, `package_diff`, `failover_in_progress`; the history records the phases of each promotion and, for automatic ones, how long the peer had been silent (`detect_ms`). | Phases proven; the GUI shows the new fields. |
| P2 | `sync.go` `handleConfig` | A configuration push carries no epoch; it is accepted from the pinned peer whenever this node is standby. | Ordering assumed from "only the active node pushes". | **Not changed.** The pinned mutual-TLS identity limits it to the peer, and an active node refuses pushes. | — |
| P2 | peer API | No per-message nonce. The promote path now has transition ids and epochs; other endpoints rely on the pinned TLS 1.3 channel. | — | **Not changed.** | — |

Checked and found sound: the isolation guard (a silent peer is promoted only when this node can show its own management/HA carrier is up, plus optional fencing), the boot sequence (a paired node always boots silent), state persistence (fsynced, backup copy, fail-safe on corruption), peer API access control (certificate pinned per endpoint, allow-listed file names for lease replication).

## What cannot be solved in software (two nodes, no arbiter)

With two nodes and no independent witness it is impossible to have both automatic takeover **and** a guarantee against two active nodes under every partition:

* **Symmetric partition of the two nodes only** (for example the HA cable and the management path both cut while each node still sees its own carrier up): each node sees a silent peer and may promote. The isolation guard cannot tell this from a dead peer. Without fencing the result is a double active until a path returns; then the epoch rule resolves it. With a fencing command (IPMI, smart PDU) the promotion is cancelled unless the peer was really switched off.
* **Peer alive but its data plane dead** (a failed switch port, for example) cannot be told from a healthy node by heartbeats alone. The health checks (monitored interfaces, probes) exist for this and decide on the active node only.
* A "witness that answers ping" is not fencing and is not used as proof.

The behaviour chosen: on any ambiguity during a *graceful* hand-over the old node stays standby (no service is better than a double active). For an *automatic* takeover after silence the node promotes only when it can show it is not the isolated one, and the operator is told why otherwise. Recommended for production: a management path **and** a fencing command (see `docs/security.md`; the design of a faster takeover is in `docs/fast-ha.md`).

## Tests

`daemon/failover_test.go` and `daemon/supervision_test.go` run the real failover, election and split-brain code with fakes for the system and the network (a link that can lose requests or replies and deliver late or twice). Covered: A active/B standby, a real failure of either node, loss of the heartbeat paths, intermittent loss below the detection time, lost/delayed/duplicated promote, concurrent promote, concurrent failover, split-brain for every epoch/role combination, a returning old active, election after both nodes booted, a stuck busy flag, retry rate limits, and the pf, package and pfsync parsers.

They validate the **decision logic**. They are not a validation on OPNsense, a switch or real NICs (see `docs/validation.md`, section G, for the lab procedure). On the machine used for this change Windows policy blocks freshly built executables, so the suite was run compiled to WebAssembly under Node (`GOOS=js GOARCH=wasm go test -exec ...`), which has no race detector. CI runs `go test -race`, which is where the concurrency tests count.

## Second round (0.9.0): the two real daemons against each other, and the NICs measured

The first round was done from the code and with fakes. This one ran the real daemons, paired over mutual TLS, on a FreeBSD 15.1 machine (`lab/sandbox`: vnet jails as nodes, `epair` interfaces, bridges as the switches, a client, stub scripts instead of OPNsense's PHP) and measured the NICs of the target hardware. The sandbox is a real kernel and a real network stack; it is not a real NIC driver or a real switch (see below).

| Severity | Component | Problem (shown by a real run) | Cause | Correction | Validation |
|---|---|---|---|---|---|
| P0 | `health.go` | **A dead network card never caused a failover with VLAN-on-LAGG, nor on NICs that drop their link when down.** The active node only switched when the standby read link on its own port; a standby keeps its ports down, so a VLAN on a LAGG (it follows its parent) and an administratively down `igb` read "no carrier" by construction. Measured on the target machine (VLAN down with its LAGG: "no carrier"). | The standby's "no carrier" meant both "no cable" and "not looked at". | Tri-state link evidence: the standby reports the links it could not read (`links_unknown`); only a link it read and found down blocks the switch. Pre-flight warns when nothing is monitored. | Unit tests (`health_test.go`), sandbox scenario 15 |
| P0 | `autofail.go` | **A frozen daemon that wakes up promoted at once (1.2 s of double active, and it won the arbitration).** A process stopped for 95 s (SIGSTOP; the same for a paused or migrated VM) woke up with a heartbeat clock that said "the peer has been silent for 95 s". Found by sandbox scenario 14 in warm mode. | The silence was measured across a gap in which nobody was listening. | The daemon notices that its own loop did not run for 3 s and restarts the detection from the wake-up. | Unit tests, sandbox scenario 14 |
| P1 | `transition.go` | A peer that dies during a hand-over left the survivor without service for 54 s (it waited the whole 45 s window for an unreachable peer); a promote request sitting in a frozen peer's socket could be executed long after its sender moved on. | Unbounded validity of the request, no early exit. | The request is valid for 10 s (the peer refuses it 5 s later); the wait for the answer is 45 s (was 90 s) and ends early when nobody answers and no heartbeat arrives on any path. Recovery after a peer dying during a hand-over: 18 s. | Unit tests, sandbox scenarios 8 and 14 |
| P1 | `pfsync.go` | The supervision re-applied pfsync every 10 s and filled the event log. | FreeBSD 15 prints `syncdev`, `syncpeer`, `maxupd` on one line; the parser read one setting per line. | Parser reads every field. | Unit test with the real output, sandbox |
| P1 | `warm.go`, `ifaces.php` | A VLAN on an unassigned LAGG would have been classed warm and the LAGG treated as a plain NIC (`ifconfig lagg0 link 00:00:...`). | Eligibility looked at assigned parents only; `ifconfig` shows an all-zero `hwaddr` for a LAGG. | A VLAN is warm only on a plain NIC; all-zero and multicast MACs refused; addresses, prefixes, MTU validated in Go as well. | Unit tests |
| P2 | `ops.go`, `main.go` | The sandbox needs to move the helper scripts and the command search path through the environment. | — | Honoured only for directories owned by root and not writable by others; otherwise ignored with an error in the log. | Unit tests (native FreeBSD) |
| — | tests | Defects of the tests themselves, not of the product: `TestFenceCommandValidation` failed when run as root, the first MAC-change measurement split the MAC on its colons, an `epair` never loses its carrier (carrier loss is simulated by a shim in the sandbox), a powered-off node keeps its addresses on a down interface (the double-active detector counted it). | — | Fixed. | — |

### Hardware finding: the warm standby has no advantage on these NICs

Measured on the Supermicro (X710 and I210): setting a MAC with the link up drops the link for about 0.74 s (X710) to 0.9-1.9 s (I210), as much as an administrative down and up, and the X710 keeps its link when the port is down. The warm standby is kept (off by default); the classic mode is the recommendation. Numbers and consequences in [warm standby](fast-ha.md).

### What the sandbox proves, and what it cannot

Proven on a real kernel with the real daemons (results in [validation](validation.md) section I): pairing over mutual TLS, heartbeats on two paths, configuration replication, graceful failover in about 0.1 s with two lost pings, automatic takeover after a power cut, split-brain resolution, a promotion refused when pf does not work, the confirmation protocol after a lost or late request, a frozen peer, partitions, packet loss, a dead monitored interface, and in both the classic and the warm mode, with a detector that samples both nodes every 100 ms for two consecutive samples with the shared identity on both.

Not proven by it: anything about a real NIC driver or switch (an `epair` has no link negotiation, no CAM table, no port security), OPNsense's own PHP (stubbed), timing on hardware, LACP with a real partner.

## Third round (0.9.1): real hardware

Two Supermicro machines with Intel X710 NICs, a real switch as traffic generator and the production configuration (20 VLANs on a LAGG). Results in [validation](validation.md) section J.

| Severity | Component | Problem (shown by the hardware) | Cause | Correction | Validation |
|---|---|---|---|---|---|
| P0 | `spoof_macs.php`, `promote.go` | **A VLAN interface received no unicast packet.** ARP and broadcasts worked, every ping to the firewall or through it was lost. | The pairing gave every VLAN a MAC of its own; the vlan(4) driver does not put it into the receive filter of the X710, which holds the parent's MAC only (the sandbox's virtual NICs accept any MAC and could not show it). | A VLAN on an assigned parent shares the parent's MAC; one on an unassigned parent gets its own MAC plus OPNsense's `promisc`; the daemon sets promiscuous mode after a takeover on any VLAN still carrying another MAC than its parent's (`vlanmac.go`). | VLAN 65 and 40 answer with promiscuous mode off after re-pairing; unit tests for the detection |
| P1 | `reconfigure_ifaces.php` | A promotion took 10.1 s with 21 interfaces (client outage up to 10 s). | The per-interface call used OPNsense's *reload* option: routing, IPsec, DHCP and Unbound restarted once per interface (0.9 s each instead of 0.17 s). | Interface only; routes and DNS already run once afterwards. | promotion 4.4-4.9 s; crash 8.4 s, dead card 11.9 s, graceful 5.9 s on the client |
| P2 | `autofail.go` | After a promotion inside the automatic loop the daemon logged "did not run for 10s (frozen...)" and restarted its detection. | The tick was stamped when the step began, a promotion lasts seconds. | Stamped when the step ends. | cold start of both nodes: no warning |

## Fourth round (0.9.2): the takeover itself

| Severity | Component | Problem (shown by the hardware) | Cause | Correction | Validation |
|---|---|---|---|---|---|
| P1 | `fastpath.go`, `ifaces.php`, `promote.go` | 20 VLANs cost 2.3 s of PHP at every takeover whatever the parallelism. | `interface_configure()` parses all interfaces for each interface; for a static IPv4 VLAN it does nothing else than `ifconfig`. | One `ifconfig` per VLAN (address, options and `up`, then one per alias), only for VLANs the plan marks fast, verified afterwards, stock code as fallback. Two ifconfig syntax traps met on the hardware: after `inet <address>` the next word is read as a peer address. | 20 VLANs in 0.06-0.14 s; unit tests with a model of ifconfig; the plan checked again in Go (device name, CIDR, MAC, description) before it is passed to `ifconfig` as separate arguments (no shell) |
| P1 | `promote.go` | New connections waited for the ruleset, generated after the interfaces. | Sequential. | Generated while the interfaces come up; the authoritative reload after routes and gateways stays; a failed promotion reloads the ruleset of the standby view. | Same outage for new and established flows |
| P1 | `health.go` | A dead card took 8.9-11.9 s to be switched away from. | A 6 s hold for every health problem, evaluated on a 2 s tick (a 2 s hold met at the second tick only by chance). | Carrier loss of a monitored link counts after hold/3 (at least 2 s), tick every 0.5 s while a problem is pending. | 5.1 s |
| info | lab | 3-4 s of extra outage after every takeover. | RSTP auto-edge on the switch ports. | Edge ports (documented requirement). | 1.0 s graceful |

Security review of the new code: the fast path passes only values from the plan to `ifconfig` as separate arguments (no shell), after checking them again in Go; a VLAN that is not in the expected state before (existing address) or after is left to the stock code, which flushes and reconfigures it, so the fast path cannot make a takeover different, only faster; the early ruleset is the ruleset the node is going to keep (authoritative reload follows), and established connections never depended on it (pfsync states).

## Fifth round (0.9.3): the hot standby on real LACP

| Severity | Component | Problem (shown by the hardware) | Cause | Correction | Validation |
|---|---|---|---|---|---|
| P1 | `hot.go` | The LACP LAGG of the standby was shut down every 3 s by the transmit watch, so no takeover could be hot. | A LAGG in LACP mode transmits LACPDUs (counted in its software counter). | LACP LAGGs are exempt from the watch (their VLANs are not). | LACP takeover in 176-222 ms |
| P1 | `hot.go` | After a node had been active, every VLAN was shut down when it stood down. | The watch kept the baseline from before it was active; the traffic it carried looked like a standby that speaks. | The baseline is taken when a device is made silent (and forgotten when it takes its addresses). | dead card and crash tests: the old active returns as a ready standby |
| P1 | `fastpath.go` | The native VLAN takeover failed on the hardware (`inet6: bad value`, `inet: bad value`). | After `inet <address>` ifconfig reads the next word as a peer address, also after `alias`. | One invocation per address, in the order of OPNsense's code. | VLANs in 0.06-0.14 s |
| P1 | design | Hot standby on an LACP LAGG could merge both firewalls into one LAG on a switch that aggregates dynamically. | The standby's LACP system id is the shared MAC. | Opt-in setting (*Hot standby with an LACP LAGG*), default off, documented; the measured bonds (RouterOS) stay separate. | two bonds, two active ports each, partner system id = shared MAC |
| info | design | A standby that leaks a frame with the shared MAC would move the MAC on the switch. | A NIC may transmit with the OS MAC. | Transmit watch on every silent interface, shut down on the first packet; the packet filter of the standby must have its default deny. | measured silent for 45 s on the X710; unit tests for both checks |

Known limit (documented): the ruleset on a standby is the one of its last active period; the authoritative reload of the takeover refreshes it 1-2 s in.

## Sixth round (1.0.0): independent review of everything since 0.9.1

An independent reviewer read all changes (hot standby, ruleset mirror, native VLAN takeover, health and heartbeat changes, PHP helpers) for injection, the peer endpoint, races and plain bugs. No remote-exploitable issue and no blocker was found; eight items to fix and a list of nits, all fixed (tests added for each behaviour):

| Component | Finding | Correction |
|---|---|---|
| `hot.go` | The transmit watch was not latched: the device it shut down came back at the next enforcer pass. | Latched down for ten minutes (`hotLeak`); the watch runs whatever else is wrong. |
| `ruleset.go` | The role was checked before the lock wait, not after (old active node vs new active node). | Checked again with the lock held. |
| `promote.go` | A failed promotion restored the standby view while the early ruleset generation and the routes still ran. | Both are waited for before the cleanup. |
| `ruleset.go` | Refusals were repeated every 3 s with up to 8 MiB; after a failed load the standby claimed a mirror it no longer had; a peer without hot mode was sent rulesets. | Exponential backoff to 5 min, the claim cleared, the send skipped for a peer that is not warm/hot. |
| `heartbeat.go`, `warm.go` | A 4 KiB heartbeat limit with an unbounded readiness text: many VLANs with problems could make the peer look dead. | 64 KiB and the text capped at 300 characters. |
| `ruleset.go` | `/tmp/rules.debug` read without ownership check. | Regular file, owner root, not group/other writable; `include` and `load anchor` refused in a mirrored ruleset. |
| `ruleset.go` | The "peer still heard" wait held the operations lock. | Done outside the lock; the rollback only while still a standby. |
| `hot.go` | The safety checks failed open on an unreadable `pfctl` or `netstat`. | Fail closed. |
| nits | device names of parents and LAGG members not validated; link hold could exceed the configured hold | validated; `min(hold, ...)` |
Not changed, documented: the MTU set at a takeover is not restored at stand-down; `promisc` stays on a former parent; the enforcer takes the operations lock every second (a promotion may wait for one pass).

## Seventh round (1.0.2): gap analysis against the audit brief

The brief in `todo/version-1.x.md` was compared with the code, the tests, the CI and the documentation one section at a time (by a reader who did not write the code, then each finding checked again before it was acted on). What was found and what was done:

| Section | Finding | Result |
|---|---|---|
| 3 persistence | The role was changed in memory before it was saved; no test could make the disk fail | transactional update with a defined commit point, `UpdateSafe` for what already happened, deep copy, validation at save and at load; fault-injection tests for a full disk, EIO, fsync, rename, directory sync, crashes between the steps, torn and invalid files, concurrent updates ([security](security.md#review-of-101-to-102-persistence-promote-request-fencing-policy)) |
| 4 promotion | A failed save after the takeover left the node active with an error returned to the peer | the takeover is committed with `UpdateSafe`; a failed save marks the node degraded and the answer stays "promoted" |
| 6 `/v1/promote` | `tid`, `epoch`, `exp` optional, parse errors ignored | required and strict; table test of 18 malformed requests, bounds on epoch and validity, replay after the log was trimmed |
| 7 `/v1/config` | headers not checked; "a deposed active node can overwrite" | headers checked; the second claim was wrong (a push is accepted only by a standby), documented; no epoch comparison on purpose (it could stop the sync after a re-pairing) | (superseded in 1.0.3: a leadership generation is now carried and checked, see the eighth round)
| 5 split-brain | the invariant was asserted in a few places only; no policy without fencing | seeded property test of the role protocol (failovers with lost requests/answers, restarts, a manual promotion of the standby, nobody active) with the invariants "at most one active" and "epoch never decreases", mutation-checked; losses of 10/30/50 %, duplicates and late delivery; new option `fence_required`; fencing policy table test |
| 10 services | a service that failed to start was only logged | checked after the start; failure marks the node degraded |
| 14 tests | no test of the configuration push | header and refusal tests |
| 17 CI | ShellCheck did not fail the build and covered four scripts; no vulnerability check | ShellCheck on every script (15 findings fixed or justified), `govulncheck`, `go mod verify`, reproducible-build check, package smoke test, attestation and checksum of the daemon binary |
| 19 documentation | old repository name in the install and verification commands; stale file names; build section out of date | corrected; certificate rotation and pfsync exposure documented; the limit without fencing stated at the top of the README |

Not done, and why:
- **Latency simulation (500 ms, 2 s) in the unit tests**: the tests use losses, duplicates and late delivery with a seeded generator, not delays (they would need a simulated clock in the transition code). Delays and a frozen peer were measured on the lab and in the sandbox.
- **`bootStandby` and the enforcer** are not covered by a Go test (they act on real interfaces); the sandbox and the hardware runs cover them.
- Not run on this machine: `staticcheck` and `govulncheck` (programs cannot be started here); both run in the CI.

## Eighth round (1.0.3): an external audit of 1.0.2

A second, independent audit of the tag v1.0.2 confirmed that three of the items left open above mattered, and found problems in how they had been left. What was done:

| Finding | Result |
|---|---|
| No protocol version: `checkVersions` warned but nothing was refused between versions | `proto` in the heartbeat (absent = 1) and `X-Proto` on the configuration push. A node whose peer speaks a newer protocol refuses what needs both sides to agree (configuration and ruleset push, graceful failover) with a message that says to update it first; the pre-flight fails with the same text. The safety paths (automatic takeover after a silence, split-brain resolution) never look at it. A newer sender is refused by the receiver as well |
| `/v1/config` carried no leadership generation. The 1.0.2 answer (nothing, because comparing with the node's own epoch can wedge the sync) was incomplete | the sender gives its epoch (`X-Epoch`); the standby keeps the highest epoch it accepted from this peer (`leader_epoch`) and refuses a lower one (409). It is not the node's own epoch, so it follows the leader; it starts again at 0 when a pair is formed. A sender without `X-Proto` (plugin 1.0.1 or older, a rolling upgrade) is accepted without an epoch, a sender with one must give it |
| The property test checked the `state` field, not the network identity | the model now tracks what the fake interface actions did: the shared IP and MAC must be on at most one node, and a node that has them must believe it is active. Mutation-checked (a demotion that leaves the identity makes it fail at step 1). 40 seeds of 300 steps. The model uses a disk that never fails, because the disk is covered by `state_fault_test.go` |
| `enforceOnce` returned silently when the interface list could not be read | falls back to the configuration in use (as the boot guard does) and reports it once per ten minutes; if both fail it says that the devices are NOT being kept down |
| Errors of the rollback of a failed configuration push were ignored | reported, and part of the answer to the active node |
| CI: the manifest of checksums mixed paths and could not be verified | the daemon binary is copied to `dist/hasyncd-freebsd-amd64`, `SHA256SUMS` lists both files and the job runs `sha256sum -c`; the release carries the binary |
| CI: the workflow of 1.0.2 was invalid (a colon in two step names), `govulncheck` v1.1.4 crashes on Go 1.27, `staticcheck` 2026.2.1 cannot read the export data of Go 1.27.2, a test raced with a background goroutine | fixed: quoted names, `govulncheck` v1.8.0, `staticcheck` 2026.2.2, guarded timing variables, the test waits for the failover to end. The v1.0.2 tag never produced a release (the workflow did not start); 1.0.3 is the first release with these checks |
| `govulncheck` found nine standard-library vulnerabilities in the Go 1.27.0 that built every earlier release | Go 1.27.2 ([security](security.md)) |

Still open, and said so:
- (done in 1.0.4, see the ninth round) **The commit of a takeover after the traffic is back** stayed as it was: if the state cannot be saved the node is active, marked degraded, and boots as a silent standby after a restart. The alternative (writing an intent before touching the network and rolling it back on failure) changes what every reader of the state sees during a promotion; it needs its own design and a hardware test of a crash inside the window. Not done.
- **A crash of the active node on real hardware with this version** has not been measured; the unit tests and the earlier hardware runs (docs/validation.md) are not a substitute.
- **Latency in the unit tests** (500 ms, 2 s) needs an injectable clock in the transition code. Not done.
- **`bootStandby` and the enforcer** have no Go test of their crash behaviour (only the new fallback is reasoned about); the sandbox and the lab cover them.
- **Concurrency**: configuration push against a promotion, and goroutine leaks, have no dedicated test.

## Ninth round (1.0.4): the points left open

The brief was read again against the code, starting from the list "still open" of the eighth round and from the scenarios A to J of the brief.

| Point | Before | Now |
|---|---|---|
| Commit of a takeover after the traffic is back (B8 of the external audit) | a failed save left the node active and degraded; a restart of the daemon (not a reboot) read the old role | a **write-ahead record** of every promotion and demotion (`intent.go`): written before the first change of the machine, removed after a durable commit. At its start the daemon finds a record that is not a commit and brings the node to standby. A takeover whose commit was not saved keeps the record, so it is never taken for a definitive one ([architecture](architecture.md#commit-points-and-recovery-of-a-role-change)) |
| Scenario A: demotion interrupted before its state is saved | after a restart of the daemon the node believed it was active with its interfaces down | standby, by the record; and a demotion that fails after the devices went down sets the role to standby at once |
| Reordered messages (§5, §12) | the last heartbeat received, from either path, replaced the state of the peer and refreshed its liveness, even when it was older than one already received | each daemon sends an incarnation and a sequence number; an older message is counted and ignored |
| Latency (A3) | not tested | tested on the scale of the protocol: an answer after the requester gave up, a request delivered after its validity; the network-identity invariant holds once every message in flight has arrived. Real-time sleeps, not a simulated clock: the transition code has no injectable clock |
| Goroutine leaks (A6) | not tested | 40 failovers with 30 % loss end with the goroutines they started with |
| Commands without timeout (§8.3) | `sh`, the PHP helpers and the shell scripts could hang for ever while holding the operations lock | bounded to 2 minutes; package installation, firmware update and fencing keep their own limits |
| Authorization of every endpoint (§6, §25) | each route was wrapped by hand | one table of routes; a test sends another certificate, no certificate and a GET to every one of them |
| TLS identity (§8.1) | written without fsync; a damaged one was silently replaced | durable; a replacement is logged as an error |

Scenarios A to J of the brief, as the code stands now:

| | Scenario | Protection | Test |
|---|---|---|---|
| A | demotion interrupted before its state is saved | transition record; reboot always standby | `intent_test.go` |
| B | the takeover happened but its answer is lost | transition id, abort handshake: the old node stays standby when in doubt | `failover_test.go`, property tests |
| C | configuration sync during a promotion | both take the operations lock; the sync checks again that the node is standby | (reasoned; no Go test, the apply runs the PHP helpers) |
| D | disk full on the standby during a sync | the incoming file is written first, the previous files are snapshotted, a failure rolls back and is reported (also if the rollback fails) | `state_fault_test.go` for the role file; the sync path itself on the lab only |
| E | both start at the same time without communication | boot wait, then the isolation guard; **without fencing both may promote** (documented); `fence_required` refuses it | `fencing_policy_test.go` |
| F | standby with an address of production after a crash | enforcer every second, boot guard; the record redoes the standby view | `intent_test.go` |
| G | the nodes cannot reach each other but both see the production network | the same as E: no proof is possible without an external arbiter; fencing | documented in this file |
| H | different epochs and damaged state files at start | validation at load, backup copy, silent unpaired standby if both are damaged; the record | `state_fault_test.go` |
| I | the switch keeps an old MAC entry | gratuitous ARP after the links are up; hot standby keeps the shared MAC on the port group | measured on the lab |
| J | the plugin is updated on one node only and the protocol changed | protocol version: operations that need both sides are refused by the older node | `sync_test.go` |

Still open, and said so:
- **No injectable clock** in the transition and heartbeat code: the latency tests sleep for real (milliseconds, scaled down), and cannot reproduce a frozen clock or a clock jump.
- **The configuration sync (apply, rollback) and the boot path** have no Go test of their own; they run the PHP helpers and act on the interfaces. The lab and the sandbox cover them.
- **A crash of the active node and an interrupted promotion on real hardware with this version**: see [validation](validation.md) for what was run.
