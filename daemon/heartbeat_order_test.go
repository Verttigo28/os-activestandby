package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Heartbeats come over two paths at different speeds, and answers can be late: an older message must not overwrite a newer state of
// the peer, nor count as a newer proof of life.
func TestOlderHeartbeatDoesNotOverwriteANewerOne(t *testing.T) {
	h := newHeartbeat()
	const inc = 1000
	if !h.seen("ha", hbMsg{Inc: inc, Seq: 5, State: StateStandby}) {
		t.Fatal("first message refused")
	}
	// the peer was active a moment ago; that message (seq 4) arrives after the one that says standby (seq 5)
	if h.seen("mgmt", hbMsg{Inc: inc, Seq: 4, State: StateActive}) {
		t.Fatal("an older message was taken")
	}
	if _, _, p := h.Snapshot(); p.State != StateStandby {
		t.Fatalf("the peer state went back to %s", p.State)
	}
	if h.PathAlive("mgmt") {
		t.Fatal("an old message is no proof that the path works now")
	}
	if h.seen("ha", hbMsg{Inc: inc, Seq: 5, State: StateActive}) {
		t.Fatal("a duplicate was taken")
	}
	if !h.seen("mgmt", hbMsg{Inc: inc, Seq: 6, State: StateStandby}) || !h.PathAlive("mgmt") {
		t.Fatal("a newer message must be taken")
	}
	if h.stale != 2 {
		t.Fatalf("stale messages counted: %d", h.stale)
	}
}

func TestRestartedPeerStartsAgain(t *testing.T) {
	h := newHeartbeat()
	h.seen("ha", hbMsg{Inc: 1000, Seq: 900, State: StateActive})
	// the peer daemon restarted: a new incarnation, its sequence starts again
	if !h.seen("ha", hbMsg{Inc: 2000, Seq: 1, State: StateStandby}) {
		t.Fatal("the first message of a restarted peer was refused")
	}
	if _, _, p := h.Snapshot(); p.State != StateStandby {
		t.Fatal("not taken")
	}
}

func TestPeerWithoutIncarnationIsTakenAsBefore(t *testing.T) {
	h := newHeartbeat()
	h.seen("ha", hbMsg{Seq: 9, State: StateActive})
	if !h.seen("ha", hbMsg{Seq: 3, State: StateStandby}) {
		t.Fatal("plugin 1.0.3 and older send no incarnation: every message is taken, as before")
	}
}

// this node sends its own incarnation and an increasing sequence
func TestLocalHeartbeatCarriesItsOrder(t *testing.T) {
	d := newHANode(t, activeState(RoleMaster, 3))
	d.health = newHealthState()
	a, b := d.localHB(), d.localHB()
	if a.Inc == 0 || a.Inc != b.Inc || b.Seq <= a.Seq {
		t.Fatalf("incarnation %d/%d, sequence %d then %d", a.Inc, b.Inc, a.Seq, b.Seq)
	}
	if time.Since(time.Unix(0, a.Inc)) > time.Minute {
		t.Fatal("the incarnation is the start of this daemon")
	}
}

// Reported by the audit of ba21e4d: once the restarted peer has been heard, a late message of its PREVIOUS incarnation must not be
// taken (it carries the state of before the restart).
func TestLateMessageOfThePreviousIncarnationIsRefused(t *testing.T) {
	h := newHeartbeat()
	h.seen("ha", hbMsg{Inc: 1000, Seq: 900, State: StateActive})        // before the restart: active
	h.seen("ha", hbMsg{Inc: 2000, Seq: 1, State: StateStandby})         // the restarted peer
	if h.seen("mgmt", hbMsg{Inc: 1000, Seq: 901, State: StateActive}) { // late, over the other path
		t.Fatal("a message of the previous incarnation was taken after the new one")
	}
	if _, _, p := h.Snapshot(); p.State != StateStandby || p.Inc != 2000 {
		t.Fatalf("the peer state went back to %s (incarnation %d)", p.State, p.Inc)
	}
	if !h.seen("ha", hbMsg{Inc: 2000, Seq: 2, State: StateStandby}) {
		t.Fatal("the new incarnation must keep being taken")
	}
}

// The clock of the peer was set back and its daemon restarted without its saved counter: its new incarnation is LOWER. It is taken
// once the previous incarnation has been silent long enough that no late message of it can still arrive, and not before.
func TestLowerIncarnationAfterAClockSetBack(t *testing.T) {
	h := newHeartbeat()
	h.seen("ha", hbMsg{Inc: 5000, Seq: 10, State: StateActive})
	if h.seen("ha", hbMsg{Inc: 3000, Seq: 1, State: StateStandby}) {
		t.Fatal("taken while the current incarnation was heard a moment ago (it could be a late message)")
	}
	h.mu.Lock()
	h.last = time.Now().Add(-staleWindow - time.Millisecond) // the previous incarnation has been silent since
	h.mu.Unlock()
	if !h.seen("ha", hbMsg{Inc: 3000, Seq: 1, State: StateStandby}) {
		t.Fatal("the restarted peer must be taken once its previous run is silent")
	}
	if !h.seen("ha", hbMsg{Inc: 3000, Seq: 2, State: StateStandby}) {
		t.Fatal("the new run must keep being taken")
	}
}

// the incarnation of this daemon increases from one run to the next even when the clock went back
func TestIncarnationIncreasesAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	a := nextIncarnation(dir)
	future := time.Now().Add(24 * time.Hour).UnixNano() // the previous run had a clock one day ahead
	os.WriteFile(filepath.Join(dir, "incarnation"), []byte(strconv.FormatInt(future, 10)), 0o600)
	b := nextIncarnation(dir)
	if b <= future || b <= a {
		t.Fatalf("incarnations %d then %d after a saved %d", a, b, future)
	}
	if c := nextIncarnation(dir); c <= b {
		t.Fatalf("%d after %d", c, b)
	}
	// a state directory that cannot be written: the clock alone, no failure
	if nextIncarnation(filepath.Join(dir, "missing", "dir")) == 0 {
		t.Fatal("no incarnation")
	}
}
