package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The write-ahead record of a role transition and the recovery at the start of the daemon (intent.go): a promotion or a demotion
// that the daemon did not finish must never be taken for the role written before it, and the node must come back as standby.

type recorder struct {
	mu    sync.Mutex
	cmds  []string
	views int
}

func (r *recorder) install(d *Daemon, viewErr error) {
	d.act.sh = func(name string, args ...string) (string, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.cmds = append(r.cmds, strings.Join(append([]string{name}, args...), " "))
		return "", nil
	}
	d.act.standbyView = func(string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.views++
		return viewErr
	}
}

func (r *recorder) ran(prefix string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.cmds {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// a node on disk: state.json with st, a valid active.xml, and the record if in != nil
func nodeOnDisk(t *testing.T, st State, in *transitionIntent, rawIntent string) *Daemon {
	t.Helper()
	d := newHANode(t, st)
	t.Setenv("HASYNC_CONFIG", filepath.Join(d.store.dir, "config.xml"))
	if err := d.store.Update(func(s *State) { *s = st }); err != nil { // the file on disk says the same as the memory
		t.Fatal(err)
	}
	full := "<?xml version=\"1.0\"?>\n<opnsense>" + strings.Repeat("<x>filler</x>", 30) + "</opnsense>\n"
	if err := os.WriteFile(activeXML(d.store.dir), []byte(full), 0o600); err != nil {
		t.Fatal(err)
	}
	switch {
	case in != nil:
		in.Started = "2026-10-14T08:00:00Z"
		b, _ := json.Marshal(in)
		if err := os.WriteFile(intentPath(d.store.dir), b, 0o600); err != nil {
			t.Fatal(err)
		}
	case rawIntent != "":
		os.WriteFile(intentPath(d.store.dir), []byte(rawIntent), 0o600)
	}
	return d
}

func recordExists(d *Daemon) bool {
	_, err := os.Stat(intentPath(d.store.dir))
	return err == nil
}

func stateOnDisk(t *testing.T, d *Daemon) State {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(d.store.dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestInterruptedPromotionComesBackAsStandby(t *testing.T) {
	// standby at epoch 6 started a promotion and the daemon stopped in the middle: config.xml may hold the full configuration,
	// some interfaces the shared addresses
	d := nodeOnDisk(t, standbyState(RoleStandby, 6), &transitionIntent{Op: intentPromote, EpochBefore: 6, Reason: "peer request"}, "")
	var r recorder
	r.install(d, nil)
	d.recoverTransition()
	if d.store.Get().State != StateStandby || stateOnDisk(t, d).State != StateStandby {
		t.Fatal("must be standby in memory and on disk")
	}
	if r.views != 1 {
		t.Fatalf("the standby view must be written back, %d times", r.views)
	}
	if !r.ran("configctl ipsec stop") || !r.ran("pkill -x dpinger") || !r.ran("configctl filter reload") {
		t.Fatalf("the HA services must be stopped and the standby ruleset loaded: %v", r.cmds)
	}
	if recordExists(d) {
		t.Fatal("a finished recovery removes the record")
	}
}

func TestInterruptedDemotionIsNotTakenForActive(t *testing.T) {
	// scenario A of the brief: the active node took its devices down and stopped before it wrote its new role. On disk it is still
	// "active": without the record a restart would believe it serves.
	d := nodeOnDisk(t, activeState(RoleMaster, 7), &transitionIntent{Op: intentDemote, EpochBefore: 7, Reason: "failover"}, "")
	var r recorder
	r.install(d, nil)
	d.recoverTransition()
	if d.store.Get().State != StateStandby || stateOnDisk(t, d).State != StateStandby {
		t.Fatalf("an interrupted demotion must end as standby, got %s", d.store.Get().State)
	}
	if d.store.Get().Epoch != 7 {
		t.Fatal("the recovery does not touch the epoch")
	}
	if recordExists(d) {
		t.Fatal("record not removed")
	}
}

func TestCommittedPromotionWithALeftoverRecordStaysActive(t *testing.T) {
	// the takeover reached its commit point (active, newer epoch), only the removal of the record is missing: nothing to undo
	d := nodeOnDisk(t, activeState(RoleStandby, 7), &transitionIntent{Op: intentPromote, EpochBefore: 6}, "")
	var r recorder
	r.install(d, nil)
	d.recoverTransition()
	if d.store.Get().State != StateActive || r.views != 0 || len(r.cmds) != 0 {
		t.Fatalf("a committed takeover must not be undone: state %s, views %d, commands %v", d.store.Get().State, r.views, r.cmds)
	}
	if recordExists(d) {
		t.Fatal("record not removed")
	}
}

func TestPromotionRecordWithoutANewerEpochIsNotACommit(t *testing.T) {
	// active on disk but with the epoch of before the record: it is not the commit of this promotion (it can only come from an old
	// state): safe side
	d := nodeOnDisk(t, activeState(RoleStandby, 6), &transitionIntent{Op: intentPromote, EpochBefore: 6}, "")
	var r recorder
	r.install(d, nil)
	d.recoverTransition()
	if d.store.Get().State != StateStandby {
		t.Fatal("must go to standby")
	}
}

func TestUnreadableRecordCountsAsInterrupted(t *testing.T) {
	for _, raw := range []string{"{", "null", `{"op":"reboot"}`, ""} {
		d := nodeOnDisk(t, activeState(RoleMaster, 7), nil, "x")
		os.WriteFile(intentPath(d.store.dir), []byte(raw), 0o600)
		var r recorder
		r.install(d, nil)
		d.recoverTransition()
		if d.store.Get().State != StateStandby {
			t.Fatalf("%q: a record that cannot be read must be taken as an interrupted transition", raw)
		}
	}
}

func TestNoRecordNoRecovery(t *testing.T) {
	d := nodeOnDisk(t, activeState(RoleMaster, 7), nil, "")
	var r recorder
	r.install(d, nil)
	d.recoverTransition()
	if d.store.Get().State != StateActive || len(r.cmds) != 0 || r.views != 0 {
		t.Fatal("a daemon that restarts without a record keeps its role and touches nothing")
	}
}

func TestRecordOfAnUnpairedNodeIsDropped(t *testing.T) {
	d := nodeOnDisk(t, State{Role: RoleStandalone}, &transitionIntent{Op: intentDemote}, "")
	var r recorder
	r.install(d, nil)
	d.recoverTransition()
	if recordExists(d) || len(r.cmds) != 0 || d.store.Get().State != "" {
		t.Fatalf("nothing to protect when not paired: cmds %v state %q", r.cmds, d.store.Get().State)
	}
}

// the recovery itself fails: the record stays so that the next start tries again, and the node is standby in memory anyway
func TestRecoveryThatCannotFinishKeepsTheRecord(t *testing.T) {
	t.Run("the standby view cannot be written", func(t *testing.T) {
		d := nodeOnDisk(t, activeState(RoleMaster, 7), &transitionIntent{Op: intentDemote, EpochBefore: 7}, "")
		var r recorder
		r.install(d, errors.New("stdview: php died"))
		d.recoverTransition()
		if d.store.Get().State != StateStandby || !recordExists(d) {
			t.Fatalf("state %s, record kept %v", d.store.Get().State, recordExists(d))
		}
	})
	t.Run("the disk refuses the new role", func(t *testing.T) {
		d := nodeOnDisk(t, activeState(RoleMaster, 7), &transitionIntent{Op: intentDemote, EpochBefore: 7}, "")
		var r recorder
		r.install(d, nil)
		failDisk(t, errors.New("no space left on device"), "state.json")
		d.recoverTransition()
		if d.store.Get().State != StateStandby {
			t.Fatal("memory must say standby")
		}
		if !recordExists(d) {
			t.Fatal("the disk still says active: the record must stay for the next start")
		}
	})
}

// the commit of a takeover removes the record only when it is durable
func TestCommitOfATakeoverAndTheRecord(t *testing.T) {
	d := nodeOnDisk(t, standbyState(RoleStandby, 6), &transitionIntent{Op: intentPromote, EpochBefore: 6}, "")
	d.commitActive()
	if recordExists(d) {
		t.Fatal("a durable commit removes the record")
	}
	d = nodeOnDisk(t, standbyState(RoleStandby, 6), &transitionIntent{Op: intentPromote, EpochBefore: 6}, "")
	failDisk(t, errors.New("input/output error"), "state.json")
	d.commitActive()
	if !recordExists(d) || d.store.Get().State != StateActive {
		t.Fatal("a commit that is not durable keeps the record (a restart brings the node back to standby) and the node serves meanwhile")
	}
	if !strings.Contains(d.degradedReason(), "back to standby") {
		t.Fatalf("not shown: %q", d.degradedReason())
	}
}
