package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Scenarios of the audit brief on the model of the pair (property_test.go: fake interface actions, the shared identity tracked).

// A partition that cuts both heartbeat paths while each node still sees its own links up. Without an external arbiter the standby
// takes the silent peer for dead: the identity is on both nodes until a path comes back, then the split-brain rule leaves one. With
// fence_required and no fencing command, the standby refuses and the identity is never doubled. This is the documented limit, shown.
func TestPartitionOfBothPaths(t *testing.T) {
	old := carrier
	carrier = func(string) bool { return true } // each node sees its own cables up
	t.Cleanup(func() { carrier = old })
	for _, fenceRequired := range []bool{false, true} {
		m := newPairModel(t)
		m.b.cfg.FenceRequired = fenceRequired
		seePeer(m.b, hbMsg{State: StateActive})
		silence(m.b, hbDead+time.Second) // the partition: nothing from A for longer than the detection time
		m.b.autoStep()
		both := m.exposed[m.a].Load() && m.exposed[m.b].Load()
		if fenceRequired && both {
			t.Fatal("fence_required: the standby must not take over a silent peer without fencing")
		}
		if !fenceRequired && !both {
			t.Fatal("without fencing the standby takes over a silent peer: the documented limit (if this changes, update docs/audit.md)")
		}
		// the partition heals: both see each other; the split-brain rule leaves exactly one
		if both {
			m.resolveSplitBrain()
		}
		m.check(1, "after the partition healed")
		if len(m.actives()) != 1 {
			t.Fatalf("fence_required=%v: %d active nodes after the partition", fenceRequired, len(m.actives()))
		}
	}
}

// Both nodes restart at the same time and both come back standby (a paired node always does). The election lets the master go first;
// the secondary waits much longer and sees the master active by then: one active node, never two.
func TestSimultaneousRestart(t *testing.T) {
	m := newPairModel(t)
	for _, d := range m.nodes() {
		m.exposed[d].Store(false)
		d.store.Update(func(s *State) { s.State = StateStandby })
	}
	past := time.Now().Add(-(electionMaster + time.Second)) // longer than the master waits, much shorter than the secondary
	for _, d := range m.nodes() {
		seePeer(d, hbMsg{State: StateStandby, Role: m.other(d).store.Get().Role})
		d.auto.noActiveSince = past
	}
	m.a.autoStep() // master
	m.b.autoStep() // secondary, at the same moment
	m.check(1, "simultaneous restart")
	if !m.exposed[m.a].Load() || m.exposed[m.b].Load() {
		t.Fatalf("the master must be the only one active: A=%v B=%v", m.exposed[m.a].Load(), m.exposed[m.b].Load())
	}
	seePeer(m.b, hbMsg{State: StateActive})
	m.b.auto.noActiveSince = time.Now().Add(-time.Hour)
	m.b.autoStep()
	m.check(2, "the secondary sees the master active")
}

// A configuration push that arrives while the node is being promoted is applied before (the operations lock serialises them) or
// refused after: never applied to a node that is no longer standby, and its files are left as they are.
func TestConfigApplyRefusedOnceTheNodeIsActive(t *testing.T) {
	d := newHANode(t, activeState(RoleStandby, 6))
	before := []byte("<opnsense><x>old</x></opnsense>")
	os.WriteFile(activeXML(d.store.dir), before, 0o600)
	err := d.applyConfig([]byte("<opnsense><x>new</x></opnsense>"))
	if err == nil {
		t.Fatal("a configuration must not be applied on an active node")
	}
	if b, _ := os.ReadFile(activeXML(d.store.dir)); string(b) != string(before) {
		t.Fatal("the saved configuration changed")
	}
	if _, err := os.Stat(filepath.Join(d.store.dir, "incoming.xml")); err == nil {
		t.Fatal("nothing must be written")
	}
}
