package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Operational state, separate from the pairing role:
//
//	StateActive  - config.xml is the full config, data interfaces are up, services run.
//	StateStandby - config.xml is the "standby view" (no <enable> on data interfaces), interfaces are
//	               down and without addresses, IPsec/dpinger stopped. The full config is kept in active.xml.
//
// The ACTIVE node is the configuration source (edit it, it replicates to the standby).
const (
	StateActive  = "active"
	StateStandby = "standby"

	ifacesScript  = "/usr/local/opnsense/scripts/OPNsense/ActiveStandby/ifaces.php"
	configXML     = "/conf/config.xml"
	devicesScript = "/usr/local/opnsense/scripts/OPNsense/ActiveStandby/create_devices.php"
)

// Services that must only run on the active node. Stopped on demote, (re)started on promote.
// The configd actions are used because the pluginctl service names differ (strongswan, not ipsec) and dpinger
// is only a service while gateways exist; `pluginctl -c monitor` is the hook that (re)starts gateway monitoring.
var (
	haStop = [][]string{
		{"configctl", "ipsec", "stop"},
		{"configctl", "openvpn", "stop"},
		{"configctl", "wireguard", "stop"},
		{"pkill", "-x", "dpinger"},
	}
	haStart = [][]string{
		{"configctl", "ipsec", "start"},
		{"pluginctl", "-c", "monitor"},
		{"configctl", "openvpn", "start"},
		{"configctl", "wireguard", "start"},
	}
)

type Iface struct {
	Key string `json:"key"`
	Dev string `json:"dev"`
}

type IfaceSet struct {
	Data        []Iface  `json:"data"`
	ForceDown   []string `json:"force_down"`
	KeepUp      []string `json:"keep_up"`
	DeviceTypes []string `json:"device_types"`
}

type Ops struct {
	mu    sync.Mutex // serialises promote/demote
	cache IfaceSet
	mtime time.Time
}

func configPath() string {
	if p := os.Getenv("HASYNC_CONFIG"); p != "" {
		return p
	}
	return configXML
}

func activeXML(dir string) string { return filepath.Join(dir, "active.xml") }

func loadIfaces(path string) (IfaceSet, error) {
	var s IfaceSet
	out, err := exec.Command(ifacesScript, "list", path).Output()
	if err != nil {
		return s, fmt.Errorf("ifaces.php list: %w", err)
	}
	return s, json.Unmarshal(out, &s)
}

// ifaces returns the interface set, re-reading only when the config on disk changed.
func (o *Ops) ifaces(path string) (IfaceSet, error) {
	fi, err := os.Stat(path)
	if err == nil && fi.ModTime().Equal(o.mtime) && len(o.cache.ForceDown) > 0 {
		return o.cache, nil
	}
	s, err := loadIfaces(path)
	if err == nil && fi != nil {
		o.cache, o.mtime = s, fi.ModTime()
	}
	return s, err
}

func sh(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func step(t0 time.Time, what string) { log.Printf("  +%4dms %s", time.Since(t0).Milliseconds(), what) }

func copyFile(src, dst string, perm os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFileDurable(dst, b, perm)
}

// writeStandbyView derives the standby config.xml from the full config.
func (d *Daemon) writeStandbyView(full string) error {
	unlockConfig()
	defer lockConfig()
	tmp := configPath() + ".sbv"
	if out, err := sh(ifacesScript, "stdview", full, tmp); err != nil {
		return fmt.Errorf("stdview: %v: %s", err, out)
	}
	os.Chmod(tmp, 0o640)
	syncFile(tmp)
	if err := os.Rename(tmp, configPath()); err != nil {
		return err
	}
	return syncDir(filepath.Dir(configPath()))
}

// flushDev removes every address from the device and sets it down.
func flushDev(dev string) {
	out, _ := sh("ifconfig", dev)
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && f[0] == "inet" {
			sh("ifconfig", dev, "inet", f[1], "-alias")
		} else if len(f) >= 2 && f[0] == "inet6" && !strings.HasPrefix(f[1], "fe80") {
			sh("ifconfig", dev, "inet6", f[1], "-alias")
		}
	}
	sh("ifconfig", dev, "inet6", "-ifdisabled") // harmless if unsupported
	sh("ifconfig", dev, "-promisc")
	sh("ifconfig", dev, "down")
}

// demote: active -> standby. Fast and safe to call repeatedly.
func (d *Daemon) demote(reason string) error {
	d.ops.mu.Lock()
	defer d.ops.mu.Unlock()
	t0 := time.Now()
	log.Printf("DEMOTE (%s)", reason)
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
	for _, c := range haStop {
		sh(c[0], c[1:]...)
	}
	step(t0, "ipsec/dpinger/openvpn/wireguard stopped")
	set, err := d.ops.ifaces(activeXML(d.store.dir))
	if err != nil {
		return err
	}
	// interfaces first (traffic stops immediately), then rewrite the config so nothing re-enables them
	for _, dev := range set.ForceDown {
		flushDev(dev)
	}
	step(t0, fmt.Sprintf("%d devices down", len(set.ForceDown)))
	if err := d.writeStandbyView(activeXML(d.store.dir)); err != nil {
		return err
	}
	step(t0, "standby view written")
	d.event("change", "demoted to STANDBY (%s)", reason)
	return d.store.Update(func(s *State) { s.State = StateStandby })
}

