package main

import (
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// promote: standby -> active.
func (d *Daemon) promote(reason string) (retErr error) {
	d.ops.mu.Lock()
	defer d.ops.mu.Unlock()
	t0 := time.Now()
	ph := &phaseLog{t0: t0}
	log.Printf("PROMOTE (%s)", reason)
	d.rs.set("")            // a node that becomes active holds its own ruleset, not a mirror
	d.blockPackageInstall() // no plugin installation runs next to the takeover
	defer d.unblockPackageInstall()
	if d.store.Get().State == StateActive {
		return nil
	}
	d.degraded.Store("")
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
	// The first change of the machine follows: from here on a failure must not leave the full config (shared IPs/MACs) on a node
	// that stays standby, and a daemon that stops in the middle must be detected at its restart (intent.go).
	if err := d.beginIntent(intentPromote, reason); err != nil {
		return fmt.Errorf("promotion refused, it could not be recorded (a crash in the middle would not be recognised): %w", err)
	}
	earlyStarted := false // the full ruleset is loaded before the interfaces are up (below)
	// the two things that run in the background of the interface bring-up; the cleanup below waits for both, otherwise it
	// would rewrite the configuration and reload the packet filter while they are still generating from the full one
	var earlyDone, routesDone chan struct{}
	var earlyErr error
	defer func() {
		if retErr != nil && d.store.Get().State != StateActive {
			if earlyDone != nil {
				<-earlyDone
			}
			if routesDone != nil {
				<-routesDone
			}
			log.Printf("promote failed (%v): back to the standby view", retErr)
			viewErr := d.writeStandbyView(full)
			if viewErr != nil {
				log.Printf("promote cleanup: %v", viewErr)
			}
			d.ops.cache, d.ops.mtime = IfaceSet{}, time.Time{}
			d.enforceOnce()
			if earlyStarted {
				sh("configctl", "filter", "reload") // the ruleset of the standby view again
			}
			if viewErr == nil { // back where it started; otherwise the record makes a restart redo the standby view
				d.endIntent()
			}
		}
	}()
	unlockConfig()
	if err := copyFile(full, configPath(), 0o640); err != nil {
		return err
	}
	ph.mark("full config restored")
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
	ph.mark("virtual devices ready")
	// The ruleset is generated (about 0.7 s of PHP) while the interfaces come up, instead of after them: new connections only
	// pass once pf has the ruleset of the active node, established ones already pass on the states pfsync replicated. The
	// authoritative reload below still runs after routes, gateways and addresses exist (a rule that depends on them is then
	// regenerated); until then this is the same ruleset the node will keep, loaded a second or two earlier.
	if d.cfg.fastOn() {
		earlyDone = make(chan struct{})
		earlyStarted = true
		go func() {
			defer close(earlyDone)
			earlyErr = reloadFilter()
		}()
	}
	d.ops.cache, d.ops.mtime = IfaceSet{}, time.Time{}
	var firstErr error
	// warm standby: the eligible interfaces take the shared identity natively (MAC, addresses); the others, and everything
	// if this fails or the standby was not ready, go through the classic path below
	warmDone := map[string]bool{}
	if p, perr := d.warmPlanFor(full); perr != nil {
		log.Printf("promote: warm plan: %v", perr)
	} else if p != nil {
		if done, werr := d.warmActivate(p, set); werr != nil {
			d.event("warning", "warm takeover not possible (%v): using the classic takeover", werr)
		} else {
			warmDone = done
			ph.mark(fmt.Sprintf("shared identity taken natively on %d interface(s) (warm standby)", len(done)))
		}
	}
	// each interface costs a fraction of a second of mostly waiting; independent ones are configured at the same time,
	// each in its own php process, tier by tier so parents are up before what sits on them (LAGG, then VLAN, then the rest)
	tiers := make([][]Iface, 4)
	var enabled []Iface
	for _, i := range set.Data {
		if !i.Enabled {
			continue // assigned but disabled in the config: stays down on the active node too
		}
		enabled = append(enabled, i)
		if warmDone[i.Key] {
			continue // already configured natively
		}
		tiers[devTier(i)] = append(tiers[devTier(i)], i)
	}
	for n, group := range tiers {
		if len(group) == 0 {
			continue
		}
		if total := len(group); n >= 2 { // the VLANs and what sits on them: the stock code is skipped for those the plan marks "fast" (fastpath.go)
			if group = d.fastTier(full, group); len(group) < total {
				ph.mark(fmt.Sprintf("tier %d: %d interface(s) brought up natively", n, total-len(group)))
			}
			if len(group) == 0 {
				continue
			}
		}
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
		ph.mark(fmt.Sprintf("tier %d: %d interface(s) configured", n, len(group)))
	}
	// routes and gateway monitoring (0.3-0.8 s) need the addresses only: they run while the links settle and the switch is told
	routesDone = make(chan struct{})
	go func() {
		defer close(routesDone)
		if out, err := sh(reconfScript, "routes"); err != nil {
			log.Printf("promote: routes: %v: %s", err, out)
			d.markDegraded("static routes could not be restored (%v): %s", err, out)
		}
	}()
	ph.mark("interfaces reconfigured")
	if firstErr != nil {
		<-routesDone
		return firstErr
	}
	// some virtual NIC drivers keep their hardware receive filter when the lladdr changes; a VLAN with a MAC of its own
	// is not received at all on a real NIC without promiscuous mode (vlanmac.go)
	for _, i := range enabled {
		if d.cfg.ForcePromisc || vlanNeedsPromisc(i.Dev) {
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
	ph.mark("links up")
	// gratuitous ARP first, right after the links are up so the switch and neighbours relearn the port
	for _, i := range enabled {
		announce(i.Dev)
	}
	ph.mark("gratuitous ARP sent")
	if earlyDone != nil {
		<-earlyDone
		if err := earlyErr; err != nil {
			log.Printf("promote: early ruleset not loaded (%v), the reload after the interfaces decides", err)
		} else {
			ph.mark("packet filter loaded (early)")
		}
	}
	<-routesDone
	ph.mark("routes and gateway monitoring ready")
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
	var pfErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		pfErr = reloadFilter()
	}()
	for _, c := range haStart {
		run(c...)
	}
	wg.Wait()
	// "the command ran" is not "the firewall is working": the interfaces are up and carry traffic by now, so a node whose
	// ruleset could not be loaded must not stay in this state. Returning the error puts the standby view back and the
	// interfaces down (deferred cleanup above), and the node reports the failure instead of a false ACTIVE.
	if pfErr != nil {
		for _, c := range haStop { // what the parallel start brought up must not keep running on a node that stays standby
			sh(c[0], c[1:]...)
		}
		d.event("error", "promotion aborted, the packet filter is not working: %v", pfErr)
		return fmt.Errorf("packet filter: %w", pfErr)
	}
	// DHCP and DNS restart once (not once per interface) and re-read the lease files replicated from the old active node.
	// Nothing waits for it (traffic already flows), but a demote does, so it cannot restart them after the node went standby.
	d.ops.bg.Add(1)
	go func() {
		defer d.ops.bg.Done()
		t1 := time.Now()
		out, err := sh(reconfScript, "dns")
		log.Printf("  dhcp+dns restart: %dms err=%v out=%q", time.Since(t1).Milliseconds(), err, out)
		var bad []string
		if err != nil {
			bad = append(bad, "DHCP and DNS restart")
		}
		// a demotion that started meanwhile has taken the identity down: nothing more is started on a node that stands down
		if !d.standingDown.Load() {
			bad = append(bad, restartBoundServices()...)
		}
		if !d.standingDown.Load() {
			_, failed := d.startStoppedServices()
			bad = append(bad, failed...)
		}
		// a node that is already standby again (a quick hand-back) has nothing to report
		if len(bad) > 0 && d.store.Get().State == StateActive {
			d.markDegraded("services that did not start after the takeover: %s", strings.Join(bad, ", "))
		}
	}()
	ph.mark("pf reloaded, services started")
	d.event("change", "promoted to ACTIVE (%s) in %d ms", reason, time.Since(t0).Milliseconds())
	d.commitActive()
	d.hist.add(HistoryEntry{Type: "promote", Reason: reason, DurationMs: time.Since(t0).Milliseconds(), Epoch: d.store.Get().Epoch, Phases: ph.list(), DetectMs: d.auto.detectMs.Swap(0)})
	d.leases.reset()
	d.drift.reason.Store("")
	d.health.startGrace()
	ph.mark("ACTIVE")
	return nil
}

// commitActive is the commit point of a takeover. The interfaces, the ruleset and the services are up already, so this node IS
// serving: the memory must say so even if the disk refuses (UpdateSafe publishes it). The failure is then loud: the node is marked
// degraded, the transition record stays, and a restart (of the daemon or of the machine) brings it back to standby: the safe direction. Returning an
// error here instead would tell the peer that the promotion failed while this node forwards traffic.
func (d *Daemon) commitActive() {
	err := d.store.UpdateSafe(func(s *State) {
		_, _, pe := d.hb.Snapshot()
		// the highest generation known: the peer's heartbeat, and the highest epoch of the leader whose pushes this node accepted (the
		// heartbeat is empty after a restart of this daemon while the peer is silent)
		if s.LeaderEpoch > s.Epoch {
			s.Epoch = s.LeaderEpoch
		}
		if pe.Epoch > s.Epoch {
			s.Epoch = pe.Epoch
		}
		s.State = StateActive
		s.Epoch++
		s.AckedSHA = "" // the new active node pushes its full config to the new standby, so both sides converge
	})
	if err != nil {
		// the transition record stays: a restart of the daemon (or a reboot) brings this node back to standby, because the takeover
		// is not durable
		d.markDegraded("the new role could not be saved to disk (%v): a restart of the daemon or a reboot brings this node back to standby", err)
		return
	}
	d.endIntent()
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

// restartBoundServices returns the services whose restart failed.
func restartBoundServices() (failed []string) {
	svc, _ := sh("pluginctl", "-S")
	for name, cmd := range boundServices {
		if strings.Contains(svc, `"name": "`+name+`"`) {
			if out, err := sh(cmd[0], cmd[1:]...); err != nil {
				log.Printf("  restart %s after takeover: %v: %s", name, err, out)
				failed = append(failed, name)
			}
		}
	}
	sort.Strings(failed)
	return failed
}
