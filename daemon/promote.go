package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// promote: standby -> active.
func (d *Daemon) promote(reason string) (retErr error) {
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
		os.Chmod(full, 0o600)
		syncDir(d.store.dir)
	} else {
		log.Printf("promote: could not refresh local parts (%v: %s), using active.xml as is", err, o)
	}
	unlockConfig()
	if err := copyFile(full, configPath(), 0o640); err != nil {
		return err
	}
	// from here on a failure must not leave the full config (shared IPs/MACs) on a node that stays standby
	defer func() {
		if retErr != nil && d.store.Get().State != StateActive {
			log.Printf("promote failed (%v): back to the standby view", retErr)
			if err := d.writeStandbyView(full); err != nil {
				log.Printf("promote cleanup: %v", err)
			}
			d.ops.cache, d.ops.mtime = IfaceSet{}, time.Time{}
			d.enforceOnce()
		}
	}()
	step(t0, "full config restored")
	set, err := loadIfaces(full)
	if err != nil {
		return err
	}
	// virtual devices (VLAN, LAGG, ...): they survive on a standby, so the slow device script only runs when the device
	// definitions changed since it last ran here (or a device is missing)
	if len(set.DeviceTypes) > 0 {
		h := devSectionsHash(full)
		if h == "" || h != d.ops.devHash || !virtualDevicesPresent(set) {
			if out, err := sh(devicesScript); err != nil {
				log.Printf("promote: creating virtual devices: %v: %s", err, out)
				d.event("error", "creating the virtual devices (VLAN/LAGG/...) failed: %s", strings.TrimSpace(out))
			} else {
				d.ops.devHash = h
			}
		}
	}
	// the standby flushed VLAN parents and LAGG members down too: bring every non-data device back up first
	dataDev := map[string]bool{}
	for _, i := range set.Data {
		dataDev[i.Dev] = true
	}
	for _, dev := range set.ForceDown {
		if !dataDev[dev] {
			sh("ifconfig", dev, "up")
		}
	}
	step(t0, "virtual devices ready")
	d.ops.cache, d.ops.mtime = IfaceSet{}, time.Time{}
	var firstErr error
	// each interface costs a fraction of a second of mostly waiting; independent ones are configured at the same time,
	// each in its own php process, tier by tier so parents are up before what sits on them (LAGG, then VLAN, then the rest)
	tiers := make([][]Iface, 4)
	var enabled []Iface
	for _, i := range set.Data {
		if !i.Enabled {
			continue // assigned but disabled in the config: stays down on the active node too
		}
		enabled = append(enabled, i)
		tiers[devTier(i)] = append(tiers[devTier(i)], i)
	}
	for _, group := range tiers {
		var rw sync.WaitGroup
		var emu sync.Mutex
		for _, i := range group {
			rw.Add(1)
			go func(key string) {
				defer rw.Done()
				if out, err := sh(reconfScript, "iface", key); err != nil {
					log.Printf("promote: fast reconfigure of %s failed (%v: %s), using the stock action", key, err, out)
					if out, err := sh("configctl", "interface", "reconfigure", key); err != nil {
						emu.Lock()
						if firstErr == nil {
							firstErr = fmt.Errorf("reconfigure %s: %v: %s", key, err, out)
						}
						emu.Unlock()
					}
				}
			}(i.Key)
		}
		rw.Wait()
	}
	if out, err := sh(reconfScript, "routes"); err != nil {
		log.Printf("promote: routes: %v: %s", err, out)
	}
	step(t0, "interfaces reconfigured")
	if firstErr != nil {
		return firstErr
	}
	if d.cfg.ForcePromisc {
		// some virtual NIC drivers keep their hardware receive filter when the lladdr changes
		for _, i := range enabled {
			sh("ifconfig", i.Dev, "promisc")
		}
	}
	// a config without <enable> "reconfigures" fine but leaves the device down: verify, never assume. Devices created
	// later by a daemon (PPPoE & co.) are exempt.
	up, _ := sh("ifconfig", "-l", "-u")
	upSet := map[string]bool{}
	for _, n := range strings.Fields(up) {
		upSet[n] = true
	}
	for _, i := range enabled {
		if !upSet[i.Dev] && !asyncDev.MatchString(i.Dev) {
			return fmt.Errorf("interface %s (%s) did not come up after reconfigure; is it enabled in the config?", i.Key, i.Dev)
		}
	}
	waitCarrier(enabled, 8*time.Second)
	step(t0, "links up")
	// gratuitous ARP first, right after the links are up so the switch and neighbours relearn the port
	for _, i := range enabled {
		announce(i.Dev)
	}
	step(t0, "gratuitous ARP sent")
	// pf and the services do not depend on each other: run them side by side
	var wg sync.WaitGroup
	run := func(c ...string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t1 := time.Now()
			out, err := sh(c[0], c[1:]...)
			log.Printf("  %v: %dms err=%v out=%q", c, time.Since(t1).Milliseconds(), err, out)
		}()
	}
	run("configctl", "filter", "reload")
	for _, c := range haStart {
		run(c...)
	}
	wg.Wait()
	// DHCP and DNS restart once (not once per interface) and re-read the lease files replicated from the old active node.
	// Nothing waits for it (traffic already flows), but a demote does, so it cannot restart them after the node went standby.
	d.ops.bg.Add(1)
	go func() {
		defer d.ops.bg.Done()
		t1 := time.Now()
		out, err := sh(reconfScript, "dns")
		log.Printf("  dhcp+dns restart: %dms err=%v out=%q", time.Since(t1).Milliseconds(), err, out)
		restartBoundServices()
	}()
	step(t0, "pf reloaded, services started")
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
	d.hist.add(HistoryEntry{Type: "promote", Reason: reason, DurationMs: time.Since(t0).Milliseconds(), Epoch: d.store.Get().Epoch})
	d.leases.reset()
	d.drift.reason.Store("")
	d.health.startGrace()
	step(t0, "ACTIVE")
	return err
}

// boundServices are services that bind to interface addresses and do not notice that the addresses came back. When one
// of them is installed and enabled (it shows up as a service), it is restarted after a takeover. A missing action is
// harmless: the result is ignored.
var boundServices = map[string][]string{
	"ntpd":          {"configctl", "ntpd", "restart"},
	"captiveportal": {"configctl", "captiveportal", "restart"},
	"suricata":      {"configctl", "ids", "restart"},
	"haproxy":       {"configctl", "haproxy", "restart"},
	"ddclient":      {"configctl", "ddclient", "restart"},
}

func restartBoundServices() {
	svc, _ := sh("pluginctl", "-S")
	for name, cmd := range boundServices {
		if strings.Contains(svc, `"name": "`+name+`"`) {
			if out, err := sh(cmd[0], cmd[1:]...); err != nil {
				log.Printf("  restart %s after takeover: %v: %s", name, err, out)
			}
		}
	}
}
