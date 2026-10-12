package main

import (
	"os"
	"strings"
	"time"
)

// bootStandby runs from the early syshook, before interfaces are configured: a paired node must never
// come up active on its own, otherwise a rebooting old master would collide with the promoted peer.
func bootStandby() error {
	st, err := openStore()
	if err != nil {
		return err
	}
	s := st.Get()
	if s.Role == RoleStandalone {
		return nil
	}
	d := &Daemon{store: st, ops: &Ops{}}
	full := activeXML(st.dir)
	if _, err := os.Stat(full); err != nil || s.State == StateActive || s.State == "" {
		if err := copyFile(configPath(), full, 0o600); err != nil {
			return err
		}
	}
	if err := d.writeStandbyView(full); err != nil {
		return err
	}
	writeRoleMarker(State{PeerFP: s.PeerFP, State: StateStandby})
	return st.Update(func(x *State) { x.State = StateStandby })
}

// bootGuard runs from the early syshook (detached) while a paired node boots into standby. OPNsense raises
// VLAN/LAGG devices and their member NICs itself during bring-up, and those are not covered by the standby
// view (they are not "enabled interfaces"). The daemon's enforcer only starts later, so this guard polls fast
// until the daemon is running, keeping the window in which a standby device is up to a fraction of a second.
func bootGuard() error {
	st, err := openStore()
	if err != nil {
		return err
	}
	if st.Get().Role == RoleStandalone {
		return nil
	}
	ops := &Ops{}
	deadline := time.Now().Add(10 * time.Minute)
	daemonSeen := 0
	for time.Now().Before(deadline) {
		if _, err := sh("pgrep", "-f", "hasyncd daemon"); err == nil {
			daemonSeen++
			if daemonSeen > 20 { // the daemon has been up for a few seconds: its enforcer takes over
				return nil
			}
		}
		set, err := ops.ifaces(activeXML(st.dir))
		if err != nil {
			set, err = ops.ifaces(configPath()) // the early hook may have failed (disk full, read-only /conf): guard from the live config
		}
		if err == nil { // any paired node boots silent, whatever the state file says
			if up, _ := sh("ifconfig", "-l", "-u"); up != "" {
				upSet := map[string]bool{}
				for _, n := range strings.Fields(up) {
					upSet[n] = true
				}
				for _, dev := range set.ForceDown {
					if upSet[dev] {
						flushDev(dev)
					}
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}
