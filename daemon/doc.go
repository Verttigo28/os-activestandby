// Command hasyncd is the daemon and control tool behind the os-activestandby OPNsense plugin: SonicWall-style
// active/standby high availability with one shared IP and MAC per interface.
//
// Roles and state. A node is standalone, primary ("master") or secondary ("standby"); independent of that it is
// either active (owns the addresses, runs the services) or standby (interfaces down, config locked). The persistent
// record is /conf/activestandby/state.json, never config.xml, so replication cannot overwrite a node's own role.
//
// Files by concern:
//
//	daemon.go, main.go       start-up, configuration, listeners (peer API on the HA/management address, control socket)
//	pairing.go, tlsid.go     token-authenticated pairing and certificate pinning (mutual TLS, no CA)
//	heartbeat.go, autofail.go  heartbeats on up to two paths, election, split-brain resolution, failover orchestration
//	promote.go, demote.go    taking over and standing down; devices.go, enforce.go, boot.go keep a standby silent
//	sync.go, packages.go     whole-config replication as numbered revisions, plugin set, update proxy (proxy.go)
//	health.go, drift.go      health monitoring of the active node, drift detection on the standby
//	maintenance.go, preempt.go, fence.go, upgrade.go, updates.go  operator features
//	sessions.go, leases.go   GUI sessions and DHCP leases follow the active node
//	notify.go, metrics.go, history.go, events.go  observability
//	ctl.go, ctl_features.go, kit.go  control interface used by the GUI (through configd) and the console
//
// Only the Go standard library is used. Everything that touches OPNsense internals goes through the PHP helpers
// in src/opnsense/scripts/OPNsense/ActiveStandby and the configd actions, never through a shell string.
package main
