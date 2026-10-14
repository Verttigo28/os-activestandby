package main

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// bootStandby runs from the early syshook, before interfaces are configured: a paired node must never
// come up active on its own, otherwise a rebooting old master would collide with the promoted peer.
func bootStandby() error { return bootStandbyWith(nil) }

// bootStandbyWith is bootStandby with the seams of the tests (act: the standby view).
func bootStandbyWith(act *actions) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	s := st.Get()
	if s.Role == RoleStandalone {
		return nil
	}
	d := &Daemon{store: st, ops: &Ops{}, act: act}
	full := activeXML(st.dir)
	// config.xml is the full configuration only while the node was active and no demotion had started. A demotion saves active.xml
	// before it changes anything (and leaves its record), then writes the standby view into config.xml: copying config.xml then
	// would replace the full configuration with the view.
	rec, _ := readIntent(st.dir, runIntentPath)
	demoting := rec != nil && rec.Op == intentDemote
	if _, err := os.Stat(full); err != nil || ((s.State == StateActive || s.State == "") && !demoting) {
		if err := copyFile(configPath(), full, 0o600); err != nil {
			return err
		}
	}
	// the role first, then the view: whichever step a power loss interrupts, the next boot copies nothing over active.xml
	if err := st.Update(func(x *State) { x.State = StateStandby }); err != nil {
		return err
	}
	if err := d.doStandbyView(full); err != nil {
		return err
	}
	writeRoleMarker(State{PeerFP: s.PeerFP, State: StateStandby})
	// a transition that was running when the machine went down is over: the node is standby again (intent.go)
	os.Remove(intentPath(st.dir))
	os.Remove(runIntentPath)
	markBootHandled() // the daemon then knows this boot went through the standby path
	return nil
}

// bootMarker records the boot that went through the standby path (bootStandby) or that the daemon already checked. It holds the
// kernel boot time, so a marker of an earlier boot never counts, whether /var/run is cleared at boot or not.
const bootMarker = "/var/run/activestandby.boot"

// bootTime returns the kernel boot time ("" if unknown; a variable for the tests).
var bootTime = func() string {
	out, err := sh("sysctl", "-n", "kern.boottime")
	if err != nil {
		return ""
	}
	// "{ sec = 1791900000, usec = 123456 } Wed Oct 14 ..."
	if i := strings.Index(out, "sec = "); i >= 0 {
		v := out[i+len("sec = "):]
		if j := strings.IndexAny(v, ", }"); j > 0 {
			return v[:j]
		}
	}
	return ""
}

var bootMarkerPath = bootMarker // a variable for the tests

// bootWindow: how long after the boot of the kernel a first start of the daemon counts as the start of that boot.
const bootWindow = 10 * time.Minute

func markBootHandled() {
	if bt := bootTime(); bt != "" {
		if err := fsWrite(bootMarkerPath, []byte(bt), 0o600); err != nil {
			log.Printf("boot marker not written: %v", err)
		}
	}
}

// firstStartOfThisBoot: no marker of this boot. The boot path either did not run or failed, or the daemon has not yet checked.
func firstStartOfThisBoot() (first, known bool) {
	bt := bootTime()
	if bt == "" {
		return false, false // unknown: no conclusion (checkBoot says so)
	}
	// only right after a boot: a daemon (re)started later, by an upgrade of the plugin that predates the marker for example, is not
	// the start of a boot, and an active node must not be taken down for that
	if sec, err := strconv.ParseInt(bt, 10, 64); err != nil || time.Since(time.Unix(sec, 0)) > bootWindow {
		return false, true
	}
	b, err := os.ReadFile(bootMarkerPath)
	return err != nil || strings.TrimSpace(string(b)) != bt, true
}

// checkBoot runs once at the start of the daemon. A paired node that is "active" on disk at the first start of a boot did not go
// through the standby path (bootStandby failed: a full or read-only /conf, a PHP error): a paired node never comes up active on
// its own, so it goes to standby now, before anything acts on its role. Then the boot counts as checked.
func (d *Daemon) checkBoot() {
	st := d.store.Get()
	first, known := firstStartOfThisBoot()
	if !known && st.PeerFP != "" && st.State == StateActive {
		// kern.boottime cannot be read: whether this is the start of a boot cannot be told, and an active node must not be taken down
		// for a restart of its daemon. The protection that remains is the standby path of the boot itself, and the boot guard.
		d.event("warning", "the boot time of this machine cannot be read: the check that a booted node does not come up active on its own was skipped")
	}
	if first && st.PeerFP != "" && st.State == StateActive {
		d.event("error", "this node starts as ACTIVE although it was just booted: the standby path of the boot did not complete. Going to STANDBY; the peer or the election decides who serves")
		d.toSafeStandby("boot without the standby path", true)
	}
	markBootHandled()
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
