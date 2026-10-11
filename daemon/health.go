package main

import (
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
	mu          sync.Mutex
	failSince   map[string]time.Time
	reasons     []string
	cooldown    time.Time
	switches    []time.Time
	lastRestart map[string]time.Time
	warned      map[string]bool
	links       map[string]bool // carrier of this node's data devices, sent in every heartbeat
	devMu       sync.Mutex
	devPath     string
	devMtime    time.Time
	devSet      IfaceSet
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

func (d *Daemon) healthLoop() {
	for range time.Tick(2 * time.Second) {
		d.healthStep()
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
	byKey := map[string]string{}
	for _, i := range set.Data {
		links[i.Dev] = carrierUp(i.Dev)
		byKey[i.Key] = i.Dev
	}
	h.mu.Lock()
	h.links = links
	h.mu.Unlock()

	if st.State != StateActive || !d.cfg.autoOn() || d.maintenanceOn() {
		h.mu.Lock()
		h.failSince, h.reasons = map[string]time.Time{}, nil
		h.mu.Unlock()
		return
	}

	failures := map[string]string{} // id -> description
	for _, key := range strings.Split(d.cfg.MonitorIfaces, ",") {
		key = strings.TrimSpace(key)
		if dev, ok := byKey[key]; ok && key != "" && !links[dev] {
			failures["link:"+dev] = "no carrier on " + key + " (" + dev + ")"
		}
	}
	if d.cfg.HealthServices {
		d.checkServices(failures)
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
		if now.Sub(h.failSince[id]) >= hold {
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
	if !alive || peer.State != StateStandby || peer.Health != "" {
		warn("peer", "health problem persists but the standby cannot take over (not reachable, not standby or unhealthy): staying active")
		return
	}
	for _, id := range due {
		if strings.HasPrefix(id, "link:") {
			dev := strings.TrimPrefix(id, "link:")
			if !peer.Links[dev] {
				warn(id, "no carrier on "+dev+" here, but the standby cannot confirm link on its port either: not switching (the outage would only move)")
				return
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
