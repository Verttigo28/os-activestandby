package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
		if err := d.writeStandbyView(full); err != nil {
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
