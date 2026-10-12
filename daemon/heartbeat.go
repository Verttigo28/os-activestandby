package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// hbDead: no heartbeat on a path for this long = path dead (the failure detection time, setting "detect_time").
// Heartbeats go out several times per detection period so one lost packet never triggers anything.
var (
	hbInterval = 500 * time.Millisecond
	hbDead     = 3 * time.Second
)

func setDetection(sec int) {
	if sec < 1 || sec > 30 {
		sec = 3
	}
	hbDead = time.Duration(sec) * time.Second
	hbInterval = hbDead / 8
	if hbInterval < 150*time.Millisecond {
		hbInterval = 150 * time.Millisecond
	}
}

type hbMsg struct {
	Role   string `json:"role"`
	State  string `json:"state"`
	Epoch  uint64 `json:"epoch"`
	Seq    uint64 `json:"seq"`
	Mgmt   string `json:"mgmt,omitempty"`    // sender's management IPv4, lets the peer open the secondary path
	Resync bool   `json:"resync,omitempty"`  // standby asks the active node for a full config push
	TimeMs int64  `json:"time_ms,omitempty"` // sender clock; the standby follows the active node
	Ver    string `json:"ver,omitempty"`     // OPNsense version of the sender
	Plug   string `json:"plug,omitempty"`    // hasyncd (plugin) version of the sender
	Warm   string `json:"warm,omitempty"`    // warm standby: "ready", or why this node could not take over fast (empty: classic mode)

	Links   map[string]bool `json:"links,omitempty"`   // carrier of the sender's data devices
	Health  string          `json:"health,omitempty"`  // non-empty: the sender (active) currently fails a health check
	Applied string          `json:"applied,omitempty"` // hash of the last config revision the sender (standby) applied

	Busy       bool   `json:"busy,omitempty"`        // sender is in the middle of a promote/demote: the peer must not elect itself meanwhile
	Host       string `json:"host,omitempty"`        // sender hostname, shown in the GUI so both firewalls can be told apart
	MgmtProbe  string `json:"mgmt_probe,omitempty"`  // "ok"/"fail": can the sender reach its management probe targets (empty: not configured)
	Maint      bool   `json:"maint,omitempty"`       // maintenance mode as the sender knows it ...
	MaintUntil string `json:"maint_until,omitempty"` // ... its deadline ...
	MaintAt    string `json:"maint_at,omitempty"`    // ... and when it last changed (the newest change wins on both nodes)
}

// Heartbeat tracks the peer over up to two independent paths: the dedicated HA link ("ha") and the
// optional management network ("mgmt"). A peer is considered alive if ANY path is alive.
type Heartbeat struct {
	mu       sync.Mutex
	pathLast map[string]time.Time
	last     time.Time
	peer     hbMsg
	seq      uint64
	started  time.Time
	rtt      map[string]time.Duration
}

func newHeartbeat() *Heartbeat {
	return &Heartbeat{pathLast: map[string]time.Time{}, started: time.Now(), rtt: map[string]time.Duration{}}
}

func (h *Heartbeat) setRTT(path string, d time.Duration) {
	h.mu.Lock()
	h.rtt[path] = d
	h.mu.Unlock()
}

// RTT is the last heartbeat round-trip time measured on the path.
func (h *Heartbeat) RTT(path string) time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rtt[path]
}

func (h *Heartbeat) seen(path string, m hbMsg) {
	h.mu.Lock()
	now := time.Now()
	h.pathLast[path], h.last, h.peer = now, now, m
	h.mu.Unlock()
}

func (h *Heartbeat) PathAlive(path string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.pathLast[path]
	return ok && time.Since(t) < hbDead
}

func (h *Heartbeat) Snapshot() (alive bool, ago time.Duration, peer hbMsg) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.last.IsZero() {
		return false, 0, h.peer
	}
	ago = time.Since(h.last)
	return ago < hbDead, ago, h.peer
}

// silentFor is how long no path has delivered a heartbeat (since daemon start if never).
func (h *Heartbeat) neverSeen() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last.IsZero()
}

func (h *Heartbeat) silentFor() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.last.IsZero() {
		return time.Since(h.started)
	}
	return time.Since(h.last)
}

func (d *Daemon) localHB() hbMsg {
	st := d.store.Get()
	d.hb.mu.Lock()
	d.hb.seq++
	s := d.hb.seq
	d.hb.mu.Unlock()
	return hbMsg{Role: st.Role, State: st.State, Epoch: st.Epoch, Seq: s, Mgmt: d.mgmtAddr(), TimeMs: time.Now().UnixMilli(), Ver: d.localVer, Plug: version,
		Resync:  st.State == StateStandby && st.PeerFP != "" && (!activeValid(d.store.dir) || d.driftDetected()),
		Links:   d.health.snapshotLinks(),
		Health:  d.healthReason(),
		Applied: st.AppliedSHA, Busy: d.opsBusy(), Warm: d.warmSummary(), Host: hostName(), MgmtProbe: d.mgmtProbe(), Maint: st.Maintenance, MaintUntil: st.MaintenanceUntil, MaintAt: st.MaintAt}
}

