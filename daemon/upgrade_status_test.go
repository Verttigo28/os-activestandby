package main

import (
	"sync/atomic"
	"testing"
	"time"
)

// the update of a standby goes on when its daemon restarts (the upgrade of this plugin restarts it): the new daemon reads the
// state of the update from its log instead of answering "not done" for ever
func TestUpdateStateFromItsLog(t *testing.T) {
	for log, want := range map[string][3]any{
		"":                       {false, false, 0},
		"Fetching...\n":          {true, false, 0},
		"opnsense-update rc=0\n": {true, false, 0},
		"opnsense-update rc=0\npkg upgrade rc=0\n": {false, true, 0},
		"pkg upgrade rc=3\n":                       {false, true, 3},
		"pkg upgrade rc=x\n":                       {false, true, 1},
	} {
		r, d, rc := updateFromLog(log)
		if r != want[0] || d != want[1] || rc != want[2] {
			t.Errorf("%q: running=%v done=%v rc=%d, want %v", log, r, d, rc, want)
		}
	}
}

// Maintenance mode pauses the takeovers, not the election of a pair where both nodes answer and nobody is active (a failed hand-over
// of the guided upgrade): that pair would otherwise serve nothing until the maintenance mode expires.
func TestElectionRunsInMaintenanceWhenNobodyIsActive(t *testing.T) {
	d := newHANode(t, standbyState(RoleMaster, 5))
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	d.store.Update(func(s *State) { s.Maintenance, s.MaintenanceUntil = true, until })
	var promoted atomic.Int32
	d.act.promote = func(string) error { promoted.Add(1); return nil }
	seePeer(d, hbMsg{State: StateStandby})
	d.autoStep()
	if promoted.Load() != 0 {
		t.Fatal("not at once")
	}
	d.auto.noActiveSince = time.Now().Add(-(electionMaster + electionInMaintenance + time.Second))
	seePeer(d, hbMsg{State: StateStandby})
	d.autoStep()
	if promoted.Load() != 1 {
		t.Fatal("nobody active and both nodes answer: the master must take over, maintenance or not")
	}
}

// ... but a silent peer is not taken for dead in maintenance mode, and an active peer is not preempted
func TestMaintenanceStillPausesTheTakeovers(t *testing.T) {
	d := newHANode(t, standbyState(RoleMaster, 5))
	d.cfg.Preempt = true
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	d.store.Update(func(s *State) { s.Maintenance, s.MaintenanceUntil = true, until })
	var promoted atomic.Int32
	d.act.promote = func(string) error { promoted.Add(1); return nil }
	silence(d, time.Hour)
	d.autoStep()
	seePeer(d, hbMsg{State: StateActive})
	d.auto.peerActiveSince = time.Now().Add(-time.Hour)
	d.autoStep()
	if promoted.Load() != 0 {
		t.Fatal("maintenance mode must pause the takeover after a silence and the preemption")
	}
}
