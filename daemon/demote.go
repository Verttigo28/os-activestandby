package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// demote: active -> standby. Fast and safe to call repeatedly.
func (d *Daemon) demote(reason string) error { return d.demoteHook(reason, nil) }

// demoteHook is demote with a callback that fires the moment the data interfaces are down (traffic has stopped), so the
// peer can start taking over while this node still tidies up (stops IPsec/dpinger, rewrites its config).
func (d *Daemon) demoteHook(reason string, afterDown func()) (retErr error) {
	d.ops.mu.Lock()
	defer d.ops.mu.Unlock()
	t0 := time.Now()
	log.Printf("DEMOTE (%s)", reason)
	d.standingDown.Store(true) // the background work of the last promotion starts nothing more (promote.go)
	defer d.standingDown.Store(false)
	d.rs.set("") // what a standby holds is decided again: the new active node sends its ruleset
	st := d.store.Get()
	full := activeXML(d.store.dir)
	saved := true // active.xml holds the configuration the standby view is derived from
	if st.State == StateActive {
		// config.xml is the authoritative full config right now: keep it
		if b, err := os.ReadFile(configPath()); err == nil && st.AckedSHA != "" && shaHex(b) != st.AckedSHA {
			// edits the peer never received: keep them aside, the peer's config will replace this one
			keep := filepath.Join(d.store.dir, fmt.Sprintf("unreplicated-%d.xml", time.Now().Unix()))
			if writeFileDurable(keep, b, 0o600) == nil {
				d.event("warning", "this node had configuration changes the peer never received; saved to %s before demoting", keep)
			}
		}
		if err := copyFile(configPath(), full, 0o600); err != nil {
			// /conf is full or failing. Standing down must happen anyway: a split-brain loser that cannot stand down keeps the shared
			// identity on two nodes. The devices go down from the configuration in use; config.xml stays the full configuration
			// (no standby view derived from an older active.xml); the enforcer keeps the devices down.
			d.event("error", "the configuration could not be saved before standing down (%v): standing down anyway, from the configuration in use", err)
			saved = false
		}
	} else if _, err := os.Stat(full); err != nil {
		return fmt.Errorf("no active.xml and node is not active; refusing to derive standby view")
	}
	step(t0, "active config saved")
	setPath := full
	if !saved {
		setPath = configPath()
	}
	set, err := d.ops.ifaces(setPath)
	if err != nil {
		return err
	}
	if d.cfg.HADevice != "" && set.HAKey == "" {
		// a restored backup without this plugin's settings: the HA link and management would be cut together with the data
		// interfaces. Refuse; the node stays as it is until the settings are back.
		return errors.New("the saved configuration has no HA settings (was a backup restored without them?): refusing to stand down, which would cut the HA link; restore the Active/Standby settings first")
	}
	// the configuration is saved; the machine changes from here on (intent.go): a daemon that stops before the end is detected at
	// its restart, and the steps after the devices went down are redone there
	d.beginIntent(intentDemote, reason) // a failure is reported; standing down goes on (it is the safe direction)
	devicesDown := false
	defer func() {
		// Once the data interfaces are down this node does not serve, whatever failed afterwards: its role must say so.
		if retErr != nil && devicesDown && d.store.Get().State != StateStandby {
			d.store.UpdateSafe(func(s *State) { s.State = StateStandby })
			d.event("error", "demotion incomplete (%v): the data interfaces are down and this node counts as STANDBY", retErr)
		}
	}()
	// interfaces first (traffic stops immediately), then rewrite the config so nothing re-enables them
	// warm standby: the devices of the eligible interfaces keep their link but lose the shared MAC and the addresses (in that
	// order, see silenceCmds); everything else goes down as in the classic standby
	warm := d.warmSet(setPath, set)
	var fw sync.WaitGroup
	for _, dev := range set.ForceDown {
		fw.Add(1)
		go func(dev string) { defer fw.Done(); d.standbyDevice(dev, warm) }(dev)
	}
	fw.Wait()
	// "the commands ran" is not "the devices are down": read them back. A device that is still up, or still has an address (a driver that
	// refuses "down", a failed alias removal), is tried once more; if it is still there the peer is NOT released: it would take over
	// while this node can still transmit with the shared identity. The node stays as it is and the failure is the answer (a split-brain
	// loser tries again every few seconds; the enforcer and the next attempt keep working on it).
	if bad := d.standbyDown(set.ForceDown, warm); len(bad) > 0 {
		for _, dev := range bad {
			d.flush(dev)
		}
		if bad = d.standbyDown(set.ForceDown, warm); len(bad) > 0 {
			d.event("error", "standing down failed: %s still up or still carrying an address after being taken down: the peer is not released", strings.Join(bad, ", "))
			return fmt.Errorf("standing down failed: %s did not go down; this node keeps its role (the peer must not take over while it can still transmit with the shared identity)", strings.Join(bad, ", "))
		}
	}
	step(t0, fmt.Sprintf("%d devices down", len(set.ForceDown)))
	devicesDown = true
	// The role, as soon as nothing is served any more, before the slower steps: a power loss after the standby view is written must
	// not find "active" on disk (the next boot would copy that view over active.xml). In memory even if the disk refuses.
	saveErr := d.store.UpdateSafe(func(s *State) { s.State = StateStandby })
	if afterDown != nil {
		afterDown()
	}
	// the services that the last promotion restarts in the background: waited for only now (they check the role, which is standby
	// already), so that they never delay taking the shared identity down
	d.ops.bg.Wait()
	for _, c := range haStop {
		sh(c[0], c[1:]...)
	}
	step(t0, "ipsec/dpinger/openvpn/wireguard stopped")
	if saved {
		if err := d.doStandbyView(full); err != nil {
			return err
		}
		step(t0, "standby view written")
	}
	d.sess.trackExisting(sessionDir)
	d.degraded.Store("")
	d.event("change", "demoted to STANDBY (%s)", reason)
	if saveErr == nil && saved {
		d.endIntent()
	}
	d.hist.add(HistoryEntry{Type: "demote", Reason: reason, DurationMs: time.Since(t0).Milliseconds(), Epoch: d.store.Get().Epoch})
	d.recordIntegrity()
	return saveErr
}
