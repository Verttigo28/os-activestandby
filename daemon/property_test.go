package main

import (
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Property test of the role protocol. Two daemons, the system actions replaced by fakes (as in failover_test.go), and a long seeded
// random sequence of the events that happen to a pair: graceful failovers with lost requests and lost answers, a node that restarts
// (a paired node always boots as standby), an operator who promotes the standby by hand while the other is active, nobody active.
// After every step the safety properties must hold:
//
//	S1  the shared IP and MAC are on at most one node (what the fake interface actions did, not what the state file says) when a step has
//	    finished, the split-brain resolution included; and a node that has them believes it is active;
//	S2  the epoch of a node never decreases;
//
// It proves the decision logic under many interleavings, not the behaviour on real interfaces (see docs/validation.md).

type pairModel struct {
	t    *testing.T
	a, b *Daemon
	// exposed: the node really has the shared addresses and MAC on its interfaces (what the fake system actions did), not what its
	// state file says. The invariant is about this.
	exposed map[*Daemon]*atomic.Bool
	epoch   map[*Daemon]uint64
}

func newPairModel(t *testing.T) *pairModel {
	reliableDisk(t)
	m := &pairModel{t: t, epoch: map[*Daemon]uint64{}, exposed: map[*Daemon]*atomic.Bool{}}
	m.a = newHANode(t, activeState(RoleMaster, 5))
	m.b = newHANode(t, standbyState(RoleStandby, 5))
	for i, d := range m.nodes() {
		e := &atomic.Bool{}
		e.Store(i == 0) // A starts as the active node: it has the addresses
		m.exposed[d] = e
		d := d
		d.act.promote = func(string) error {
			e.Store(true) // the interfaces take the shared addresses and MAC
			return d.store.Update(func(s *State) { s.State = StateActive; s.Epoch++ })
		}
		d.act.demoteHook = func(_ string, after func()) error {
			e.Store(false) // the data interfaces go down and lose the addresses
			if after != nil {
				after()
			}
			return d.store.Update(func(s *State) { s.State = StateStandby })
		}
	}
	return m
}

func (m *pairModel) nodes() []*Daemon { return []*Daemon{m.a, m.b} }

func (m *pairModel) other(d *Daemon) *Daemon {
	if d == m.a {
		return m.b
	}
	return m.a
}

func (m *pairModel) name(d *Daemon) string {
	if d == m.a {
		return "A"
	}
	return "B"
}

func (m *pairModel) actives() []*Daemon {
	var out []*Daemon
	for _, d := range m.nodes() {
		if d.store.Get().State == StateActive {
			out = append(out, d)
		}
	}
	return out
}

func (m *pairModel) check(step int, what string) {
	m.t.Helper()
	if n := len(m.actives()); n > 1 {
		m.t.Fatalf("step %d (%s): both nodes are active (A epoch %d, B epoch %d)", step, what, m.a.store.Get().Epoch, m.b.store.Get().Epoch)
	}
	// the invariant is about the network: the shared identity is on at most one node, and a node that has it believes it is active
	// (a node that believes it is standby while its interfaces still carry the identity is the situation the enforcer must remove)
	if m.exposed[m.a].Load() && m.exposed[m.b].Load() {
		m.t.Fatalf("step %d (%s): the shared IP and MAC are on both nodes at once", step, what)
	}
	for _, d := range m.nodes() {
		if m.exposed[d].Load() != (d.store.Get().State == StateActive) {
			m.t.Fatalf("step %d (%s): %s has the identity on its interfaces=%v but its state is %s", step, what, m.name(d), m.exposed[d].Load(), d.store.Get().State)
		}
	}
	for _, d := range m.nodes() {
		e := d.store.Get().Epoch
		if e < m.epoch[d] {
			m.t.Fatalf("step %d (%s): the epoch of %s went back from %d to %d", step, what, m.name(d), m.epoch[d], e)
		}
		m.epoch[d] = e
	}
}

// resolveSplitBrain runs the decision of both nodes that see each other active, like the election loop does.
func (m *pairModel) resolveSplitBrain() {
	for round := 0; round < 3 && len(m.actives()) > 1; round++ {
		for _, d := range m.nodes() {
			o := m.other(d)
			if d.store.Get().State != StateActive {
				continue
			}
			ost := o.store.Get()
			d.auto.dualSince = time.Now().Add(-time.Second) // the conflict has been seen for long enough
			d.auto.lastSBTry = time.Time{}
			d.splitBrainStep(d.store.Get(), true, hbMsg{Role: ost.Role, State: ost.State, Epoch: ost.Epoch})
			if len(m.actives()) <= 1 {
				return
			}
		}
	}
}

func TestRoleProtocolKeepsAtMostOneActiveNode(t *testing.T) {
	for seed := int64(1); seed <= 40; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			fastResolve(t)
			rng := rand.New(rand.NewSource(seed))
			m := newPairModel(t)
			m.check(0, "start")
			for step := 1; step <= 300; step++ {
				var what string
				switch act := m.actives(); {
				case len(act) == 0:
					// nobody is active (a failed hand-over, both restarted): the election of the booting pair, the master first
					what = "election"
					first := m.a
					if rng.Intn(4) == 0 {
						first = m.b
					}
					first.auto.lastChange = time.Time{}
					first.autoPromote("election", false)
					// the other node, a moment later, sees an active peer and stays what it is (the election only runs with nobody active)
				case rng.Intn(10) < 6:
					// graceful failover from the active node, each request or answer possibly lost
					x := act[0]
					dropReq, dropRep := rng.Intn(5) == 0, rng.Intn(5) == 0
					what = fmt.Sprintf("failover from %s (request lost %v, answer lost %v)", m.name(x), dropReq, dropRep)
					x.act.peerCall = link(m.other(x), dropReq, dropRep, nil)
					seePeer(x, hbMsg{State: StateStandby})
					x.failover(true)
				case rng.Intn(10) < 5:
					// a node restarts: a paired node always comes up as standby, whatever it was
					y := m.nodes()[rng.Intn(2)]
					what = fmt.Sprintf("%s restarts", m.name(y))
					m.exposed[y].Store(false) // the interfaces are down while it reboots, and a paired node comes back silent
					y.store.Update(func(s *State) { s.State = StateStandby })
				default:
					// an operator promotes the standby by hand while the other node is active: a double active that must resolve
					x := act[0]
					y := m.other(x)
					what = fmt.Sprintf("%s promoted by hand while %s is active", m.name(y), m.name(x))
					y.doPromote("manual")
					if len(m.actives()) > 1 {
						m.resolveSplitBrain()
					}
				}
				m.check(step, what)
			}
		})
	}
}

