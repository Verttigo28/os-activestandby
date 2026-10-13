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
func (d *Daemon) demoteHook(reason string, afterDown func()) error {
	d.ops.mu.Lock()
	defer d.ops.mu.Unlock()
	d.ops.bg.Wait() // a DHCP/DNS restart started by the last promote must not run after we went standby
	t0 := time.Now()
	log.Printf("DEMOTE (%s)", reason)
	d.rs.set("") // what a standby holds is decided again: the new active node sends its ruleset
	st := d.store.Get()
	if st.State == StateActive {
		// config.xml is the authoritative full config right now: keep it
		if b, err := os.ReadFile(configPath()); err == nil && st.AckedSHA != "" && shaHex(b) != st.AckedSHA {
			// edits the peer never received: keep them aside, the peer's config will replace this one
			keep := filepath.Join(d.store.dir, fmt.Sprintf("unreplicated-%d.xml", time.Now().Unix()))
			if writeFileDurable(keep, b, 0o600) == nil {
				d.event("warning", "this node had configuration changes the peer never received; saved to %s before demoting", keep)
			}
		}
		if err := copyFile(configPath(), activeXML(d.store.dir), 0o600); err != nil {
			return fmt.Errorf("saving active config: %w", err)
		}
	} else if _, err := os.Stat(activeXML(d.store.dir)); err != nil {
		return fmt.Errorf("no active.xml and node is not active; refusing to derive standby view")
	}
	step(t0, "active config saved")
	set, err := d.ops.ifaces(activeXML(d.store.dir))
	if err != nil {
		return err
	}
	if d.cfg.HADevice != "" && set.HAKey == "" {
		// a restored backup without this plugin's settings: the HA link and management would be cut together with the data
		// interfaces. Refuse; the node stays as it is until the settings are back.
		return errors.New("the saved configuration has no HA settings (was a backup restored without them?): refusing to stand down, which would cut the HA link; restore the Active/Standby settings first")
	}
	// interfaces first (traffic stops immediately), then rewrite the config so nothing re-enables them
	// warm standby: the devices of the eligible interfaces keep their link but lose the shared MAC and the addresses (in that
	// order, see silenceCmds); everything else goes down as in the classic standby
	warm := d.warmSet(activeXML(d.store.dir), set)
	var fw sync.WaitGroup
	for _, dev := range set.ForceDown {
		fw.Add(1)
		go func(dev string) { defer fw.Done(); d.standbyDevice(dev, warm) }(dev)
	}
	fw.Wait()
	step(t0, fmt.Sprintf("%d devices down", len(set.ForceDown)))
	if afterDown != nil {
		afterDown()
	}
	for _, c := range haStop {
		sh(c[0], c[1:]...)
	}
	step(t0, "ipsec/dpinger/openvpn/wireguard stopped")
	if err := d.writeStandbyView(activeXML(d.store.dir)); err != nil {
		return err
	}
	step(t0, "standby view written")
	d.sess.trackExisting(sessionDir)
	d.degraded.Store("")
	d.event("change", "demoted to STANDBY (%s)", reason)
	// the data interfaces are down already: the memory must say standby even if the disk refuses (the failure is returned and logged)
	err = d.store.UpdateSafe(func(s *State) { s.State = StateStandby })
	d.hist.add(HistoryEntry{Type: "demote", Reason: reason, DurationMs: time.Since(t0).Milliseconds(), Epoch: d.store.Get().Epoch})
	d.recordIntegrity()
	return err
}