func (d *Daemon) handleHB(w http.ResponseWriter, r *http.Request) {
	path := "ha"
	if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if h, _, err := net.SplitHostPort(la.String()); err == nil && h != "" && h == d.mgmtAddr() {
			path = "mgmt"
		}
	}
	var m hbMsg
	if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&m) == nil {
		d.hb.seen(path, m)
		d.learnPeerMgmt(m.Mgmt)
		d.peerWantsResync(m)
		d.adjustClock(m)
		d.checkVersions(m)
		d.reconcileMaintenance(m)
	}
	writeJSON(w, 200, d.localHB())
}

func (d *Daemon) learnPeerMgmt(addr string) {
	if addr != "" && d.store.Get().PeerMgmt != addr {
		d.store.Update(func(s *State) { s.PeerMgmt = addr })
	}
}

// hbLoop starts one persistent mutual-TLS heartbeat sender per path.
func (d *Daemon) hbLoop() {
	go d.hbPath("ha", func(s State) string { return s.PeerAddr })
	go d.hbPath("mgmt", func(s State) string { return s.PeerMgmt })
}

func (d *Daemon) hbPath(path string, addrOf func(State) string) {
	var cl *http.Client
	var pinned string
	for range time.Tick(hbInterval) {
		st := d.store.Get()
		addr := addrOf(st)
		if st.PeerFP == "" || addr == "" || (path == "mgmt" && d.mgmtAddr() == "") {
			cl, pinned = nil, ""
			continue
		}
		if cl == nil || pinned != st.PeerFP+addr {
			cl = d.peerClient(st.PeerFP, nil)
			cl.Timeout = time.Second
			cl.Transport.(*http.Transport).DisableKeepAlives = false
			pinned = st.PeerFP + addr
		}
		b, _ := json.Marshal(d.localHB())
		t0 := time.Now()
		resp, err := cl.Post("https://"+net.JoinHostPort(addr, fmt.Sprint(st.PeerPort))+"/v1/hb", "application/json", bytes.NewReader(b))
		if err != nil {
			continue
		}
		var m hbMsg
		if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&m) == nil && resp.StatusCode == 200 {
			d.hb.setRTT(path, time.Since(t0))
			d.hb.seen(path, m)
			d.learnPeerMgmt(m.Mgmt)
			d.peerWantsResync(m)
			d.adjustClock(m)
			d.checkVersions(m)
			d.reconcileMaintenance(m)
		}
		resp.Body.Close()
	}
}

// ---- failover ----

func (d *Daemon) peerCall(path string, timeout time.Duration) (map[string]any, error) {
	st := d.store.Get()
	if st.PeerFP == "" {
		return nil, errors.New("not paired")
	}
	cl := d.peerClient(st.PeerFP, nil)
	cl.Timeout = timeout
	resp, err := cl.Post("https://"+net.JoinHostPort(d.peerEndpoint(st), fmt.Sprint(st.PeerPort))+path, "application/json", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out)
	if resp.StatusCode != 200 {
		return out, fmt.Errorf("peer answered %d: %v", resp.StatusCode, out["error"])
	}
	return out, nil
}

// handleHandover: the standby asks us (active) to hand over: we demote, then ask it to promote.
func (d *Daemon) handleHandover(w http.ResponseWriter, r *http.Request) {
	msg, err := d.failover(r.URL.Query().Get("force") == "1")
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok", "message": msg})
}

