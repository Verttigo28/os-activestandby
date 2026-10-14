package main

import (
	"path/filepath"
	"sync/atomic"
	"testing"
)

// A transition that this node refused or ran must still be known after the daemon restarts: the abort that cancelled a request, and the
// request itself arriving late (a connection that was queued, a retry) would otherwise run on the new process, while the node that sent
// it has taken its role back.
func TestACancelledTransitionStaysCancelledAfterARestart(t *testing.T) {
	b := newHANode(t, standbyState(RoleStandby, 5))
	b.tx.attach(filepath.Join(b.store.dir, "transactions.json"))
	var promoted atomic.Int32
	b.act.promote = func(string) error { promoted.Add(1); return nil }
	tid := newTID()
	if out, err := deliver(b, "/v1/abort?tid="+tid); err != nil || out["tx"] != txAborted {
		t.Fatalf("abort: %v %v", out, err)
	}
	// the daemon restarts: a new process with the same state directory
	b2 := newHANode(t, standbyState(RoleStandby, 5))
	b2.act.promote = b.act.promote
	b2.tx.attach(filepath.Join(b.store.dir, "transactions.json"))
	if _, err := deliver(b2, promoteURL(tid, 5)); err == nil {
		t.Fatal("the late request of a cancelled transition was accepted by the restarted daemon")
	}
	if promoted.Load() != 0 {
		t.Fatalf("the restarted daemon promoted itself %d time(s) for a transition that was cancelled", promoted.Load())
	}
}

func TestATransitionThatRanIsNotRunAgainAfterARestart(t *testing.T) {
	b := newHANode(t, standbyState(RoleStandby, 5))
	b.tx.attach(filepath.Join(b.store.dir, "transactions.json"))
	var promoted atomic.Int32
	b.act.promote = func(string) error { promoted.Add(1); return nil }
	tid := newTID()
	if _, err := deliver(b, promoteURL(tid, 5)); err != nil {
		t.Fatal(err)
	}
	b2 := newHANode(t, standbyState(RoleStandby, 5)) // the state says standby again (it was demoted and restarted meanwhile)
	b2.act.promote = b.act.promote
	b2.tx.attach(filepath.Join(b.store.dir, "transactions.json"))
	out, err := deliver(b2, promoteURL(tid, 5))
	if err != nil || out["replay"] != true {
		t.Fatalf("the same request again must be recognised: %v %v", out, err)
	}
	if promoted.Load() != 1 {
		t.Fatalf("promoted %d times", promoted.Load())
	}
}

func TestATornTransactionFileIsIgnored(t *testing.T) {
	b := newHANode(t, standbyState(RoleStandby, 5))
	p := filepath.Join(b.store.dir, "transactions.json")
	writeFileDurable(p, []byte(`{"order":["abc`), 0o600)
	b.tx.attach(p) // must not panic, and starts empty
	if s := b.tx.begin(newTID()); s != "" {
		t.Fatalf("a fresh transition was refused: %q", s)
	}
}
