# Security

## Threat model

| Adversary | Assumed capability | Goal we defend against |
|---|---|---|
| On the HA link or management network | read/modify/inject traffic, connect to the peer port | impersonate a node, read or push configuration, trigger a failover |
| Low-privileged GUI user, or API key with limited privileges | call GUI/API endpoints | read secrets (config download), change HA settings, cause an outage |
| Local unprivileged user on a node | read/write world-accessible files, create symlinks in `/tmp` | escalate to root through files the root daemon trusts |
| Compromised peer node | holds the pinned certificate | **out of scope**: a paired peer is trusted by design (it replicates the full configuration, including users and keys, and may promote itself) |

## Controls

**Peer channel**
- Mutual TLS 1.3 between the nodes with self-signed certificates and **no CA**. After pairing every call is pinned to the peer's SHA-256 certificate fingerprint on the client and on the server.
- Pairing uses a one-time 100-bit token. The token is never sent: both sides prove knowledge with HMACs that bind both certificate fingerprints and both nonces, so a relayed or man-in-the-middle session fails. The first request is authenticated too (nothing is disclosed about a node to an unauthenticated caller), the confirm step goes over a connection pinned to the certificate that answered the first request, failures are rate-limited per source address, and the pairing window is bounded in time and memory.
- The peer API listens only on the HA and management addresses. Heartbeats, configuration, sessions, leases and the update proxy all use it; the proxy accepts the peer's address only and reaches only the OPNsense repository hosts.
- Peer-supplied data is validated: package names, lease and session file names (fixed set / fixed pattern, size limits), configuration (size limit, checksum, DOM merge, rollback on failure).

**Local**
- State, certificates, the saved full configuration and logs are root-only (0700/0600) and re-checked every minute. The control socket is root-only.
- All root helper scripts are installed 0755 root and never run from world-writable directories. The update log lives in the state directory, not in `/tmp`.
- No user input reaches a shell: the GUI calls fixed configd actions with positional parameters, validated in PHP and again in Go. The fencing command must be a root-owned, non-writable absolute path and is executed without a shell.
- Webhook delivery does not follow redirects.

**GUI**
- Three privileges (System > Access > Groups or Users): *System: Active/Standby HA (administrator: settings, setup, kit download, break pair)*, *(operator: status, failover, maintenance, upgrade, pre-flight)* and *(read-only status and metrics)*. Give Prometheus scrapers and dashboard users the read-only one, on-call staff the operator one. The administrator privilege also reaches the kit download (a full configuration copy), so treat it as equivalent to an OPNsense administrator.
- Every manual action (failover, promote, demote, break, maintenance, kit, upgrade, settings, service reconfigure) is written to the HA event log and syslog with user and source address.
- Output is escaped in the views; the standby shows a banner on every page.

**Session replication** copies GUI session files between the nodes: they are bearer credentials. They travel only over the pinned connection, are written 0600 root, deletions are mirrored, and the feature can be switched off.

## Things to know

