package main

import (
	"net"
	"strings"
	"sync"
	"time"
)

// Health of the ACTIVE node. Both nodes keep publishing the carrier state of their data devices in the heartbeat; the
// active node additionally checks what the operator asked for:
//   * carrier of the monitored interfaces (a lost cable/port on the active node),
//   * critical services (packet filter, IPsec) with a restart attempt before anything else.
// When a problem persists for the hold time the roles are switched, but only when the standby can take over:
// for a link problem the standby must report link on the same port, otherwise the outage would just move. Anti ping-pong:
// at most two health switches in 30 minutes and a 5 minute cool-down.

type healthState struct {
	mu            sync.Mutex
	failSince     map[string]time.Time
	reasons       []string
	cooldown      time.Time
	switches      []time.Time
	lastRestart   map[string]time.Time
	warned        map[string]bool
	links         map[string]bool // carrier of this node's data devices whose state can be read, sent in every heartbeat
	linksUnknown  []string        // data devices whose state cannot be read (administratively down and no carrier: a standby port)
	graceUntil    time.Time       // no health switching right after a promote (DHCP/PPPoE WAN, gateways, services still coming up)
	devMu         sync.Mutex
	devPath       string
	devMtime      time.Time
	devSet        IfaceSet
	mgmtBad       int    // consecutive failed management probes
	mgmtMsg       string // last failure message
	lastMgmtProbe time.Time
}

func newHealthState() *healthState {
	return &healthState{failSince: map[string]time.Time{}, lastRestart: map[string]time.Time{}, warned: map[string]bool{}, links: map[string]bool{}}
}

func (h *healthState) ifaces(path string) (IfaceSet, error) {
	h.devMu.Lock()
	defer h.devMu.Unlock()
	if fi, err := osStat(path); err == nil && path == h.devPath && fi.Equal(h.devMtime) && len(h.devSet.Data) > 0 {
		return h.devSet, nil
	}
	s, err := loadIfaces(path)
	if err == nil {
		h.devPath = path
		if fi, e := osStat(path); e == nil {
			h.devMtime = fi
		}
		h.devSet = s
	}
	return s, err
}

func (h *healthState) snapshotUnknown() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.linksUnknown...)
}

func (h *healthState) snapshotLinks() map[string]bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]bool, len(h.links))
	for k, v := range h.links {
		out[k] = v
	}
	return out
}

func (d *Daemon) healthFailing() bool {
	d.health.mu.Lock()
	defer d.health.mu.Unlock()
	return len(d.health.reasons) > 0
}

// healthReason is what the heartbeat tells the peer ("" = healthy).
func (d *Daemon) healthReason() string {
	d.health.mu.Lock()
	defer d.health.mu.Unlock()
	return strings.Join(d.health.reasons, "; ")
}

// healthLoop: every 2 s; every 0.5 s while a problem is pending, so that the hold time (2 s at the shortest) is met within
// half a second of its end instead of at the next 2 s tick (which, with a 2 s hold, was a coin flip between 2 and 4 s).
func (d *Daemon) healthLoop() {
	for {
		d.healthStep()
		wait := 2 * time.Second
		d.health.mu.Lock()
		if len(d.health.failSince) > 0 {
			wait = 500 * time.Millisecond
		}
		d.health.mu.Unlock()
		time.Sleep(wait)
	}
}

func (d *Daemon) healthStep() {
	st := d.store.Get()
	h := d.health
	if st.PeerFP == "" || (st.State != StateActive && st.State != StateStandby) {
		return
	}
	// carrier of every data device, published in the heartbeat (the standby's value is the "can it take over" evidence)
	path := configPath()
	if st.State == StateStandby {
		path = activeXML(d.store.dir)
	}
	set, err := h.ifaces(path)
	if err != nil {
		return
	}
	links := map[string]bool{}
	carrier := map[string]bool{} // what this node sees on each device, for its own monitored interfaces
	var unknown []string
	byKey := map[string]string{}
	for _, i := range set.Data {
		up, known := linkState(i.Dev)
		carrier[i.Dev] = up
		if known {
			links[i.Dev] = up
		} else {
			unknown = append(unknown, i.Dev)
		}
		byKey[i.Key] = i.Dev
	}
	h.mu.Lock()
	h.links, h.linksUnknown = links, unknown
	h.mu.Unlock()
	if time.Since(h.lastMgmtProbe) >= 1900*time.Millisecond { // the probe pings: never faster than every 2 s, whatever the tick
		h.lastMgmtProbe = time.Now()
		d.mgmtProbeStep()
	}

	h.mu.Lock()
	inGrace := time.Now().Before(h.graceUntil)
	h.mu.Unlock()
	if st.State != StateActive || !d.cfg.autoOn() || d.maintenanceOn() || inGrace {
		h.mu.Lock()
		h.failSince, h.reasons = map[string]time.Time{}, nil
		h.mu.Unlock()
		return
	}

	failures := map[string]string{} // id -> description
	for _, key := range strings.Split(d.cfg.MonitorIfaces, ",") {
		key = strings.TrimSpace(key)
		if dev, ok := byKey[key]; ok && key != "" && !carrier[dev] {
			failures["link:"+dev] = "no carrier on " + key + " (" + dev + ")"
		}
	}
	if d.cfg.HealthServices {
		d.checkServices(failures)
	}
	if d.cfg.ProbeTargets != "" {
		if msg := d.probeFailed(); msg != "" {
			failures["probe"] = msg
		}
	}

	now := time.Now()
	h.mu.Lock()
	for id := range h.failSince {
		if _, still := failures[id]; !still {
			delete(h.failSince, id)
			delete(h.warned, id)
			go d.event("info", "health problem cleared: %s", id)
		}
	}
	var reasons []string
	hold := time.Duration(d.cfg.HealthHold) * time.Second
	if hold <= 0 {
		hold = 6 * time.Second
	}
	var due []string
	for id, desc := range failures {
		if _, seen := h.failSince[id]; !seen {
			h.failSince[id] = now
			go d.event("warning", "health problem on the active node: %s", desc)
		}
		reasons = append(reasons, desc)
		if now.Sub(h.failSince[id]) >= holdFor(id, hold) {
			due = append(due, id)
		}
	}
	h.reasons = reasons
	h.mu.Unlock()

	if len(due) == 0 {
		return
	}
	d.healthSwitch(due, failures)
}

