package main

import (
	"log"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Automatic failover and split-brain protection.
//
// Standby promotes itself only when:
//   * the peer is silent on EVERY heartbeat path (HA link and, if configured, management network) for
//     the detection time, and
//   * this node is not the isolated one: the management carrier is up (if mgmt is configured), or, with
//     the HA link alone, the HA link carrier is up. A peer that is silent while OUR HA cable is unplugged
//     is not proof that the peer is dead, so by default we do NOT promote in that case.
// Nobody active at all (both booted into standby): the master role promotes after a short delay, the
// secondary only after a long one. If two nodes are ever active together, the higher epoch wins
// (tie: the master role) and the loser demotes itself immediately.

const (
	electionMaster        = 5 * time.Second  // both standby, peer alive: master takes over after this
	electionSecondary     = 30 * time.Second // ... the secondary only if the master did not
	electionInMaintenance = 2 * time.Minute  // added to both while maintenance mode is on
	bootWaitMaster        = 10 * time.Second // peer never seen since daemon start
	bootWaitSecondary     = 30 * time.Second
	minHold               = 10 * time.Second // minimum time between automatic role changes
)

type autoState struct {
	noActiveSince   time.Time
	dualSince       time.Time
	lastChange      time.Time
	peerActiveSince time.Time    // preemption: how long the peer has been active, alive and healthy
	peerBusySince   time.Time    // the peer has been promoting/demoting since
	dualBusySince   time.Time    // both nodes active, but the peer reports a transition in progress since
	lastSBTry       time.Time    // last split-brain demotion attempt (retry throttle)
	blocked         atomic.Value // string: why automatic promotion is currently held back
	holdUntil       atomicTime   // no election of this node until then (the peer is taking its role back after an aborted hand-over)
	probeAt         time.Time    // the last probe of the peer system and what it found (autoStep only)
	probeAddr       string
	detectMs        atomic.Int64 // silence of the peer when the automatic promotion was decided (for the history)
	lastTick        time.Time    // when autoStep last ran (to notice that this daemon itself was frozen)
}

// Split-brain resolution timing. It is deliberately NOT subject to minHold (anti-flapping): two active nodes are an
// emergency, not a role change that may be damped.
// stallGap: autoStep runs every 500 ms. If it did not run for this long the daemon itself was frozen (SIGSTOP, a paused or migrated
// virtual machine, a starved machine) and the silence of the peer measured across that gap says nothing about the peer.
var stallGap = 3 * time.Second

const (
	sbBusyMax = time.Minute     // a peer "busy" this long no longer excuses a double active
	sbRetry   = 2 * time.Second // between two attempts to stand down
)

// carrier reports the link state of a device; replaced in the tests.
var carrier = carrierUp

func (a *autoState) setBlocked(s string) { a.blocked.Store(s) }
func (a *autoState) Blocked() string {
	if v, ok := a.blocked.Load().(string); ok {
		return v
	}
	return ""
}

func carrierUp(dev string) bool {
	if dev == "" {
		return false
	}
	out, err := sh("ifconfig", dev)
	if err != nil {
		return false
	}
	return strings.Contains(out, "status: active")
}

func (d *Daemon) autoLoop() {
	for range time.Tick(500 * time.Millisecond) {
		d.autoStep()
	}
}

func (d *Daemon) autoStep() {
	now := time.Now()
	if !d.auto.lastTick.IsZero() && now.Sub(d.auto.lastTick) > stallGap {
		d.event("warning", "this daemon did not run for %s (frozen, paused or starved): the silence of the peer across that gap is not evidence, the detection starts again from now", now.Sub(d.auto.lastTick).Round(time.Second))
		d.hb.rebase()
	}
	// stamped when the step ENDS: a step that promotes this node takes seconds, and that is this daemon working, not a freeze
	defer func() { d.auto.lastTick = time.Now() }()
	st := d.store.Get()
	if st.PeerFP == "" || (st.State != StateActive && st.State != StateStandby) {
		d.auto.noActiveSince, d.auto.dualSince, d.auto.dualBusySince = time.Time{}, time.Time{}, time.Time{}
		d.auto.setBlocked("")
		return
	}
	if !d.ops.mu.TryLock() { // promote/demote in progress
		return
	}
	d.ops.mu.Unlock()
	alive, _, peer := d.hb.Snapshot()

	// A double active is resolved first and always: whatever the anti-flapping hold (minHold), the maintenance mode or the
	// "automatic failover" switch say. Standing down never makes the cluster less safe.
	if st.State == StateActive {
		d.auto.setBlocked("")
		d.splitBrainStep(st, alive, peer)
		return
	}
	d.auto.dualSince, d.auto.dualBusySince = time.Time{}, time.Time{}
	if !d.cfg.autoOn() {
		d.auto.noActiveSince = time.Time{}
		d.auto.setBlocked("")
		return
	}
	if time.Since(d.auto.lastChange) < minHold || d.failing.Load() {
		return
	}
	maint := d.maintenanceOn() // pauses everything below

	switch st.State {
	case StateStandby:
		// Maintenance pauses the takeovers (after a silence of the peer, preemption, health). It does not pause the election of a pair
		// in which both nodes answer and nobody is active (a failed hand-over of the guided upgrade, for example): that is not a
		// failover, nobody serves at all. It only waits longer, to leave room for what the operator is doing.
		if maint && (!alive || peer.State == StateActive) {
			d.auto.noActiveSince, d.auto.peerActiveSince = time.Time{}, time.Time{}
			d.auto.setBlocked("")
			return
		}
		if alive {
			if peer.State == StateActive {
				d.auto.peerBusySince = time.Time{}
				d.auto.noActiveSince = time.Time{}
				d.auto.setBlocked("")
				d.preemptStep(st, peer)
				return
			}
			d.auto.peerActiveSince = time.Time{}
			if peer.Busy {
				if d.auto.peerBusySince.IsZero() {
					d.auto.peerBusySince = time.Now()
				}
				if time.Since(d.auto.peerBusySince) < time.Minute {
					// the peer is taking over right now (a manual failover takes a few seconds): wait for it
					d.auto.noActiveSince = time.Time{}
					return
				}
			} else {
				d.auto.peerBusySince = time.Time{}
			}
			// peer is alive but nobody is active (typically both just booted)
			if d.auto.noActiveSince.IsZero() {
				d.auto.noActiveSince = time.Now()
			}
			wait := electionSecondary
			if st.Role == RoleMaster {
				wait = electionMaster
			}
			if maint {
				wait += electionInMaintenance // added, not a maximum: the master still goes first
			}
			if time.Now().Before(d.auto.holdUntil.Get()) {
				return // the peer cancelled a hand-over and is taking its role back: do not race it
			}
			if time.Since(d.auto.noActiveSince) >= wait {
				d.autoPromote("election: no active node, "+st.Role+" role", false)
			}
			return
		}
		d.auto.noActiveSince, d.auto.peerActiveSince = time.Time{}, time.Time{}

		// peer silent on every path
		need := hbDead
		if d.hb.neverSeen() { // never heard from the peer since this daemon started: boot case
			need = bootWaitSecondary
			if st.Role == RoleMaster {
				need = bootWaitMaster
			}
		}
		if d.hb.silentFor() < need {
			return
		}
		mgmtOK := d.mgmtAddr() != "" && carrier(d.cfg.MgmtDevice) && d.mgmtProbe() != "fail"
		haOK := carrier(d.cfg.HADevice)
		switch {
		case mgmtOK:
			// both paths silent while our management network is healthy: the peer is down
		case d.mgmtAddr() == "" && haOK:
			// HA link only, but our end is healthy: peer is down
		case d.cfg.AllowHAOnly && d.mgmtAddr() == "":
			// operator accepted the risk of promoting without proof that we are not the isolated node
		default:
			if d.auto.Blocked() == "" {
				d.event("warning", "peer silent but automatic promotion held back: this node cannot prove it is not isolated")
			}
			d.auto.setBlocked("peer is silent, but this node cannot prove it is not the isolated one " +
				"(HA/management carrier or management probe down). Promote manually if the peer is really down.")
			return
		}
		// The daemon of the peer may be what died (killed, hung, stopped by hand) while its system still forwards with the shared
		// identity. Its system answering ping on the HA link or on management proves that much: then only fencing (switching it
		// off) makes a takeover safe. No answer proves nothing more than before (a partition looks the same) and changes nothing.
		if addr := d.peerSystemAnswers(st); addr != "" && d.cfg.FenceCommand == "" {
			why := "the daemon of the peer is silent but its system still answers ping (" + addr + "): it may still forward traffic with the shared " +
				"addresses. Not taking over by itself. Restart hasyncd on the peer, or switch the peer off and promote this node by hand " +
				"(a fencing command makes this automatic)."
			if d.auto.Blocked() != why {
				d.event("error", "automatic takeover refused: %s", why)
			}
			d.auto.setBlocked(why)
			return
		}
		d.autoPromote("peer lost on all heartbeat paths", true)
	}
}

// autoPromote promotes this node. With fence set (the peer went silent) the configured fencing command must
// succeed first, otherwise the promotion is cancelled: a silent peer may still be alive.
func (d *Daemon) autoPromote(reason string, fence bool) {
	// a software update (opnsense-update, pkg upgrade) is running on THIS node: it replaces the base system and the packages that a
	// takeover uses. No takeover of its own starts next to it; the peer elects itself, or the update ends (it has a deadline).
	if d.updateInProgress() {
		const why = "a software update is running on this node: not taking over by itself until it ends"
		if d.auto.Blocked() != why {
			d.event("warning", "automatic takeover postponed: %s", why)
		}
		d.auto.setBlocked(why)
		return
	}
	// policy: no takeover on the strength of a silence alone when nothing can switch the peer off (see the setting "fence_required")
	if fence && d.cfg.FenceRequired && d.cfg.FenceCommand == "" {
		const why = "the peer is silent, but fencing is required (Settings > Safety) and no fencing command is configured: not taking over by itself. Promote manually once the peer is known to be off."
		if d.auto.Blocked() == "" {
			d.event("error", "automatic takeover refused: %s", why)
		}
		d.auto.setBlocked(why)
		return
	}
	d.auto.lastChange = time.Now()
	d.auto.noActiveSince = time.Time{}
	d.auto.setBlocked("")
	if fence {
		d.auto.detectMs.Store(d.hb.silentFor().Milliseconds())
	}
	if fence && d.cfg.FenceCommand != "" {
		d.event("warning", "fencing the peer before promoting (%s)", d.cfg.FenceCommand)
		if err := d.runFence(); err != nil {
			d.event("error", "promotion cancelled: %v", err)
			d.auto.setBlocked("promotion cancelled, fencing failed: " + err.Error())
			return
		}
	}
	if err := d.doPromote("AUTO: " + reason); err != nil {
		log.Printf("automatic promote failed: %v", err)
		d.auto.setBlocked("automatic promote failed: " + err.Error())
		// stay standby, never leave half-configured interfaces up
		d.ops.mu.Lock()
		d.ops.cache = IfaceSet{}
		d.enforceOnce()
		d.ops.mu.Unlock()
	}
}

// splitBrainStep: this node is active. If the peer is active too, the loser (lower epoch, on a tie the secondary) stands
// down. A peer that reports a transition in progress gets a limited grace period: a graceful failover legitimately
// overlaps for a moment, a promote or demote stuck for a minute does not.
func (d *Daemon) splitBrainStep(st State, alive bool, peer hbMsg) {
	if !alive || peer.State != StateActive {
		d.auto.dualSince, d.auto.dualBusySince = time.Time{}, time.Time{}
		return
	}
	if peer.Busy {
		if d.auto.dualBusySince.IsZero() {
			d.auto.dualBusySince = time.Now()
		}
		if time.Since(d.auto.dualBusySince) < sbBusyMax {
			d.auto.dualSince = time.Time{}
			return
		}
	} else {
		d.auto.dualBusySince = time.Time{}
	}
	if d.auto.dualSince.IsZero() {
		d.auto.dualSince = time.Now()
		d.event("error", "SPLIT-BRAIN: both nodes active (my epoch %d, peer %d)", st.Epoch, peer.Epoch)
		log.Printf("SPLIT-BRAIN detected: both nodes active (my epoch %d, peer %d)", st.Epoch, peer.Epoch)
	}
	// two consecutive heartbeats must agree before acting: one stale packet from before a hand-over is not a conflict
	confirm := 2 * hbInterval
	if confirm < 300*time.Millisecond {
		confirm = 300 * time.Millisecond
	}
	if time.Since(d.auto.dualSince) < confirm {
		return
	}
	loser := st.Epoch < peer.Epoch || (st.Epoch == peer.Epoch && st.Role != RoleMaster)
	if !loser || time.Since(d.auto.lastSBTry) < sbRetry {
		return
	}
	d.auto.lastSBTry = time.Now()
	d.auto.lastChange = time.Now()
	if err := d.doDemote("split-brain: peer wins (higher epoch or master role)", nil); err != nil {
		d.event("error", "split-brain: standing down failed: %v (retrying)", err)
	}
}

// peerSystemAnswers pings the system of the peer on the HA link and on its management address, at the same time, once each (300 ms).
// It returns the first address that answered, "" if none did. The kernel answers ping without the daemon: an answer means that the
// machine runs, whatever its daemon does.
func (d *Daemon) peerSystemAnswers(st State) string {
	// the step runs twice a second while the peer is silent: the probe (a ping, and an ARP resolution that deletes the cached entry)
	// runs at most every probeEvery, and what it found is reused in between
	if !d.auto.probeAt.IsZero() && time.Since(d.auto.probeAt) < probeEvery {
		return d.auto.probeAddr
	}
	addr := d.probePeerSystem(st)
	d.auto.probeAt, d.auto.probeAddr = time.Now(), addr
	return addr
}

// probeEvery: how often the peer system is probed while its daemon is silent.
const probeEvery = 5 * time.Second

func (d *Daemon) probePeerSystem(st State) string {
	addrs := []string{}
	for _, a := range []string{st.PeerAddr, st.PeerMgmt} {
		if a != "" && net.ParseIP(a).To4() != nil {
			addrs = append(addrs, a)
		}
	}
	res := make(chan string, len(addrs))
	for _, a := range addrs {
		go func(a string) {
			if d.pingOnce(a) {
				res <- a
				return
			}
			res <- ""
		}(a)
	}
	found := ""
	for range addrs {
		if a := <-res; a != "" && found == "" {
			found = a
		}
	}
	return found
}

// pingOnce asks whether the machine at addr is there: an ICMP echo, and if that gets no answer a fresh ARP resolution. A pf rule or
// a sysctl (net.inet.icmp.icmplim, icmp_bmcastecho) can silence ICMP while the machine runs and forwards; ARP is answered by the
// kernel below pf, and only means something for an address on a segment this node is attached to (the HA link): the cached entry
// is deleted first, so that an answer was given just now and not 20 minutes ago.
func (d *Daemon) pingOnce(addr string) bool {
	if d.icmpAnswers(addr) {
		return true
	}
	return d.arpAnswers(addr)
}

func (d *Daemon) icmpAnswers(addr string) bool {
	if d.act != nil && d.act.ping != nil {
		return d.act.ping(addr)
	}
	_, err := sh("ping", "-c", "1", "-W", "300", "-q", addr) // FreeBSD: -W in milliseconds
	return err == nil
}

func (d *Daemon) arpAnswers(addr string) bool {
	d.run("arp", "-d", addr)
	d.run("ping", "-c", "1", "-W", "300", "-q", addr) // makes the kernel ask who has addr, whatever the answer to the echo is
	out, err := d.run("arp", "-n", addr)
	return err == nil && arpResolved(out)
}

// arpResolved reads `arp -n addr`: "? (10.99.0.1) at 0c:c4:7a:1:2:3 on igb0 expires in 1197 seconds [ethernet]" is an answer,
// "at (incomplete)" and "no entry" are not.
func arpResolved(out string) bool {
	i := strings.Index(out, " at ")
	if i < 0 {
		return false
	}
	f := strings.Fields(out[i+4:])
	if len(f) == 0 || strings.HasPrefix(f[0], "(") {
		return false
	}
	// BSD prints each byte without a leading zero ("1" for "01"): net.ParseMAC would refuse it
	parts := strings.Split(f[0], ":")
	if len(parts) != 6 {
		return false
	}
	for _, p := range parts {
		if _, err := strconv.ParseUint(p, 16, 8); err != nil {
			return false
		}
	}
	return true
}