- The secondary kit is a full configuration backup with a pairing token inside: it holds password hashes and keys. It expires after 48 hours but delete it after use, and move it over a trusted channel.
- Releases carry a signed build attestation (`gh attestation verify`) and a SHA-256 list; a signed pkg repository is available once the maintainer has configured the signing key (see [building](building.md#signing)).
- A paired peer is trusted. Protect both nodes as one unit: the HA cable should not leave the rack, and the management network should be restricted.
- Place a firewall rule on the HA link that allows only the peer, and keep the peer API port (default 7690) closed on every other interface; the daemon does not listen there anyway.
- Not covered: IPv6 neighbour announcements, encryption of pfsync (use a dedicated cable or an IPsec-protected link).

## Reporting a vulnerability

Please do not open a public issue. Use GitHub's private vulnerability reporting for this repository (Security tab > Report a vulnerability) or e-mail the maintainer listed in the `Makefile`. Include the version, steps to reproduce and the impact. Expect an acknowledgement within a few days.

## Audit log (0.5.6)

A full audit of the daemon, the GUI and helper scripts, and the CI/supply chain found no critical issue. Fixed in 0.5.6: loose umask inherited by helper processes, tokens visible on the command line, JSON injection through settings, unauthenticated hosts reaching the peer port, peer-controlled clock/maintenance/event flooding, symlink following in privileged writes, a single over-broad GUI privilege, a false-audit GET, raw helper output in the GUI, an over-broad wizard firewall rule, and several supply-chain items (see the changelog).

Known and accepted:
- **A paired peer is root-equivalent on the other node.** It replicates the full configuration (users, SSH keys, cron, the fencing command) and can ask for package installs, reboot and plugin settings. Protect both nodes as one unit and keep the HA link private.
- **Tunnel-type interfaces** (gif, gre, wireguard, openvpn, ...) assigned as interfaces keep their configuration on a standby; their parent data interfaces are down. Review them before relying on the standby being completely silent.
- **The update proxy** authenticates by source address only (plain HTTP on the HA link); it reaches repository hosts only. Use a dedicated HA cable.
- **The signing key** is handed to the third-party FreeBSD VM action that builds the repository; see the release hardening notes in [building](building.md#signing).

## Review of the 0.7 to 0.9 additions

Reviewed with the same method as 0.5.6 (every new input, every new command run as root, every new file). Nothing critical was found; what was tightened is listed.

| Addition | Input it takes | Control |
|---|---|---|
| `POST /v1/abort` and the transition id, epoch and validity (`exp`) on `/v1/promote` | query parameters from the paired peer | only the pinned peer certificate reaches them; `tid` must be 32 hex characters, `epoch` and `exp` are parsed as numbers and a request that is stale, expired, cancelled or repeated is refused or answered idempotently; the transition log is bounded (64 entries). The only side effect of an abort is that the node does not elect itself for 45 s |
| `ifconfig` commands of the warm standby, run as root | device names, MAC and addresses from the configuration plan (`ifaces.php plan`), which comes from the replicated configuration | no shell: arguments are separate. The plan is re-validated in Go (`sanitize`): plain NIC names only (no LAGG, bridge, tunnel, nested VLAN), unicast non-zero MAC, `address/prefix` parsed with `net.ParseCIDR`, MTU range; anything else is not warm. The HA and management devices are never touched |
| `hwmacs.json` (permanent MAC memory) | what `ifconfig` prints | written through the same durable, exclusive-create path as the other state files, mode 0600 |
| `pfctl -si` and `configctl filter reload` after a promotion | none (fixed arguments) | the result decides whether the node stays active |
| `HASYNC_SCRIPTS` and `HASYNC_PATH` (sandbox only) | the environment of the daemon | the daemon runs as root and would execute what they point to, so they are honoured only for directories that root owns and nobody else can write to (symlinks refused); otherwise they are ignored with an error in the log. Unset in production |
| `deploy/apply-settings.sh` | the API key and the settings | the key goes to curl on stdin, never on the command line; every value is URL-encoded, so a value cannot add or change another setting |
| `lab/sandbox` | none | for a test machine only: it creates jails and interfaces, changes one global sysctl (`net.link.bridge.pfil_member`, restored by `down.sh`) and runs the daemons against stub scripts. Never run it on a production firewall |

Known and accepted, in addition to the list above:
- A configuration push carries no epoch and the peer API has no per-message nonce; both are limited to the pinned peer by mutual TLS. The promote path now has the transition id, the epoch and the validity.
- Two nodes without an arbiter cannot rule out a double active when both paths between them are cut while each still sees its own links (see [audit](audit.md)). Fencing is the only software-side answer and is optional.
- The warm standby (experimental) holds the permanent MAC of each NIC on disk and changes MAC addresses at runtime; on hardware this has not been validated (see [warm standby](fast-ha.md)).

## Review of 0.9.1 to 1.0.0 (hot standby, ruleset mirror)

| Surface | Input | Control |
|---|---|---|
| `ifconfig` commands of the fast VLAN takeover and of the hot standby, run as root | device names, addresses, MAC, descriptions from the plan (`ifaces.php plan`, from the replicated configuration) | no shell, one argument per word; every value is checked again in Go (device name pattern, CIDR, MAC, description character set) before it reaches `ifconfig`; a device that is not in the expected state before or after is left to the stock code |
| `POST /v1/ruleset` (new) | a pf ruleset from the paired peer | only the pinned peer certificate reaches it (same trust as `/v1/config`); a standby in hot mode only; at most 8 MiB; SHA-256 header checked; no NUL; must contain the default deny rule; must pass `pfctl -nf` on this node; node-specific `set hostid`/`set limit` lines replaced by this node's own (a node with no ruleset of its own to take them from loads nothing); the operations lock is taken (never during a promotion or demotion); if the peer is no longer heard after the load, the node's own ruleset is loaded again and the mirror stays off for ten minutes |
| `/tmp/rules.debug` read by the active node to send | a file written by OPNsense (root, mode 0600) | the daemon only reads it; a failed load leaves `/tmp/rules.debug.error` and then nothing is sent |
| Transmit watch and default-deny check on a hot standby | `netstat -I <dev> -n -b` and `pfctl -sr` output | fixed arguments; a device that transmits is shut down at once; a standby without default deny is not ready |
| Health loop at 0.5 s while a problem is pending; link hold of a third of the hold time (at least 2 s) | local state | the anti-flapping hold (10 s) and the switch limits still apply; the stall guard follows the detection time |

Known and accepted: a hot standby carries the shared MAC on ports that are up; whether a given NIC ever transmits with it is verified at run time (the transmit watch) and was measured silent on the Intel X710. The mirrored ruleset on the standby is generated by the active node: whoever controls the active node controls the standby's ruleset, as they already control its configuration.

## Review of 1.0.1 to 1.0.2 (persistence, promote request, fencing policy)

An independent gap analysis of the code against the audit brief found these points. Each one was checked in the code first; a claim that did not hold is listed too.

| Point | Finding | Change |
|---|---|---|
| Role file | `Store.Update` changed the state in memory first and wrote the disk afterwards: a failed write left a state in memory that the disk did not have | the new state is built on a copy, checked (known role, known state) and saved; it is published only when `state.json` (the commit point) is written. A failure returns the error and changes nothing. `UpdateSafe` is used where the machine has already changed (a node that went down, a node that took over): there the memory follows the machine, the failure is returned and logged, and the takeover marks the node degraded ("the new role could not be saved"). After a restart such a node reads its old file and boots as a silent standby |
| Role file | `Get` returned a copy that shared the upgrade job and the kit with the live state | `Get` returns a deep copy |
| Role file at start | any valid JSON was accepted (`{}` or `null` gave a standalone node) | a file that is not a state this software wrote counts as damaged: the backup copy is tried, then the node starts as a silent standby |
| `/v1/promote` | `tid`, `epoch` and `exp` were optional and an unparsable value was ignored | all three are required and strictly parsed (400); an epoch more than 1000 ahead and an expiry further than the request validity in the future are refused (400); an old epoch or a passed expiry is refused (409); a refused request leaves no record |
| `/v1/config` | no check of the revision and checksum headers before use. The analysis also said a deposed active node could overwrite the configuration of the node that took over: that is not the case, the receiver only accepts a push while it is itself standby (and checks again under its operations lock), so a deposed node gets a 409 | revision and checksum are required and strictly parsed. No epoch comparison was added: the epoch of a standby does not follow the active node, so a comparison could stop the synchronisation for good after a re-pairing | **Superseded in 1.0.3**: the push now carries `X-Proto` and `X-Epoch`, and the standby refuses an epoch lower than the highest it accepted from this peer (not its own epoch, which is what could wedge the sync).
| Fencing | a silent peer made the standby take over after the detection time even with no way to switch the peer off | new setting `fence_required` (Settings > Safety, off by default): the standby then refuses an automatic takeover caused by a silent peer unless a fencing command is configured and succeeds. The refusal is shown in the status and in the pre-flight. A deliberate failover and the election of a booting pair are not affected |
| Services after a takeover | a service that did not start was only logged | the services that are enabled but not running are started, then checked once; a failure marks the node "active, degraded" with the names |

### Peer certificate lifetime and rotation

Each node creates a self-signed ECDSA P-256 certificate on first start, valid for 20 years. Nobody trusts it through a CA: each peer keeps the SHA-256 of the certificate it saw at pairing and refuses any other, so the date in the certificate is not what protects the link. To replace a certificate (a node was cloned, a private key may have leaked, or 20 years have passed): `hasyncd ctl break` on both nodes, delete `/conf/activestandby/cert.pem` and `key.pem` on the node whose key must change, restart the daemon (`service hasyncd restart`), pair again with a new token. There is no rotation without a new pairing.

### pfsync

pfsync carries the firewall states (addresses, ports, sequence numbers) in the clear and without authentication beyond the network it runs on. It is not encrypted by this plugin and none is added: use a dedicated cable for the HA link, or an IPsec-protected link between the nodes, and a firewall rule on the HA interface that allows only the peer. Anyone who can write on that link can inject states into the other node.

### Go toolchain (found by `govulncheck` in the CI, 2026-10-13)

Releases up to and including 1.0.1 were built with Go 1.27.0, which has nine standard-library vulnerabilities that the daemon reaches through the peer API (`net/http` and its HTTP/2 server: GO-2026-6603, 6605, 6607, 6608, 6610, 6611, 6612, 6613, 6617: denial of service by a crafted HTTP/2 stream, request desynchronisation, a TLS ECH parsing issue). All are fixed in Go 1.27.2, which builds 1.0.2 and later. The peer API only accepts a connection that presents the pinned certificate of the other node, so the reach is limited to a holder of that key or to someone who can already speak TLS to the port before the pin is checked; upgrade the plugin on both nodes anyway.
