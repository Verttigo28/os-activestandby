package main

import (
	"fmt"
	"log"
	"strings"
	"time"
)

// enforce keeps a standby silent: any data device that is up gets flushed and downed again.
func (d *Daemon) enforceLoop() {
	tick := 0
	for range time.Tick(time.Second) {
		tick++
		if tick%60 == 1 {
			hardenFiles(d.store.dir)
		}
		d.markerStep()
		d.ensureConfigLock()
		if d.store.Get().State == StateActive && d.warmOn() && tick%30 == 2 && d.ops.mu.TryLock() {
			// an active node carries the shared MAC: remember its permanent one while that is still visible
			if set, err := d.ops.ifaces(configPath()); err == nil {
				d.warmLearn(configPath(), set)
			}
			d.ops.mu.Unlock()
		}
		if d.store.Get().State != StateStandby {
			continue
		}
		// never interfere with a promote/demote in progress (they hold the lock)
		if !d.ops.mu.TryLock() {
			continue
		}
		d.enforceOnce()
		if tick%10 == 3 {
			d.prepareDevices()
		}
		d.ops.mu.Unlock()
		if tick%5 == 0 {
			d.enforceServices() // outside the lock, see enforceServices
		}
	}
}

func (d *Daemon) enforceOnce() {
	{
		if d.store.Get().State != StateStandby {
			return
		}
		set, err := d.ops.ifaces(activeXML(d.store.dir))
		if err != nil {
			// A standby that cannot read its saved configuration must still keep its data devices silent: the standby view in
			// config.xml lists the same devices (the boot guard falls back to it the same way). Only if that fails too is nothing enforced,
			// and that is reported, not hidden.
			var err2 error
			if set, err2 = d.ops.ifaces(configPath()); err2 != nil {
				if time.Since(d.ops.enforceErrAt) > 10*time.Minute {
					d.ops.enforceErrAt = time.Now()
					d.event("error", "the standby cannot read its interface list (%v; and %v): its data devices are NOT being kept down", err, err2)
				}
				return
			}
			if time.Since(d.ops.enforceErrAt) > 10*time.Minute {
				d.ops.enforceErrAt = time.Now()
				d.event("warning", "the standby cannot read its saved configuration (%v): keeping its devices down from the configuration in use", err)
			}
		}
		warm := d.warmSet(activeXML(d.store.dir), set)
		up, _ := d.run("ifconfig", "-l", "-u")
		upSet := map[string]bool{}
		for _, n := range strings.Fields(up) {
			upSet[n] = true
		}
		for _, dev := range set.ForceDown {
			if _, isWarm := warm[dev]; isWarm {
				continue // kept up and silent by warmReadiness below
			}
			if upSet[dev] {
				log.Printf("enforcer: %s was up on standby, forcing down", dev)
				d.flush(dev)
			}
		}
		d.warmReadiness(activeXML(d.store.dir), set)
	}
}

// enforceServices: IPsec, gateway monitoring and VPN daemons must not run on a standby. OPNsense starts them
// at boot from the (replicated) config, so check periodically and stop anything that slipped in.
// standbyServicesRunning lists the HA services that run although this node is standby (the boot sequence starts them). Fast
// commands only, run without the operations lock.
func standbyServicesRunning() []string {
	var running []string
	for _, p := range []string{"charon", "dpinger", "openvpn"} {
		if _, err := sh("pgrep", "-x", p); err == nil {
			running = append(running, p)
		}
	}
	if out, err := sh("wg", "show", "interfaces"); err == nil && strings.TrimSpace(out) != "" {
		running = append(running, "wireguard") // a standby must not complete handshakes with the peers of the shared key
	}
	return running
}

// enforceServices stops them. The check runs without the operations lock (an automatic takeover must not wait for it); the stop
// takes the lock and checks again that the node is still standby, so that it never stops what a promotion has just started.
func (d *Daemon) enforceServices() {
	running := standbyServicesRunning()
	if len(running) == 0 || !d.ops.mu.TryLock() {
		return
	}
	defer d.ops.mu.Unlock()
	if d.store.Get().State != StateStandby {
		return
	}
	for _, c := range haStop {
		sh(c[0], c[1:]...)
	}
	d.event("warning", "stopped %s on the standby (started by the boot sequence)", strings.Join(running, ", "))
}

const sfImmutable = 0x20000

func isImmutable(path string) bool {
	out, err := sh("stat", "-f", "%f", path)
	if err != nil {
		return false
	}
	var fl uint64
	fmt.Sscan(out, &fl)
	return fl&sfImmutable != 0
}

func unlockConfig() { sh("chflags", "noschg", configPath()) }

func lockConfig() { sh("chflags", "schg", configPath()) }

// ensureConfigLock enforces the lock state for the current role (called at start and by the enforcer).
func (d *Daemon) ensureConfigLock() {
	st := d.store.Get()
	locked := isImmutable(configPath())
	switch {
	case st.State == StateStandby && !locked && time.Now().After(d.unlockedUntil.Get()):
		wasWindow := !d.unlockedUntil.Get().IsZero()
		lockConfig()
		if wasWindow { // the operator's unlock window ended: whatever was changed in it is accepted as the new baseline
			d.unlockedUntil.Set(time.Time{})
			d.recordIntegrity()
		}
	case st.State != StateStandby && locked:
		unlockConfig()
	}
}
