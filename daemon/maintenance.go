package main

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Maintenance mode: automatic failover, preemption and health-driven switching are paused on BOTH nodes
// (split-brain resolution stays active). It switches itself off after a few hours so it cannot be forgotten.

const defaultMaintenanceHours = 4

func (d *Daemon) maintenanceOn() bool {
	st := d.store.Get()
	if !st.Maintenance {
		return false
	}
	if st.MaintenanceUntil != "" {
		if t, err := time.Parse(time.RFC3339, st.MaintenanceUntil); err == nil && time.Now().After(t) {
			go d.setMaintenance(false, 0, true)
			return false
		}
	}
	return true
}

// setMaintenance changes the mode locally and, unless the change came from the peer, tells the peer too.
func (d *Daemon) setMaintenance(on bool, hours int, fromPeer bool) {
	if hours <= 0 {
		hours = defaultMaintenanceHours
	}
	until := ""
	if on {
		until = time.Now().Add(time.Duration(hours) * time.Hour).UTC().Format(time.RFC3339)
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	d.store.Update(func(s *State) { s.Maintenance, s.MaintenanceUntil, s.MaintAt = on, until, at })
	if on {
		d.event("change", "maintenance mode ON for %d h: automatic failover, preemption and health switching are paused", hours)
	} else {
		d.event("change", "maintenance mode OFF: automatic failover resumed")
	}
	if !fromPeer && d.store.Get().PeerFP != "" {
		q := "/v1/maintenance?on=0"
		if on {
			q = fmt.Sprintf("/v1/maintenance?on=1&hours=%d", hours)
		}
		if _, err := d.peerCall(q, 5*time.Second); err != nil {
			d.event("warning", "could not tell the peer about the maintenance mode change: %v", err)
		}
	}
}

func (d *Daemon) handleMaintenance(w http.ResponseWriter, r *http.Request) {
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	d.setMaintenance(r.URL.Query().Get("on") == "1", hours, true)
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// reconcileMaintenance adopts the peer's maintenance setting when it is newer than ours. This repairs the case where a node
// was down while the mode was switched (it would otherwise keep a stale "on" and pause its own failover).
func (d *Daemon) reconcileMaintenance(m hbMsg) {
	st := d.store.Get()
	if m.MaintAt == "" || m.MaintAt <= st.MaintAt { // RFC3339Nano UTC strings sort chronologically
		return
	}
	d.store.Update(func(s *State) { s.Maintenance, s.MaintenanceUntil, s.MaintAt = m.Maint, m.MaintUntil, m.MaintAt })
	if m.Maint != st.Maintenance {
		d.event("change", "maintenance mode %s (adopted from the peer, which changed it more recently)", map[bool]string{true: "ON", false: "OFF"}[m.Maint])
	}
}
