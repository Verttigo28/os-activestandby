# Architecture

## Model

Two nodes, one configuration. Each node is **primary** or **secondary** (decided at pairing, persistent) and, independently, **active** or **standby** (changes at failover). The primary is active first and wins ties.

| | Active | Standby |
|---|---|---|
| Interfaces | up, shared IP and MAC | data interfaces down and without addresses; only HA link (and management) up |
| Services | IPsec, gateway monitoring, DHCP/DNS as configured | stopped |
| `/conf/config.xml` | the full, authoritative configuration | "standby view": data interfaces without `<enable>`, immutable flag set |
| Full configuration | `config.xml` | `/conf/activestandby/active.xml` |
| Role | configuration source | receives numbered revisions |

State lives in `/conf/activestandby/state.json` (fsync'd, with a `.bak` copy), never in `config.xml`, so replication cannot overwrite what a node is. A node with unreadable state fails safe: silent standby with no peer pinned.

## Components

```
GUI (Phalcon MVC, Volt) -> API controllers -> configd actions -> hasyncd ctl -> control socket (root only)
                                                                                    |
                       hasyncd daemon (root) --- mTLS peer API on the HA (and management) address --- peer
                           |-- PHP helpers (root, framework-free where possible)
                           |-- ifconfig / pfctl / configctl for the actual work
```

The daemon is Go, standard library only, one static binary. The GUI never talks to it directly: controllers validate input and call fixed configd actions with positional parameters, so no user input reaches a shell string. The file map is in [`daemon/doc.go`](../daemon/doc.go).

## Failure detection

Heartbeats go out several times per detection period (default 3 s, configurable 1-30) on the HA link and, if configured, the management network. A peer counts as dead only after silence on **every** path. Before a standby promotes itself it must prove it is not the isolated node: its management carrier is up (and, if management probe targets are configured, it reaches them), or, with the HA link only, its own HA carrier is up. Otherwise promotion is held back and an operator decides.

If the peer went silent and a fencing command is configured, that command must succeed first. It runs as a root-owned absolute path without a shell, and a failure blocks the promotion.

A daemon that was itself frozen (a stopped process, a paused or migrated virtual machine, a starved machine) does not count the silence it slept through: if its 500 ms loop did not run for 3 s, the detection of a silent peer starts again from the wake-up, so a node that wakes up listens before it acts.

On the active node, a lost link on a *monitored* interface switches the roles after the hold time, provided the standby can take over. The standby keeps its ports down, so it reports which links it could not read; a link it read and found down is the only evidence that stops the switch (the outage would only move). See [operations](operations.md#which-failures-cause-a-failover).

Cold start with nobody active: the primary promotes after about 5 s, the secondary only after about 30 s. A rebooted node always boots as standby (an early rc hook rewrites the config before interfaces are configured, and a boot guard keeps VLAN/LAGG members down), then follows these rules.

## Failover pipeline

One code path serves manual failover, health switches, preemption and the upgrade assistant:

1. The old active node keeps a copy of its config (as `unreplicated-*.xml` if the peer never received some edits; a graceful failover refuses in that case unless forced).
2. It takes its data interfaces **down** (traffic stops) and in the same moment tells the peer to promote. The rest of its clean-up (stop IPsec/dpinger, write the standby view) overlaps with the takeover.
3. The peer restores the full config, makes sure the virtual devices (VLAN, LAGG, ...) exist (they normally already do), configures the interfaces tier by tier in parallel (NICs, then LAGG, VLAN, bridges/tunnels), sets routes, sends a gratuitous ARP, then reloads pf and starts services side by side. DHCP/DNS restart afterwards in the background.
4. The new epoch is persisted, and the new active node pushes its complete configuration so both sides converge.

Every hand-over carries a unique transition id and the sender's epoch. The peer executes a given id at most once (a duplicate is acknowledged, not re-executed), refuses a request from an older epoch, and refuses an id the sender cancelled. If the answer to the promote request is lost, the old node does **not** guess: it asks the peer (`/v1/abort`) what became of the id. The peer answers atomically with its role:

| Peer's answer | What the old node does |
|---|---|
| peer is active | stays standby: the hand-over worked, only the answer was lost |
| id cancelled (never started, now can never start) or failed, peer is standby | rolls back and becomes active again |
| id still running | keeps asking (up to 45 s) |
| no answer, or anything else | **stays standby** and reports an error; nobody serves until the election (peer alive, nobody active) or the "peer lost" rule (peer silent, fencing) or an operator decides |

While it cancels an id, the peer also holds back its own election for 45 s so the two cannot race. A promote request is also only valid for 10 s after it was sent (the peer refuses it 5 s later, to allow for clock differences): a request that sat in a frozen peer's socket cannot be executed minutes later, after this node has moved on. When nobody answers and no heartbeat arrives on any path, the wait ends about 16 s after the request instead of lasting the whole 45 s, and the automatic rules (isolation guard, fencing) decide, so the survivor of a peer that died during a hand-over recovers in about 20 s. A node never elects itself while the peer reports (heartbeat `busy` flag) that it is promoting, and automatic promotion is paused on a node that is orchestrating a failover itself.

A promotion is only reported as done when the packet filter is verified: if the ruleset could not be loaded or `pf` is not enabled, the promotion fails, the node returns to its standby view and the interfaces go down again (a failed promotion never leaves routed traffic without a firewall). Smaller failures (static routes) leave the node active but flagged *degraded* in the status and the event log.

## Split-brain

Every promotion increments an epoch. If two nodes are ever active together, the higher epoch wins (tie: primary) and the loser demotes itself as soon as two consecutive heartbeats confirm the conflict (twice the heartbeat interval: 0.75 s with the default 3 s detection time). This resolution is **not** subject to the anti-flapping hold, to maintenance mode or to the "automatic failover" switch: standing down never makes the cluster less safe. A peer that reports a transition in progress is excused for at most one minute, and a failing stand-down is retried every 2 s. Pairing, the boot sequence, the isolation guard and the optional fencing hook exist so that this stays a rare, self-healing event.

## Replication

The active node pushes its whole `config.xml` as a numbered, compressed revision after every save (config syshook) and on a timer. The standby merges it with its node-local parts (hostname, HA and management interface, enabled flag and port), applies it, reloads pf only and acknowledges; on any error it rolls back and reports. Revisions are idempotent and checksummed. Drift detection compares what the daemon wrote with what is on disk.

Also replicated: the set of installed `os-*` plugins (the standby installs missing ones through a restricted proxy on the active node), DHCP lease files, firewall states (pfsync) and GUI sessions.

**GUI sessions.** The web GUI keeps logins in PHP session files. The active node pushes them to the standby every two seconds (changes only), so the browser cookie stays valid on the new active node: same shared IP and, because certificates are part of the configuration, same certificate. Deletions are mirrored, so a logout on the active node leaves no valid copy behind, and sessions created locally on the standby are never touched. TCP connections cannot move between machines: a request in flight fails and the browser reconnects, while firewall sessions survive through pfsync. Disable with the setting *Keep GUI logins across a failover*.

## Design decisions

- **Whole-file, one-way sync.** Predictable and easy to reason about. Edits made on both nodes are deliberately not merged; the standby's config is immutable so it cannot happen silently.
- **State outside the replicated config.** A role flag inside `config.xml` could be overwritten by the peer; a file the sync never touches cannot.
- **Standby silent by construction.** The standby view has no `<enable>` on data interfaces, so OPNsense itself cannot bring them up, and an enforcer keeps them flushed.
- **Virtual MAC.** Shared interfaces get a generated locally administered MAC (`02:xx:...`), independent of either node's hardware, so replacing a machine never changes what the network sees.
- **No dependencies.** Standard library only, no cgo.

## Lab findings that shaped the design

- A standby that boots with the active node's configuration *does* transmit with the shared IP and MAC (gratuitous ARP, IPv6 neighbour discovery) within seconds. On a real switch this moves the MAC to the standby's port. Hence the standby view and the early boot hook.
- `<spoofmac>` gives both nodes identical MACs from replicated config alone.
- OPNsense logs every config save and runs `rc.syshook config` on it; that is the replication trigger.
- VLANs: a VLAN interface shares the shared MAC of its (assigned) parent. A VLAN with a MAC of its own is not received at all by a real NIC unless promiscuous (measured on the X710), so the pairing never gives one to a VLAN on an assigned parent, and after a takeover the daemon sets promiscuous mode on any VLAN whose MAC differs from its parent's.
- Hyper-V quirks (lab only): the `hn` driver needs promiscuous mode for MACs that are not the hardware MAC (setting *Promiscuous mode on data interfaces*); a virtual switch does not relearn a vendor MAC owned by another port, so use locally administered MACs there.
