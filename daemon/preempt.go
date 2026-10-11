package main

import "time"

// Preemption (opt-in): the primary node takes the active role back once it is standby again and the peer has been active,
// reachable, healthy and in sync for the configured delay. Never during maintenance or while the versions differ.
// The hand-over itself is the normal graceful failover, including its guard against losing unreplicated edits.

func (d *Daemon) preemptStep(st State, peer hbMsg) {
	if !d.cfg.Preempt || st.Role != RoleMaster {
		d.auto.peerActiveSince = time.Time{}
		return
	}
	if _, mism := d.versionInfo(); mism || peer.Health != "" || st.SyncError != "" {
		d.auto.peerActiveSince = time.Time{}
		return
	}
	if d.auto.peerActiveSince.IsZero() {
		d.auto.peerActiveSince = time.Now()
		return
	}
	delay := time.Duration(d.cfg.PreemptDelay) * time.Second
	if delay <= 0 {
		delay = 300 * time.Second
	}
	if time.Since(d.auto.peerActiveSince) < delay {
		return
	}
	d.auto.lastChange = time.Now()
	d.auto.peerActiveSince = time.Now() // if it does not work now, wait the full delay again
	d.event("change", "preemption: this is the primary and the peer has been active and healthy for %s; taking the active role back", delay)
	go func() {
		if _, err := d.failover(false); err != nil {
			d.event("warning", "preemption not possible now: %v", err)
		}
	}()
}
