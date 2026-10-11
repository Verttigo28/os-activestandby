package main

import (
	"os"
	"sync/atomic"
	"time"
)

// Drift detection. Two independent signals, both repaired by asking the active node for a full push:
//   * integrity of the standby's own files: the SHA-256 of active.xml and the standby view of config.xml are recorded
//     whenever this daemon writes them; a later mismatch means someone or something changed them behind our back;
//   * agreement between the nodes: the standby reports the hash of the last revision it applied; the active node
//     compares it with what it believes it delivered.

type driftState struct {
	reason   atomic.Value // string, "" = none
	mismatch time.Time    // active side: first time the nodes disagreed
}

func (d *Daemon) driftDetected() bool {
	v, _ := d.drift.reason.Load().(string)
	return v != ""
}

func (d *Daemon) driftReason() string {
	v, _ := d.drift.reason.Load().(string)
	return v
}

func fileSHA(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return shaHex(b)
}

// recordIntegrity stores the hashes of the files this daemon just wrote on a standby.
func (d *Daemon) recordIntegrity() {
	a, v := fileSHA(activeXML(d.store.dir)), fileSHA(configPath())
	d.store.Update(func(s *State) { s.ActiveSHA, s.ViewSHA = a, v })
	d.drift.reason.Store("")
}

func (d *Daemon) driftLoop() {
	for range time.Tick(20 * time.Second) {
		d.driftStep()
	}
}

func (d *Daemon) driftStep() {
	st := d.store.Get()
	if st.PeerFP == "" {
		return
	}
	switch st.State {
	case StateStandby:
		if time.Now().Before(d.unlockedUntil.Get()) { // the operator deliberately unlocked the config (update, settings)
			return
		}
		if st.ActiveSHA == "" || !d.ops.mu.TryLock() { // nothing recorded yet, or an operation is in progress
			return
		}
		a, v := fileSHA(activeXML(d.store.dir)), fileSHA(configPath())
		d.ops.mu.Unlock()
		why := ""
		switch {
		case a != st.ActiveSHA:
			why = "the standby's saved configuration (active.xml) was changed outside of sync"
		case v != st.ViewSHA:
			why = "the standby's config.xml was changed outside of sync"
		}
		if why != "" {
			if !d.driftDetected() {
				d.event("warning", "DRIFT: %s; requesting a full resync from the active node", why)
			}
			d.drift.reason.Store(why)
		}
	case StateActive:
		alive, _, peer := d.hb.Snapshot()
		if !alive || peer.State != StateStandby || st.AckedSHA == "" || st.SyncError != "" || peer.Applied == "" {
			d.drift.mismatch = time.Time{}
			if d.driftDetected() && alive {
				d.drift.reason.Store("")
			}
			return
		}
		if peer.Applied == st.AckedSHA {
			d.drift.mismatch = time.Time{}
			d.drift.reason.Store("")
			return
		}
		if d.drift.mismatch.IsZero() {
			d.drift.mismatch = time.Now()
			return
		}
		if time.Since(d.drift.mismatch) > 30*time.Second { // give a fresh push time to arrive
			d.event("warning", "DRIFT: the standby reports a different configuration than the one delivered to it; pushing the full configuration again")
			d.drift.reason.Store("the standby holds a different configuration than the active node delivered")
			d.drift.mismatch = time.Now()
			d.store.Update(func(s *State) { s.AckedSHA = "" })
			d.wakeSync()
		}
	}
}