// failover performs a graceful switchover. Order matters: demote first so the two nodes are never
// active together, then let the peer promote. If the answer of the peer is lost, this node takes its role back ONLY when the
// peer has confirmed (see transition.go) that it did not and now cannot take over; on any doubt it stays standby: no
// service for a moment is better than the same IP/MAC on two nodes.
func (d *Daemon) failover(force bool) (string, error) {
	d.failMu.Lock()
	defer d.failMu.Unlock()
	d.failing.Store(true)
	defer d.failing.Store(false)
	st := d.store.Get()
	if st.PeerFP == "" {
		return "", errors.New("not paired")
	}
	if st.State != StateActive {
		if _, err := d.callPeer(map[bool]string{true: "/v1/handover?force=1", false: "/v1/handover"}[force], 90*time.Second); err != nil {
			return "", fmt.Errorf("handover request to active peer failed: %w", err)
		}
		return "peer handed over, this node is now active", nil
	}
	if !force {
		if b, err := os.ReadFile(configPath()); err == nil && shaHex(b) != st.AckedSHA {
			reason := st.SyncError
			if reason == "" {
				reason = "not replicated yet"
			}
			return "", fmt.Errorf("this node has configuration changes the peer does not have (%s); a failover would lose them. Fix the sync problem first, or force the failover", reason)
		}
	}
	alive, _, peer := d.hb.Snapshot()
	if !alive {
		return "", errors.New("peer is not reachable; promote it manually (force) if it is really down")
	}
	if peer.State != StateStandby {
		return "", fmt.Errorf("peer state is %q, expected standby", peer.State)
	}
	t0 := time.Now()
	tid := newTID()
	// the peer is told to promote as soon as our interfaces are down, while we finish tidying up
	var perr error
	done := make(chan struct{})
	fired := false
	var sentAt time.Time
	derr := d.doDemote("failover "+tid[:8], func() {
		fired = true
		sentAt = time.Now()
		go func() {
			_, perr = d.callPeer(fmt.Sprintf("/v1/promote?tid=%s&epoch=%d&exp=%d", tid, st.Epoch, sentAt.Add(reqTTL).UnixMilli()), 90*time.Second)
			close(done)
		}()
	})
	if !fired {
		// nothing was asked of the peer: it stays what it was
		if derr == nil {
			derr = errors.New("demote did not complete")
		}
		return "", fmt.Errorf("demote failed: %w", derr)
	}
	<-done
	// the peer answering "already active" means the switchover had already happened
	if perr != nil && strings.Contains(perr.Error(), "already active") {
		perr = nil
	}
	// from here on our data interfaces are down: whatever the cleanup said, we are not serving
	if derr != nil {
		d.store.Update(func(s *State) { s.State = StateStandby })
	}
	if perr == nil {
		if derr != nil {
			d.event("warning", "demote cleanup failed after the peer took over: %v", derr)
			return "switched over, but the cleanup on this node failed (see events)", nil
		}
		return fmt.Sprintf("switched over in %d ms (this node is now standby)", time.Since(t0).Milliseconds()), nil
	}

	// no usable answer: ask the peer what became of the transition instead of guessing
	d.event("warning", "failover %s: no confirmation from the peer (%v); checking what the peer did", tid[:8], perr)
	outcome, detail := d.resolveTransition(tid, sentAt)
	switch outcome {
	case outcomePeerActive:
		d.event("change", "failover %s: the peer is active (%s); this node stays standby", tid[:8], detail)
		return fmt.Sprintf("switched over in %d ms (the peer's answer was lost but it is active; this node is now standby)", time.Since(t0).Milliseconds()), nil
	case outcomePeerStandby:
		rb := d.doPromote("failover rollback " + tid[:8] + ": the peer confirmed it did not take over")
		d.event("warning", "failover %s: peer did not take over (%v; %s); rolled back (%v)", tid[:8], perr, detail, rb)
		return "", fmt.Errorf("peer did not take over (%v); this node took its role back after the peer confirmed it will not become active (rollback error: %v)", perr, rb)
	default:
		d.event("error", "failover %s: the outcome on the peer is unknown (%s). This node stays STANDBY and does not roll back, so the same addresses can never be active on both nodes. If no node serves traffic, check the peer, then promote one node manually", tid[:8], detail)
		return "", fmt.Errorf("the peer did not confirm the takeover (%v) and its state could not be verified (%s): this node stays standby on purpose to avoid two active nodes; check the peer, then promote one node manually", perr, detail)
	}
}

// peerWantsResync: a standby whose saved config is damaged asks us (active) for a full push.
func (d *Daemon) peerWantsResync(m hbMsg) {
	if m.Resync && d.store.Get().State == StateActive && d.store.Get().AckedSHA != "" {
		log.Printf("peer reports a damaged saved config: queueing full resync")
		d.store.Update(func(s *State) { s.AckedSHA = "" })
		d.wakeSync()
	}
}

// checkVersions flags nodes that run different OPNsense or plugin versions (normal while updating one node, a
// problem if it stays that way: config sync is refused across core versions).
func (d *Daemon) checkVersions(m hbMsg) {
	if m.Ver == "" || d.localVer == "" {
		return
	}
	peer := m.Ver
	if m.Plug != "" && m.Plug != version {
		peer += " (plugin " + m.Plug + ")"
	}
	mism := m.Ver != d.localVer || (m.Plug != "" && m.Plug != version)
	d.verMu.Lock()
	changed := mism != d.verMismatch || (mism && peer != d.peerVer)
	d.peerVer, d.verMismatch = peer, mism
	d.verMu.Unlock()
	if !changed {
		return
	}
	if !mism {
		// an error caused by the version difference is obsolete now
		if st := d.store.Get(); strings.Contains(st.SyncError, "version differs") {
			d.store.Update(func(s *State) { s.SyncError = "" })
		}
	}
	if mism {
		d.event("warning", "peer runs %s, this node runs %s (plugin %s): finish updating both nodes; config sync is refused across core versions", peer, d.localVer, version)
	} else {
		d.event("info", "both nodes run %s again", d.localVer)
	}
}

func (d *Daemon) versionInfo() (peer string, mismatch bool) {
	d.verMu.Lock()
	defer d.verMu.Unlock()
	return d.peerVer, d.verMismatch
}

// opsBusy reports a promote/demote in progress on this node (they hold the ops lock).
func (d *Daemon) opsBusy() bool {
	if d.ops == nil || !d.ops.mu.TryLock() {
		return d.ops != nil
	}
	d.ops.mu.Unlock()
	return false
}
