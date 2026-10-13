package main

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// Transactional hand-over.
//
// A graceful failover asks the peer to promote after this node demoted itself. The answer can be lost (timeout,
// cable pulled, peer restarted) while the peer did promote. Rolling back on the strength of a missing answer would put
// the same IP/MAC on both nodes. Every promote request therefore carries a unique transition id (tid), and the peer keeps
// a small record of what it did with it:
//
//	started -> done | failed          (the request was executed)
//	aborted                           (the requester cancelled it before it started: it can never run afterwards)
//
// Before this node takes its role back it asks the peer to abort the transition (/v1/abort). The peer answers with the
// state of the tid and its own role, atomically with respect to /v1/promote. Only "the peer never started it (now it
// cannot) or failed it, and it is standby" allows a rollback. Anything else keeps this node standby.

const (
	txStarted = "started"
	txDone    = "done"
	txFailed  = "failed"
	txAborted = "aborted"

	txKeep = 64 // transitions remembered

	// the epochs of the two nodes differ by a handful; a request that is further ahead than this is not from the peer
	maxEpochAhead = 1000
)

var tidRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// How long (and how often) the requester polls the peer for the outcome of a transition whose answer was lost.
// How long the old active node waits for the answer of the peer to "promote" (a classic takeover takes seconds, a few ten seconds on a
// big configuration; the request itself is only valid for reqTTL), and how long a standby waits for the active node to hand over.
const (
	promoteReplyTimeout = 45 * time.Second
	handoverTimeout     = 120 * time.Second
)

var (
	resolveEvery  = time.Second // read and written only through resolveTiming and setResolveTiming (a stray goroutine of an earlier test may still poll)
	resolveWindow = 45 * time.Second
	// A promote request is valid for reqTTL after it was sent (the peer refuses it later, allowing reqSkew for clock differences):
	// a request that sat in a frozen peer's socket buffer must not be executed minutes later, when this node has long moved on.
	reqTTL  = 10 * time.Second
	reqSkew = 5 * time.Second
	// abortHold: after it cancelled a transition the peer (this node) does not elect itself active on its own for a while,
	// the requester is about to take its role back.
	abortHold = 45 * time.Second
)

type txLog struct {
	mu    sync.Mutex
	m     map[string]string
	order []string
}

func newTID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // no entropy: refusing to run is safer than a predictable id
	}
	return hex.EncodeToString(b)
}

func (t *txLog) set(tid, status string) {
	if t.m == nil {
		t.m = map[string]string{}
	}
	if _, ok := t.m[tid]; !ok {
		t.order = append(t.order, tid)
		if len(t.order) > txKeep {
			delete(t.m, t.order[0])
			t.order = t.order[1:]
		}
	}
	t.m[tid] = status
}

// begin registers the execution of tid. "" means: go ahead. Otherwise the existing status is returned and nothing may run.
func (t *txLog) begin(tid string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.m[tid]; s != "" {
		return s
	}
	t.set(tid, txStarted)
	return ""
}

func (t *txLog) finish(tid string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		t.set(tid, txFailed)
		return
	}
	t.set(tid, txDone)
}

// abort cancels tid if it never started and returns its status afterwards.
func (t *txLog) abort(tid string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.m[tid]; s != "" {
		return s
	}
	t.set(tid, txAborted)
	return txAborted
}

// ---- peer side ----

// handlePromote: the active peer asks us (standby) to take over after it demoted itself.
func (d *Daemon) handlePromote(w http.ResponseWriter, r *http.Request) {
	// All three parameters are required and strictly parsed: a request without a transition id has no replay protection, one without an
	// epoch or an expiry cannot be told from a stale or delayed one. Both nodes of a pair run the same version (the plugin version
	// mismatch is reported), and every version since the transitions were introduced sends all three.
	q := r.URL.Query()
	tid := q.Get("tid")
	if !tidRe.MatchString(tid) {
		writeJSON(w, 400, map[string]string{"error": "missing or malformed transition id"})
		return
	}
	epoch, err := strconv.ParseUint(q.Get("epoch"), 10, 64)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "missing or malformed epoch"})
		return
	}
	ms, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil || ms <= 0 {
		writeJSON(w, 400, map[string]string{"error": "missing or malformed expiry"})
		return
	}
	st := d.store.Get()
	// a command from a node whose leadership generation is older than ours is stale (delayed, duplicated, or sent by a node
	// that lost a split-brain): never obey it. One from a generation far ahead of ours cannot come from the peer of this pair.
	if epoch < st.Epoch {
		d.event("warning", "ignored a promote request with a stale epoch (%d < %d)", epoch, st.Epoch)
		writeJSON(w, 409, map[string]string{"error": "stale epoch"})
		return
	}
	if epoch > st.Epoch+maxEpochAhead {
		d.event("warning", "ignored a promote request with an implausible epoch (%d, ours is %d)", epoch, st.Epoch)
		writeJSON(w, 400, map[string]string{"error": "implausible epoch"})
		return
	}
	// a request that arrives after its validity ran out was delayed (frozen peer, queued connection): the sender has given up on it;
	// one whose validity is far in the future is not one this software sends (it would stay executable for a long time)
	exp := time.UnixMilli(ms)
	if time.Now().After(exp.Add(reqSkew)) {
		d.event("warning", "ignored an expired promote request (it was valid until %s)", exp.UTC().Format(time.RFC3339))
		writeJSON(w, 409, map[string]string{"error": "request expired"})
		return
	}
	if exp.After(time.Now().Add(reqTTL + reqSkew)) {
		d.event("warning", "ignored a promote request whose validity is too long (until %s)", exp.UTC().Format(time.RFC3339))
		writeJSON(w, 400, map[string]string{"error": "validity too long"})
		return
	}
	switch s := d.tx.begin(tid); s {
	case "":
	case txDone: // the same request delivered again: it already happened
		writeJSON(w, 200, map[string]any{"status": "active", "replay": true})
		return
	default:
		writeJSON(w, 409, map[string]string{"error": "transition " + s, "tx": s})
		return
	}
	if d.store.Get().State == StateActive {
		d.tx.finish(tid, nil)
		writeJSON(w, 409, map[string]string{"error": "already active", "state": StateActive})
		return
	}
	t0 := time.Now()
	err = d.doPromote("peer request, transition " + tid[:8] + ", sender epoch " + strconv.FormatUint(epoch, 10))
	d.tx.finish(tid, err)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"status": "active", "ms": time.Since(t0).Milliseconds()})
}

