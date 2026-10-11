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
		writeRoleMarker(d.store.Get())
		d.ensureConfigLock()
		if d.store.Get().State != StateStandby {
			continue
		}
		// never interfere with a promote/demote in progress (they hold the lock)
		if !d.ops.mu.TryLock() {
			continue
		}
		d.enforceOnce()
		if tick%5 == 0 {
			d.enforceServices()
		}
		if tick%10 == 3 {
			d.prepareDevices()
		}
		d.ops.mu.Unlock()
	}
}

func (d *Daemon) enforceOnce() {
	{
		if d.store.Get().State != StateStandby {
			return
		}
		set, err := d.ops.ifaces(activeXML(d.store.dir))
		if err != nil {
			return
		}
		up, _ := sh("ifconfig", "-l", "-u")
		upSet := map[string]bool{}
		for _, n := range strings.Fields(up) {
			upSet[n] = true
		}
		for _, dev := range set.ForceDown {
			if upSet[dev] {
				log.Printf("enforcer: %s was up on standby, forcing down", dev)
				flushDev(dev)
			}
		}
	}
}

// enforceServices: IPsec, gateway monitoring and VPN daemons must not run on a standby. OPNsense starts them
// at boot from the (replicated) config, so check periodically and stop anything that slipped in.
func (d *Daemon) enforceServices() {
	var running []string
	for _, p := range []string{"charon", "dpinger", "openvpn"} {
		if _, err := sh("pgrep", "-x", p); err == nil {
			running = append(running, p)
		}
	}
	if len(running) == 0 {
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