// promote: standby -> active.
func (d *Daemon) promote(reason string) error {
	d.ops.mu.Lock()
	defer d.ops.mu.Unlock()
	t0 := time.Now()
	log.Printf("PROMOTE (%s)", reason)
	if d.store.Get().State == StateActive {
		return nil
	}
	full := activeXML(d.store.dir)
	if !activeValid(d.store.dir) {
		return fmt.Errorf("active.xml is missing or damaged, cannot promote (waiting for a full resync from the active node)")
	}
	// local settings edited on this standby since the last sync live in config.xml only: carry them over
	if o, err := sh(mergeScript, full, configPath(), full+".new"); err == nil {
		syncFile(full + ".new")
		os.Rename(full+".new", full)
		syncDir(d.store.dir)
	} else {
		log.Printf("promote: could not refresh local parts (%v: %s), using active.xml as is", err, o)
	}
	unlockConfig()
	if err := copyFile(full, configPath(), 0o640); err != nil {
		return err
	}
	step(t0, "full config restored")
	set, err := loadIfaces(full)
	if err != nil {
		return err
	}
	// virtual devices (VLAN, LAGG, ...) exist only if something created them: do it before bringing interfaces up
	if len(set.DeviceTypes) > 0 {
		if out, err := sh(devicesScript); err != nil {
			log.Printf("promote: creating virtual devices: %v: %s", err, out)
		}
	}
	step(t0, "virtual devices created")
	d.ops.cache, d.ops.mtime = IfaceSet{}, time.Time{}
	var firstErr error
	for _, i := range set.Data {
		if out, err := sh("configctl", "interface", "reconfigure", i.Key); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("reconfigure %s: %v: %s", i.Key, err, out)
		}
	}
	step(t0, "interfaces reconfigured")
	if firstErr != nil {
		return firstErr
	}
	if d.cfg.ForcePromisc {
		// some virtual NIC drivers keep their hardware receive filter when the lladdr changes
		for _, i := range set.Data {
			sh("ifconfig", i.Dev, "promisc")
		}
	}
	// a config without <enable> "reconfigures" fine but leaves the device down: verify, never assume
	up, _ := sh("ifconfig", "-l", "-u")
	upSet := map[string]bool{}
	for _, n := range strings.Fields(up) {
		upSet[n] = true
	}
	for _, i := range set.Data {
		if !upSet[i.Dev] {
			return fmt.Errorf("interface %s (%s) did not come up after reconfigure; is it enabled in the config?", i.Key, i.Dev)
		}
	}
	sh("configctl", "filter", "reload")
	step(t0, "pf reloaded")
	// gratuitous ARP right after the link is up so the switch and neighbours relearn the port
	for _, i := range set.Data {
		announce(i.Dev)
	}
	step(t0, "gratuitous ARP sent")
	var wg sync.WaitGroup
	for _, c := range haStart {
		wg.Add(1)
		go func(c []string) { defer wg.Done(); sh(c[0], c[1:]...) }(c)
	}
	wg.Wait()
	step(t0, "services started")
	d.event("change", "promoted to ACTIVE (%s) in %d ms", reason, time.Since(t0).Milliseconds())
	err = d.store.Update(func(s *State) {
		_, _, pe := d.hb.Snapshot()
		if pe.Epoch > s.Epoch {
			s.Epoch = pe.Epoch
		}
		s.State = StateActive
		s.Epoch++
		s.AckedSHA = "" // the new active node pushes its full config to the new standby, so both sides converge
	})
	step(t0, "ACTIVE")
	return err
}

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
	return st.Update(func(x *State) { x.State = StateStandby })
}

// enforce keeps a standby silent: any data device that is up gets flushed and downed again.
func (d *Daemon) enforceLoop() {
	tick := 0
	for range time.Tick(time.Second) {
		tick++
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

// activeValid reports whether active.xml looks like a complete OPNsense config (not empty/torn).
func activeValid(dir string) bool {
	b, err := os.ReadFile(activeXML(dir))
	return err == nil && len(b) > 200 && strings.Contains(string(b[:min(len(b), 4096)]), "<opnsense") &&
		strings.Contains(string(b[max(0, len(b)-64):]), "</opnsense>")
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

// ---- standby config lock ----
// On a standby /conf/config.xml carries the immutable flag (schg). A change made through the GUI or API of a
// standby then never reaches disk (OPNsense falls back to a read-only handle), so nobody can plant a user, rule or
// SSH key that a failover would activate. Only this daemon lifts the flag, for its own writes. The active node's
// config is never locked.

const sfImmutable = 0x20000 // SF_IMMUTABLE (schg)

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
func lockConfig()   { sh("chflags", "schg", configPath()) }

// ensureConfigLock enforces the lock state for the current role (called at start and by the enforcer).
func (d *Daemon) ensureConfigLock() {
	st := d.store.Get()
	locked := isImmutable(configPath())
	switch {
	case st.State == StateStandby && !locked && time.Now().After(d.unlockedUntil):
		lockConfig()
	case st.State != StateStandby && locked:
		unlockConfig()
	}
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
		if err == nil && st.Get().State == StateStandby {
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