// handleAbort: the node that asked us to promote did not get an answer. Cancel the transition unless it already started
// and report what happened, together with our role.
func (d *Daemon) handleAbort(w http.ResponseWriter, r *http.Request) {
	tid := r.URL.Query().Get("tid")
	if !tidRe.MatchString(tid) {
		writeJSON(w, 400, map[string]string{"error": "malformed transition id"})
		return
	}
	s := d.tx.abort(tid)
	if s == txAborted {
		d.auto.holdUntil.Set(time.Now().Add(abortHold))
	}
	st := d.store.Get()
	writeJSON(w, 200, map[string]any{"tid": tid, "tx": s, "state": st.State, "epoch": st.Epoch, "busy": d.opsBusy()})
}

// ---- requester side ----

type txOutcome int

const (
	outcomeUnknown       txOutcome = iota // the peer could not be asked, or is still busy: do NOT take the role back
	outcomePeerActive                     // the peer is active: we are the standby
	outcomePeerStandby                    // the peer did not and will not take over: taking the role back is safe
	outcomePeerAmbiguous                  // peer says the transition ran but it is not active: manual decision
)

// resolveTransition asks the peer what became of tid until the answer is conclusive or the window is over.
func (d *Daemon) resolveTransition(tid string, sentAt time.Time) (txOutcome, string) {
	every, window := resolveTiming()
	deadline := time.Now().Add(window)
	last := "no answer from the peer"
	for {
		out, err := d.callPeer("/v1/abort?tid="+url.QueryEscape(tid), 5*time.Second)
		if err == nil {
			state, _ := out["state"].(string)
			tx, _ := out["tx"].(string)
			last = "peer state " + state + ", transition " + tx
			switch {
			case state == StateActive:
				return outcomePeerActive, last
			case tx == txAborted || tx == txFailed:
				if state == StateStandby {
					return outcomePeerStandby, last
				}
			case tx == txDone:
				return outcomePeerAmbiguous, last
			}
		} else {
			last = err.Error()
			// nobody answers AND no heartbeat arrives on any path: the peer is down or cut off, which the automatic rules (isolation
			// guard, fencing) handle. Waiting longer only delays them, but only once its late request can no longer be executed.
			if alive, _, _ := d.hb.Snapshot(); !alive && time.Since(sentAt) > reqTTL+reqSkew+time.Second {
				return outcomeUnknown, last + "; silent on every heartbeat path and its request has expired"
			}
		}
		if time.Now().After(deadline) {
			return outcomeUnknown, last
		}
		time.Sleep(every)
	}
}

// ---- seams for the tests ----

// actions lets the tests replace what touches the system and the network. Nil fields mean the real thing.
type actions struct {
	promote    func(reason string) error
	demoteHook func(reason string, afterDown func()) error
	peerCall   func(path string, timeout time.Duration) (map[string]any, error)
	sh         func(name string, args ...string) (string, error)
	plan       func(path string) (WarmPlan, error)
	flush      func(dev string)
	fence      func() error // replaces running the fencing command
}

func (d *Daemon) doPromote(reason string) error {
	if d.act != nil && d.act.promote != nil {
		return d.act.promote(reason)
	}
	return d.promote(reason)
}

func (d *Daemon) doDemote(reason string, afterDown func()) error {
	if d.act != nil && d.act.demoteHook != nil {
		return d.act.demoteHook(reason, afterDown)
	}
	return d.demoteHook(reason, afterDown)
}

func (d *Daemon) callPeer(path string, timeout time.Duration) (map[string]any, error) {
	if d.act != nil && d.act.peerCall != nil {
		return d.act.peerCall(path, timeout)
	}
	return d.peerCall(path, timeout)
}

var timingMu sync.RWMutex

func resolveTiming() (every, window time.Duration) {
	timingMu.RLock()
	defer timingMu.RUnlock()
	return resolveEvery, resolveWindow
}

// setResolveTiming changes the polling of the outcome of a transition (the tests shorten it) and returns the function that restores it.
func setResolveTiming(every, window time.Duration) (restore func()) {
	timingMu.Lock()
	oe, ow := resolveEvery, resolveWindow
	resolveEvery, resolveWindow = every, window
	timingMu.Unlock()
	return func() {
		timingMu.Lock()
		resolveEvery, resolveWindow = oe, ow
		timingMu.Unlock()
	}
}
