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
- Two privileges: *Active/Standby HA (full control)* and *Active/Standby HA (read-only status and metrics)*. Give Prometheus scrapers and dashboard users the read-only one. The secondary kit download, settings, failover and every other action need full control.
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
