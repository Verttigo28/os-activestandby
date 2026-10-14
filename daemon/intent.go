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

// runIntentPath is the second copy of the record. The record only has to survive a restart of the daemon (a reboot always ends as
// standby), so /var/run, which is not the file system of /conf, is enough: a full or failing /conf does not lose it.
const runIntentPath = "/var/run/activestandby.transition"

// runIntentFile is the second copy for this daemon (the tests give each node its own).
func (d *Daemon) runIntentFile() string {
	if d.runIntent != "" {
		return d.runIntent
	}
	return runIntentPath
}

// beginIntent records that a transition starts, in the state directory and in /var/run. It fails only if neither copy can be written:
// the caller decides (a promotion is then refused: a takeover that could not be recognised after a crash is not started; a
// demotion goes on, standing down is the safe direction).
func (d *Daemon) beginIntent(op, reason string) error {
	st := d.store.Get()
	b, _ := json.Marshal(transitionIntent{Op: op, EpochBefore: st.Epoch, StateBefore: st.State, Reason: reason, Started: time.Now().UTC().Format(time.RFC3339Nano)})
	err1 := fsWrite(intentPath(d.store.dir), b, 0o600)
	err2 := fsWrite(d.runIntentFile(), b, 0o600)
	switch {
	case err1 != nil && err2 != nil:
		d.event("error", "the start of the %s could not be recorded anywhere (%v; %v)", op, err1, err2)
		return fmt.Errorf("transition record not written: %v; %v", err1, err2)
	case err1 != nil:
		d.event("warning", "the start of the %s is recorded in %s only: the state directory refuses it (%v)", op, d.runIntentFile(), err1)
	}
	return nil
}

// endIntent removes both copies of the record once the transition reached its commit point.
func (d *Daemon) endIntent() {
	for _, p := range []string{intentPath(d.store.dir), d.runIntentFile()} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("transition record %s not removed: %v", p, err)
		}
	}
	syncDir(d.store.dir)
}

// readIntent returns the record (the copy in the state directory, else the one in /var/run), or nil, nil when there is none. A record
// that cannot be read is an error: the caller treats it as an interrupted transition of unknown kind.
func readIntent(dir, runFile string) (*transitionIntent, error) {
	var firstErr error
	for _, p := range []string{intentPath(dir), runFile} {
		b, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		if err == nil {
			var in transitionIntent
			if err = json.Unmarshal(b, &in); err == nil && (in.Op == intentPromote || in.Op == intentDemote) {
				return &in, nil
			}
			err = fmt.Errorf("unreadable transition record %s (%v)", p, err)
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

// committed: the transition reached its commit point and only the removal of the record is missing. Only a promotion can be in that
// situation without harm: its commit makes the node active with a newer epoch. A demotion record is never taken as finished, because
// its steps after the commit point (the standby view) are exactly what a restart must redo, and redoing them is harmless.
func (in transitionIntent) committed(st State) bool {
	return in.Op == intentPromote && st.State == StateActive && st.Epoch > in.EpochBefore
}

// recoverTransition runs once at the start of the daemon, before the loops that act on the role.
func (d *Daemon) recoverTransition() {
	in, err := readIntent(d.store.dir, d.runIntentFile())
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
	if d.toSafeStandby("recovery of the interrupted transition", false) {
		d.endIntent()
	} else {
		d.event("error", "recovery of the interrupted transition incomplete: the record is kept and the recovery runs again at the next start")
	}
}

// toSafeStandby brings the node to the state that is always safe, whatever it was: HA services stopped, the standby view of the
// configuration written, the role standby (in memory even if the disk refuses), the data devices down, the standby ruleset. It reports
// whether everything, including the save of the role, succeeded. saveActive: the node may be active with config.xml as the only copy
// of its current configuration, which is saved as active.xml first (as a demotion does).
func (d *Daemon) toSafeStandby(why string, saveActive bool) bool {
	d.ops.mu.Lock()
	defer d.ops.mu.Unlock()
	for _, c := range haStop {
		d.run(c[0], c[1:]...)
	}
	full := activeXML(d.store.dir)
	if saveActive && d.store.Get().State == StateActive {
		if err := copyFile(configPath(), full, 0o600); err != nil {
			d.event("error", "%s: the configuration in use could not be saved (%v)", why, err)
		}
	}
	// the role before the view: a power loss in between must not leave "active" on disk with the view in config.xml (the next boot
	// would copy that view over active.xml)
	saveErr := d.store.UpdateSafe(func(s *State) { s.State = StateStandby })
	if saveErr != nil {
		d.event("error", "%s: the standby role could not be saved (%v)", why, saveErr)
	}
	// the devices down at once (from the saved configuration, or the one in use): the view is generated afterwards
	d.ops.cache, d.ops.mtime = IfaceSet{}, time.Time{}
	d.enforceOnce()
	viewOK := false
	if activeValid(d.store.dir) {
		if err := d.doStandbyView(full); err != nil {
			d.event("error", "%s: the standby view could not be written (%v)", why, err)
		} else {
			viewOK = true
		}
	} else {
		d.event("error", "%s: no valid saved configuration (active.xml); the devices are kept down from the configuration in use, a full resync from the active node is needed", why)
	}
	d.ops.cache, d.ops.mtime = IfaceSet{}, time.Time{}
	d.enforceOnce()
	d.run("configctl", "filter", "reload")
	return saveErr == nil && viewOK
}

// doStandbyView writes the standby view of the configuration (a seam for the tests).
func (d *Daemon) doStandbyView(full string) error {
	if d.act != nil && d.act.standbyView != nil {
		return d.act.standbyView(full)
	}
	return d.writeStandbyView(full)
}
