package main

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const progressHead = "***GOT REQUEST TO UPDATE***\nCurrently running OPNsense 26.7 at Mon\n"

// The outcome of an update is read from the progress file of the firmware script, never from the exit code of a shell pipeline: the old
// sequence (opnsense-update; pkg upgrade) never replaced the kernel and the base system and was reported as a success.
func TestUpdateOutcomeFromTheFirmwareProgress(t *testing.T) {
	for name, c := range map[string]struct {
		text             string
		done, ok, reboot bool
	}{
		"nothing":             {"", false, false, false},
		"a check, not us":     {"***GOT REQUEST TO CHECK FOR UPDATES***\n***DONE***\n", false, false, false},
		"running":             {progressHead + "Fetching\n", false, false, false},
		"done":                {progressHead + "***DONE***\n", true, true, false},
		"reboot":              {progressHead + "***REBOOT***\n", true, true, true},
		"partial failure":     {progressHead + "Partial update failure detected: report this error log to OPNsense.\n***DONE***\n", true, false, false},
		"unsafe argument":     {progressHead + "firmware: safety violation in argument during UPDATE\n***DONE***\n", true, false, false},
		"failure then reboot": {progressHead + "Partial update failure detected\n***REBOOT***\n", true, false, true},
	} {
		done, ok, reboot := updateOutcome(c.text)
		if done != c.done || ok != c.ok || reboot != c.reboot {
			t.Errorf("%s: done=%v ok=%v reboot=%v, want %v %v %v", name, done, ok, reboot, c.done, c.ok, c.reboot)
		}
	}
}

// the daemon of the standby restarts during the update (the upgrade of this plugin does it): the new daemon reads where the update is
func TestUpdateStateAfterADaemonRestart(t *testing.T) {
	started := time.Now()
	for name, c := range map[string]struct {
		text          string
		started       time.Time
		running, done bool
		rc            int
		reboot        bool
	}{
		"never started":       {progressHead + "***DONE***\n", time.Time{}, false, false, 0, false},
		"no progress":         {"", started, false, false, 0, false},
		"running":             {progressHead + "Fetching\n", started, true, false, 0, false},
		"finished":            {progressHead + "***DONE***\n", started, false, true, 0, false},
		"finished and reboot": {progressHead + "***REBOOT***\n", started, false, true, 0, true},
		"failed":              {progressHead + "Partial update failure detected\n***DONE***\n", started, false, true, 1, false},
		"progress of a check": {"***GOT REQUEST TO CHECK FOR UPDATES***\nFetching\n", started, false, false, 0, false},
	} {
		r, d, rc, reb := updateFromProgress(c.text, c.started)
		if r != c.running || d != c.done || rc != c.rc || reb != c.reboot {
			t.Errorf("%s: running=%v done=%v rc=%d reboot=%v", name, r, d, rc, reb)
		}
	}
}

// "nothing left" is said by a check that reached the repository, not by the absence of an answer
func TestPendingUpdatesNeedsARealCheck(t *testing.T) {
	if _, checked, problem := pendingUpdates(UpdateInfo{}); checked || problem == "" {
		t.Fatal("no result of a check is not 'nothing pending'")
	}
	if _, checked, problem := pendingUpdates(UpdateInfo{Checked: true, Connection: "unresolved"}); checked || !strings.Contains(problem, "unresolved") {
		t.Fatalf("a check that did not reach the repository is an error: %v %q", checked, problem)
	}
	p, checked, problem := pendingUpdates(UpdateInfo{Checked: true, Connection: "ok"})
	if !checked || problem != "" || len(p) != 0 {
		t.Fatalf("a clean check: %v %v %q", p, checked, problem)
	}
	u := UpdateInfo{Checked: true, Connection: "ok", Available: true, Count: 15, Packages: []string{"base", "kernel"}, MajorVersion: "27.1"}
	p, _, _ = pendingUpdates(u)
	if got := strings.Join(p, ","); got != "base,kernel,and 13 more,major upgrade to 27.1" {
		t.Fatalf("pending: %s", got)
	}
}

func TestUpdateScriptRunsTheOfficialFirmwareUpdateThroughTheProxy(t *testing.T) {
	s := updateScript("http://10.9.9.1:7701")
	for _, want := range []string{"HTTP_PROXY=http://10.9.9.1:7701", "HTTPS_PROXY=http://10.9.9.1:7701", "firmware/launcher.sh update"} {
		if !strings.Contains(s, want) {
			t.Errorf("%q is missing from %q", want, s)
		}
	}
	if strings.Contains(s, "pkg upgrade") {
		t.Error("the update must be the one of the firmware page, not a bare pkg upgrade (it never replaces the kernel and the base system)")
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