// lossyLink carries the requests of a to b with a seeded probability p of losing the request, p of losing the answer, and p/2 of
// delivering a request twice. Every promote request is also kept in *sent so that it can be delivered again later.
func lossyLink(b *Daemon, rng *rand.Rand, p float64, sent *[]string) func(string, time.Duration) (map[string]any, error) {
	return func(path string, _ time.Duration) (map[string]any, error) {
		isPromote := len(path) >= 11 && path[:11] == "/v1/promote"
		if isPromote {
			*sent = append(*sent, path)
		}
		if rng.Float64() < p {
			return nil, fmt.Errorf("timeout: request lost")
		}
		out, err := deliver(b, path)
		if rng.Float64() < p/2 {
			deliver(b, path) // a duplicate
		}
		if rng.Float64() < p {
			return nil, fmt.Errorf("timeout: answer lost")
		}
		return out, err
	}
}

// Loss of 10, 30 and 50 % of the requests and of the answers, duplicates, and the delivery of every promote request again after the
// hand-over is over (a request that sat in a queue): whatever happens, the two nodes are never active at the same time, and a
// request that was cancelled cannot execute later.
func TestFailoverUnderPacketLossAndLateDelivery(t *testing.T) {
	for _, p := range []float64{0.1, 0.3, 0.5} {
		t.Run(fmt.Sprintf("loss %.0f%%", p*100), func(t *testing.T) {
			fastResolve(t)
			rng := rand.New(rand.NewSource(int64(p * 1000)))
			m := newPairModel(t)
			outcomes := map[string]int{}
			for round := 1; round <= 60; round++ {
				act := m.actives()
				if len(act) == 0 { // the previous hand-over ended with nobody active: the election of the pair
					m.a.auto.lastChange = time.Time{}
					m.a.autoPromote("election", false)
					act = m.actives()
				}
				x := act[0]
				var sent []string
				x.act.peerCall = lossyLink(m.other(x), rng, p, &sent)
				seePeer(x, hbMsg{State: StateStandby})
				_, err := x.failover(true)
				switch {
				case err == nil:
					outcomes["done"]++
				default:
					outcomes["error"]++
				}
				m.check(round, "failover under loss")
				// late and repeated delivery of what was sent
				for _, path := range sent {
					deliver(m.other(x), path)
					deliver(m.other(x), path)
				}
				m.check(round, "late delivery of the requests")
			}
			if outcomes["done"] == 0 {
				t.Fatalf("not one hand-over succeeded at %.0f%% loss: %v", p*100, outcomes)
			}
			t.Logf("loss %.0f%%: %v", p*100, outcomes)
		})
	}
}

