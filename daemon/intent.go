package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// Write-ahead record of a role transition.
//
// A promotion or a demotion changes the machine (configuration file, interfaces, addresses, MAC, packet filter, services) in many
// steps, and the role in state.json is only written at the end (the commit point). If the daemon stops in between (killed, crashed,
// a panic), a restart without a reboot would read the role of BEFORE the transition while the machine is somewhere in the middle:
//
//   - a demotion cut short: state.json still says active, the interfaces are already down. The node would believe it serves;
//   - a promotion cut short: state.json still says standby, but config.xml may already hold the full configuration and some
//     interfaces the shared addresses. The enforcer would pull the devices down, but nothing would put the standby view back.
//
// So before the first change of the machine the daemon writes transition.json (durably), and removes it only after the commit. A
// restart that finds it knows that a transition did not finish and brings the node to the one state that is always safe: standby,
// with the standby view written, the HA services stopped and the devices down. Who serves is then decided by the peer or by the
// election, never by an old role on disk. (A reboot needs none of this: a paired node always boots as standby, see boot.go.)

const (
	intentPromote = "promote"
	intentDemote  = "demote"
)

type transitionIntent struct {
	Op          string `json:"op"`
	EpochBefore uint64 `json:"epoch_before"`
	StateBefore string `json:"state_before"`
	Reason      string `json:"reason"`
	Started     string `json:"started"`
}

func intentPath(dir string) string { return filepath.Join(dir, "transition.json") }

// beginIntent records that a transition starts. A record that cannot be written does not stop the transition (an active node that
// died must still be replaced when the disk of the standby is full); the failure is reported, and a reboot still ends as standby.
func (d *Daemon) beginIntent(op, reason string) {
	st := d.store.Get()
	b, _ := json.Marshal(transitionIntent{Op: op, EpochBefore: st.Epoch, StateBefore: st.State, Reason: reason, Started: time.Now().UTC().Format(time.RFC3339Nano)})
	if err := fsWrite(intentPath(d.store.dir), b, 0o600); err != nil {
		d.event("error", "the start of the %s could not be recorded (%v): if this daemon stops before it ends, a restart cannot detect it (a reboot still comes back as standby)", op, err)
	}
}

// endIntent removes the record once the transition reached its commit point.
func (d *Daemon) endIntent() {
	if err := os.Remove(intentPath(d.store.dir)); err != nil && !os.IsNotExist(err) {
		log.Printf("transition record not removed: %v", err)
		return
	}
	syncDir(d.store.dir)
}

// readIntent returns nil, nil when there is no record. A record that cannot be read is an error: the caller treats it as an
// interrupted transition of unknown kind.
func readIntent(dir string) (*transitionIntent, error) {
	b, err := os.ReadFile(intentPath(dir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var in transitionIntent
	if err := json.Unmarshal(b, &in); err != nil || (in.Op != intentPromote && in.Op != intentDemote) {
		return nil, fmt.Errorf("unreadable transition record (%v)", err)
	}
	return &in, nil
}

// committed: the transition reached its commit point and only the removal of the record is missing. Only a promotion can be in that
// situation without harm: its commit makes the node active with a newer epoch. A demotion record is never taken as finished, because
// its steps after the commit point (the standby view) are exactly what a restart must redo, and redoing them is harmless.
func (in transitionIntent) committed(st State) bool {
	return in.Op == intentPromote && st.State == StateActive && st.Epoch > in.EpochBefore
}

// recoverTransition runs once at the start of the daemon, before the loops that act on the role.
func (d *Daemon) recoverTransition() {
	in, err := readIntent(d.store.dir)
	if in == nil && err == nil {
		return
	}
	st := d.store.Get()
	if err == nil && in.committed(st) {
		log.Printf("the %s of %s had reached its commit point: transition record removed", in.Op, in.Started)
		d.endIntent()
		return
	}
	what := "a transition whose record is unreadable"
	if err == nil {
		what = fmt.Sprintf("the %s started at %s (%s)", in.Op, in.Started, in.Reason)
	} else {
		log.Printf("transition record: %v", err)
	}
	if st.PeerFP == "" { // not paired (any more): there is no shared identity to protect
		d.endIntent()
		return
	}
	d.event("error", "%s did not finish (the daemon stopped in the middle): this node goes to STANDBY; the peer or the election decides who serves", what)
	d.ops.mu.Lock()
	defer d.ops.mu.Unlock()
	for _, c := range haStop {
		d.run(c[0], c[1:]...)
	}
	full := activeXML(d.store.dir)
	viewOK := false
	if activeValid(d.store.dir) {
		if err := d.doStandbyView(full); err != nil {
			d.event("error", "recovery of the interrupted transition: the standby view could not be written (%v); the record is kept and the recovery runs again at the next start", err)
		} else {
			viewOK = true
		}
	} else {
		d.event("error", "recovery of the interrupted transition: no valid saved configuration (active.xml); the devices are kept down from the configuration in use, a full resync from the active node is needed")
	}
	saveErr := d.store.UpdateSafe(func(s *State) { s.State = StateStandby })
	if saveErr != nil {
		d.event("error", "recovery of the interrupted transition: the standby role could not be saved (%v); the record is kept and the recovery runs again at the next start", saveErr)
	}
	d.ops.cache, d.ops.mtime = IfaceSet{}, time.Time{}
	d.enforceOnce()
	d.run("configctl", "filter", "reload")
	if saveErr == nil && viewOK { // the recovery is over only when the disk agrees and the standby view is in place
		d.endIntent()
	}
}

// doStandbyView writes the standby view of the configuration (a seam for the tests).
func (d *Daemon) doStandbyView(full string) error {
	if d.act != nil && d.act.standbyView != nil {
		return d.act.standbyView(full)
	}
	return d.writeStandbyView(full)
}
