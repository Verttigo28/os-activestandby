package main

import (
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