// holdFor: a monitored link that lost its carrier is hard evidence (a dead card, a pulled cable, a switch that went away), so it
// is acted on after a third of the hold time, at least 2 s: the hold exists to ride out a flap, not to wait for a card that is
// gone. Services and probe targets keep the whole hold (they may only be restarting).
func holdFor(id string, hold time.Duration) time.Duration {
	if strings.HasPrefix(id, "link:") {
		return min(hold, max(hold/3, 2*time.Second))
	}
	return hold
}

func (d *Daemon) healthSwitch(due []string, failures map[string]string) {
	h := d.health
	alive, _, peer := d.hb.Snapshot()
	h.mu.Lock()
	defer h.mu.Unlock()
	warn := func(id, msg string) {
		if !h.warned[id] {
			h.warned[id] = true
			go d.event("warning", "%s", msg)
		}
	}
	if alive && peer.MgmtProbe == "fail" {
		warn("peerprobe", "health problem persists but the standby cannot reach its management probe targets either: staying active (the outage would only move)")
		return
	}
	if !alive || peer.State != StateStandby || peer.Health != "" {
		warn("peer", "health problem persists but the standby cannot take over (not reachable, not standby or unhealthy): staying active")
		return
	}
	for _, id := range due {
		if strings.HasPrefix(id, "link:") {
			dev := strings.TrimPrefix(id, "link:")
			// only a link the standby could READ and found down keeps this node active. A standby keeps its ports administratively
			// down (and a VLAN on a LAGG follows its parent), where "no carrier" means "not looked at", not "no cable": an unknown
			// state does not block, the ping-pong protection below limits the cost of being wrong.
			if v, known := peer.Links[dev]; known && !v && !contains(peer.LinksUnknown, dev) {
				warn(id, "no carrier on "+dev+" here, but the standby reads no link on its port either: not switching (the outage would only move)")
				return
			}
			if contains(peer.LinksUnknown, dev) {
				warn(id+":unverified", "the standby cannot read the link of "+dev+" (its port is kept down); switching without that confirmation")
			}
		}
	}
	now := time.Now()
	var recent []time.Time
	for _, t := range h.switches {
		if now.Sub(t) < 30*time.Minute {
			recent = append(recent, t)
		}
	}
	h.switches = recent
	if now.Before(h.cooldown) || len(recent) >= 2 {
		warn("pingpong", "health switch suppressed (cool-down or two health switches in 30 minutes): staying active, fix the cause")
		return
	}
	h.switches = append(h.switches, now)
	h.cooldown = now.Add(5 * time.Minute)
	var descs []string
	for _, id := range due {
		descs = append(descs, failures[id])
	}
	go func() {
		d.event("error", "health failure on the active node (%s): switching roles", strings.Join(descs, "; "))
		// forced on purpose: a broken active node must not be kept for the sake of the replication guard;
		// unreplicated edits are saved aside by the demote
		if _, err := d.failover(true); err != nil {
			d.event("error", "health switch failed: %v", err)
		}
	}()
}

// checkServices verifies the packet filter and IPsec (when enabled) and restarts them once per 15 s before declaring a failure.
func (d *Daemon) checkServices(failures map[string]string) {
	h := d.health
	restart := func(id string, cmd ...string) {
		h.mu.Lock()
		last := h.lastRestart[id]
		ok := time.Since(last) > 15*time.Second
		if ok {
			h.lastRestart[id] = time.Now()
		}
		h.mu.Unlock()
		if ok {
			go func() {
				d.event("warning", "restarting %s on the active node", id)
				sh(cmd[0], cmd[1:]...)
			}()
		}
	}
	if out, err := sh("pfctl", "-si"); err == nil && !strings.Contains(out, "Status: Enabled") {
		failures["svc:pf"] = "packet filter is not running"
		restart("pf", "configctl", "filter", "reload")
	}
	// IPsec only counts when it is enabled (registered as a service)
	if svc, _ := sh("pluginctl", "-S"); strings.Contains(svc, `"name": "strongswan"`) {
		if _, err := sh("pgrep", "-x", "charon"); err != nil {
			failures["svc:ipsec"] = "IPsec (strongSwan) is not running"
			restart("ipsec", "configctl", "ipsec", "restart")
		}
	}
}

