package main

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// Property test of the role protocol. Two daemons, the system actions replaced by fakes (as in failover_test.go), and a long seeded
// random sequence of the events that happen to a pair: graceful failovers with lost requests and lost answers, a node that restarts
// (a paired node always boots as standby), an operator who promotes the standby by hand while the other is active, nobody active.
// After every step the safety properties must hold:
//
//	S1  at most one node is active when a step has finished (the split-brain resolution included);
//	S2  the epoch of a node never decreases;
//
// It proves the decision logic under many interleavings, not the behaviour on real interfaces (see docs/validation.md).

type pairModel struct {
	t      *testing.T
	a, b   *Daemon
	fa, fb fakeSystem
	epoch  map[*Daemon]uint64
}

func newPairModel(t *testing.T) *pairModel {
	m := &pairModel{t: t, epoch: map[*Daemon]uint64{}}
	m.a = newHANode(t, activeState(RoleMaster, 5))
	m.b = newHANode(t, standbyState(RoleStandby, 5))
	m.fa.install(m.a)
	m.fb.install(m.b)
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
	for _, seed := range []int64{1, 2, 3, 7, 42, 1000} {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			fastResolve(t)
			rng := rand.New(rand.NewSource(seed))
			m := newPairModel(t)
			m.check(0, "start")
			for step := 1; step <= 150; step++ {
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