// reliableDisk: the model tests are about the decisions, not about the disk (state_fault_test.go does that): the state is accepted
// without touching the file system, so that a refused rename of the wasm test runner on Windows cannot look like a protocol failure.
func reliableDisk(t *testing.T) {
	t.Helper()
	old := fsWrite
	fsWrite = func(string, []byte, os.FileMode) error { return nil }
	t.Cleanup(func() { fsWrite = old })
}

// delayedLink carries each request of a to b in `delay` and its answer back in another `delay`; the caller gives up after
// `timeout`. A request that outlives the caller is still delivered later: the peer receives it after the requester has moved on.
func delayedLink(b *Daemon, delay, timeout time.Duration, inflight *sync.WaitGroup) func(string, time.Duration) (map[string]any, error) {
	type answer struct {
		out map[string]any
		err error
	}
	return func(path string, _ time.Duration) (map[string]any, error) {
		ch := make(chan answer, 1)
		inflight.Add(1)
		go func() {
			defer inflight.Done()
			time.Sleep(delay)
			out, err := deliver(b, path)
			time.Sleep(delay)
			ch <- answer{out, err}
		}()
		select {
		case a := <-ch:
			return a.out, a.err
		case <-time.After(timeout):
			return nil, fmt.Errorf("timeout after %s", timeout)
		}
	}
}

// Latency, on the scale of the timings of the protocol (all of them shortened by the same factor): an answer in time, an answer that
// comes after the requester gave up, and a request that arrives after its own validity ran out. Whatever the delay, once every message
// in flight has been delivered, the shared identity is on at most one node.
func TestFailoverUnderLatency(t *testing.T) {
	t.Cleanup(setResolveTiming(5*time.Millisecond, 400*time.Millisecond))
	t.Cleanup(setRequestValidity(100*time.Millisecond, 0))
	const timeout = 40 * time.Millisecond // how long the requester waits for one answer
	for _, delay := range []time.Duration{
		5 * time.Millisecond,   // well within everything
		30 * time.Millisecond,  // the request arrives in time, the answer after the requester gave up
		150 * time.Millisecond, // the request arrives after its validity: it must be refused
	} {
		t.Run(delay.String(), func(t *testing.T) {
			m := newPairModel(t)
			var inflight sync.WaitGroup
			outcomes := map[string]int{}
			for round := 1; round <= 12; round++ {
				act := m.actives()
				if len(act) == 0 {
					m.a.auto.lastChange = time.Time{}
					m.a.autoPromote("election", false)
					act = m.actives()
				}
				x := act[0]
				x.act.peerCall = delayedLink(m.other(x), delay, timeout, &inflight)
				seePeer(x, hbMsg{State: StateStandby})
				if _, err := x.failover(true); err == nil {
					outcomes["done"]++
				} else {
					outcomes["error"]++
				}
				inflight.Wait() // every late request and answer has arrived
				m.check(round, fmt.Sprintf("failover with a delay of %s", delay))
			}
			t.Logf("delay %s: %v", delay, outcomes)
		})
	}
}

// The hand-over leaves no goroutine behind: every failover above, with lost, late and duplicated messages, ends with as many
// goroutines as before (a leak in the transition code would grow with every failover of a firewall that runs for years).
func TestFailoversLeaveNoGoroutineBehind(t *testing.T) {
	fastResolve(t)
	m := newPairModel(t)
	settle := func() int {
		n := runtime.NumGoroutine()
		for i := 0; i < 50; i++ {
			time.Sleep(10 * time.Millisecond)
			if c := runtime.NumGoroutine(); c <= n {
				n = c
			}
		}
		return n
	}
	before := settle()
	rng := rand.New(rand.NewSource(5))
	for round := 0; round < 40; round++ {
		act := m.actives()
		if len(act) == 0 {
			m.a.auto.lastChange = time.Time{}
			m.a.autoPromote("election", false)
			continue
		}
		x := act[0]
		var sent []string
		x.act.peerCall = lossyLink(m.other(x), rng, 0.3, &sent)
		seePeer(x, hbMsg{State: StateStandby})
		x.failover(true)
	}
	if after := settle(); after > before+2 {
		t.Fatalf("%d goroutines before 40 failovers, %d after", before, after)
	}
}