// probeFailed pings the configured probe targets (e.g. the upstream router) from the active node, all at once. Only when
// none of them answers does it report a failure, so one flaky target never triggers anything.
func (d *Daemon) probeFailed() string { return d.pingAny(d.cfg.ProbeTargets, "") }

// pingAny pings the comma separated IPv4 targets (from the given source address, if any) all at once and returns a
// message when none answers, "" when at least one does or nothing is configured.
func (d *Daemon) pingAny(list, src string) string {
	var targets []string
	for _, t := range strings.Split(list, ",") {
		if ip := net.ParseIP(strings.TrimSpace(t)); ip != nil && ip.To4() != nil {
			targets = append(targets, ip.String())
		}
	}
	if len(targets) == 0 {
		return ""
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	up := 0
	for _, t := range targets {
		wg.Add(1)
		go func(t string) {
			defer wg.Done()
			args := []string{"-c", "1", "-t", "1"}
			if src != "" {
				args = append(args, "-S", src)
			}
			if _, err := sh("ping", append(args, t)...); err == nil {
				mu.Lock()
				up++
				mu.Unlock()
			}
		}(t)
	}
	wg.Wait()
	if up == 0 {
		return "none of the targets answers (" + strings.Join(targets, ", ") + ")"
	}
	return ""
}

// startGrace: called after a promote. Health checks wait at least 30 s (or the hold time if longer) before they can
// switch roles again, so a WAN that needs a DHCP lease or PPPoE login does not bounce the role straight back.
func (h *healthState) startGrace() {
	h.mu.Lock()
	h.graceUntil = time.Now().Add(30 * time.Second)
	h.mu.Unlock()
}

// Management probe (both nodes): the optional "management probe targets" (typically the management switch or router) are
// pinged from the management address. On the standby, whose data interfaces are down, this is the only way to learn
// that its own network side is alive: a standby that cannot reach them must not take over, and an active node that
// wants to hand over because of a health problem first checks that the standby can.

// mgmtProbeStep runs every health tick; two consecutive failures flip the state to "fail".
func (d *Daemon) mgmtProbeStep() {
	if d.cfg.MgmtProbeTargets == "" || d.mgmtAddr() == "" {
		d.health.mu.Lock()
		d.health.mgmtBad, d.health.mgmtMsg = 0, ""
		d.health.mu.Unlock()
		return
	}
	msg := d.pingAny(d.cfg.MgmtProbeTargets, d.mgmtAddr())
	h := d.health
	h.mu.Lock()
	defer h.mu.Unlock()
	if msg == "" {
		if h.mgmtBad >= 2 {
			go d.event("info", "management probe recovered")
		}
		h.mgmtBad, h.mgmtMsg = 0, ""
		return
	}
	h.mgmtBad++
	if h.mgmtBad == 2 {
		go d.event("warning", "management probe failed on this node: %s", msg)
	}
	h.mgmtMsg = msg
}

// mgmtProbe is "" (not configured), "ok" or "fail"; it goes into the heartbeat.
func (d *Daemon) mgmtProbe() string {
	if d.cfg.MgmtProbeTargets == "" || d.mgmtAddr() == "" {
		return ""
	}
	d.health.mu.Lock()
	defer d.health.mu.Unlock()
	if d.health.mgmtBad >= 2 {
		return "fail"
	}
	return "ok"
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// linkState reads a device: up is "status: active"; known is false only when the device is administratively down AND reports no
// carrier, because then nothing tells a missing cable from a port that was never brought up (a standby keeps its data ports down;
// a VLAN on a LAGG reports "no carrier" as soon as the LAGG is down). Devices that keep their link while down (ixl) are known.
func linkState(dev string) (up, known bool) {
	if !devNameRe.MatchString(dev) { // the name comes from the replicated configuration and becomes an argument of ifconfig
		return false, false
	}
	if dev == "" {
		return false, false
	}
	out, err := sh("ifconfig", dev)
	if err != nil {
		return false, false
	}
	return parseLinkState(out)
}

func parseLinkState(out string) (up, known bool) {
	up = strings.Contains(out, "status: active")
	first := strings.SplitN(out, "\n", 2)[0]
	adminUp := false
	if i, j := strings.Index(first, "<"), strings.Index(first, ">"); i >= 0 && j > i {
		for _, f := range strings.Split(first[i+1:j], ",") {
			adminUp = adminUp || f == "UP"
		}
	}
	return up, up || adminUp
}
