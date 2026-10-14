package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The guided upgrade is driven here with the standby, its update and the network replaced by fakes: it proves the decisions of the
// job (what ends a round, what fails it), not an update on a real OPNsense (see docs/validation.md).

func jobNode(t *testing.T, phase, sub string) *Daemon {
	t.Helper()
	d := newHANode(t, activeState(RoleMaster, 5))
	d.pluginVer = "1.0.5"
	d.act.peerCall = func(string, time.Duration) (map[string]any, error) { return map[string]any{}, nil }
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	d.store.Update(func(s *State) {
		s.Maintenance = true
		s.Job = &Job{ID: "1", Phase: phase, Sub: sub, Origin: RoleMaster, Started: old, Updated: time.Now().UTC().Format(time.RFC3339)}
	})
	return d
}

func jobPhase(d *Daemon) string { return d.store.Get().Job.Phase }

// The update of OPNsense reboots the node when it replaced the kernel or the base system: the silence of the standby is the end of the
// update, not a failure, and the round goes on with waiting for it.
func TestAStandbyRebootedByItsUpdateIsWaitedFor(t *testing.T) {
	d := jobNode(t, "r1-update", "running")
	d.act.peerCall = func(string, time.Duration) (map[string]any, error) { return nil, errors.New("connection refused") }
	seePeer(d, hbMsg{State: StateStandby})
	silence(d, time.Minute)
	d.jobStep()
	if got := jobPhase(d); got != "r1-wait" {
		t.Fatalf("phase %q: a standby that went silent during its update has been rebooted by it", got)
	}
}

func TestAStandbyThatAnnouncesItsRebootIsWaitedFor(t *testing.T) {
	d := jobNode(t, "r1-update", "running")
	d.act.peerCall = func(string, time.Duration) (map[string]any, error) {
		return map[string]any{"done": true, "rc": float64(0), "reboot": true}, nil
	}
	seePeer(d, hbMsg{State: StateStandby})
	d.jobStep()
	if got := jobPhase(d); got != "r1-wait" {
		t.Fatalf("phase %q, want r1-wait (the node reboots itself: the daemon must not reboot it a second time)", got)
	}
}

func TestAFailedUpdateStopsTheJob(t *testing.T) {
	d := jobNode(t, "r1-update", "running")
	d.act.peerCall = func(string, time.Duration) (map[string]any, error) {
		return map[string]any{"done": true, "rc": float64(1), "tail": "Partial update failure"}, nil
	}
	seePeer(d, hbMsg{State: StateStandby})
	d.jobStep()
	if j := d.store.Get().Job; j.Phase != "failed" || !strings.Contains(j.Err, "Partial update failure") {
		t.Fatalf("%q %q", j.Phase, j.Err)
	}
}

// back from its reboot, the standby is not "done" yet: it is asked what it still has to update
func TestAfterTheRebootTheStandbyIsVerified(t *testing.T) {
	d := jobNode(t, "r1-wait", "")
	seePeer(d, hbMsg{State: StateStandby})
	d.store.Update(func(s *State) { s.Job.WaitSince = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339) })
	d.jobStep()
	if got := jobPhase(d); got != "r1-verify" {
		t.Fatalf("phase %q, want r1-verify", got)
	}
}

func TestAnUpdateThatLeftThingsPendingFailsTheRound(t *testing.T) {
	d := jobNode(t, "r1-verify", "")
	seePeer(d, hbMsg{State: StateStandby})
	d.act.verify = func() ([]string, error) { return []string{"base", "kernel"}, nil }
	d.jobStep()
	if j := d.store.Get().Job; j.Phase != "failed" || !strings.Contains(j.Err, "base, kernel") {
		t.Fatalf("the standby still has the kernel and the base system to update, the round is not over: %q %q", j.Phase, j.Err)
	}
}

func TestAVerifiedStandbyLetsTheFailoverHappen(t *testing.T) {
	d := jobNode(t, "r1-verify", "")
	seePeer(d, hbMsg{State: StateStandby})
	d.act.verify = func() ([]string, error) { return nil, nil }
	d.jobStep()
	if got := jobPhase(d); got != "failover" {
		t.Fatalf("phase %q", got)
	}
}

// a check that could not run is retried, it is neither a success nor an immediate failure
func TestAVerificationThatCannotRunIsRetried(t *testing.T) {
	d := jobNode(t, "r2-verify", "")
	seePeer(d, hbMsg{State: StateStandby})
	d.act.verify = func() ([]string, error) {
		return nil, errors.New("the standby could not reach the repository (unresolved)")
	}
	d.jobStep()
	if got := jobPhase(d); got != "r2-verify" {
		t.Fatalf("phase %q: one failed check must not end the job", got)
	}
	d.store.Update(func(s *State) { s.Job.Updated = time.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339) })
	d.jobStep()
	if got := jobPhase(d); got != "failed" {
		t.Fatalf("phase %q: after 15 minutes of failed checks the job stops", got)
	}
}

// Round 2 over: the plugin versions are compared before anything is called done, and before the role goes back (the node that gave the
// role back does not drive the job any more).
func TestTheJobEndsWithTheSamePluginOnBothNodes(t *testing.T) {
	d := jobNode(t, "r2-verify", "")
	seePeer(d, hbMsg{State: StateStandby, Plug: "1.0.5"})
	d.act.verify = func() ([]string, error) { return nil, nil }
	d.jobStep()
	if got := jobPhase(d); got != "plugin" {
		t.Fatalf("phase %q, want plugin", got)
	}
	d.jobStep()
	if got := jobPhase(d); got != "done" {
		t.Fatalf("phase %q: the same plugin on both nodes ends the job", got)
	}
	if d.store.Get().Maintenance {
		t.Fatal("maintenance mode is switched off at the end")
	}
}

func TestTheFailbackComesAfterThePluginStep(t *testing.T) {
	d := jobNode(t, "plugin", "")
	d.store.Update(func(s *State) { s.Job.Failback = true })
	seePeer(d, hbMsg{State: StateStandby, Plug: "1.0.5"})
	d.jobStep()
	if got := jobPhase(d); got != "failback" {
		t.Fatalf("phase %q, want failback", got)
	}
}

func TestADifferentPluginVersionIsNotDone(t *testing.T) {
	d := jobNode(t, "plugin", "")
	seePeer(d, hbMsg{State: StateStandby, Plug: "1.0.4"})
	d.jobStep()
	if got := jobPhase(d); got == "done" || got == "failback" {
		t.Fatalf("phase %q: the nodes run different versions of the plugin", got)
	}
}
