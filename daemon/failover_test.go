package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests drive the real failover and election logic with the system actions (interfaces, pf, the network) replaced by
// in-memory fakes. They prove the decision logic only; they are NOT a validation on OPNsense (see docs/validation.md).

func newHANode(t *testing.T, st State) *Daemon {
	t.Helper()
	return &Daemon{
		store: &Store{dir: t.TempDir(), data: st},
		ops:   &Ops{}, hb: newHeartbeat(), auto: &autoState{}, act: &actions{},
	}
}

func activeState(role string, epoch uint64) State {
	return State{Role: role, State: StateActive, PeerFP: "fp", PeerAddr: "10.0.0.2", PeerPort: 7690, Epoch: epoch}
}

func standbyState(role string, epoch uint64) State {
	return State{Role: role, State: StateStandby, PeerFP: "fp", PeerAddr: "10.0.0.1", PeerPort: 7690, Epoch: epoch}
}

// fastResolve shortens the polling of the transition outcome.
func fastResolve(t *testing.T) {
	t.Helper()
	oe, ow := resolveEvery, resolveWindow
	resolveEvery, resolveWindow = 2*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { resolveEvery, resolveWindow = oe, ow })
}

// fakeSystem installs promote/demote fakes that only change the stored state, and counts the calls.
type fakeSystem struct {
	promotes, demotes atomic.Int32
	promoteErr        error
}

func (f *fakeSystem) install(d *Daemon) {
	d.act.promote = func(string) error {
		f.promotes.Add(1)
		if f.promoteErr != nil {
			return f.promoteErr
		}
		return d.store.Update(func(s *State) { s.State = StateActive; s.Epoch++ })
	}
	d.act.demoteHook = func(_ string, after func()) error {
		f.demotes.Add(1)
		if after != nil {
			after()
		}
		return d.store.Update(func(s *State) { s.State = StateStandby })
	}
}

// deliver calls a peer endpoint of b in-process, like the HTTP layer would.
func deliver(b *Daemon, path string) (map[string]any, error) {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	rec := httptest.NewRecorder()
	switch {
	case strings.HasPrefix(path, "/v1/promote"):
		b.handlePromote(rec, req)
	case strings.HasPrefix(path, "/v1/abort"):
		b.handleAbort(rec, req)
	default:
		return nil, fmt.Errorf("unexpected path %s", path)
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK {
		return out, fmt.Errorf("peer answered %d: %v", rec.Code, out["error"])
	}
	return out, nil
}

// link connects a to b. dropRequest: the promote request never reaches b. dropReply: b executes it but its answer is lost.
// Every request is also appended to *sent so a test can deliver it late or twice.
func link(b *Daemon, dropRequest, dropReply bool, sent *[]string) func(string, time.Duration) (map[string]any, error) {
	var mu sync.Mutex
	return func(path string, _ time.Duration) (map[string]any, error) {
		if strings.HasPrefix(path, "/v1/promote") {
			mu.Lock()
			if sent != nil {
				*sent = append(*sent, path)
			}
			mu.Unlock()
			if dropRequest {
				return nil, errors.New("timeout: request lost")
			}
			out, err := deliver(b, path)
			if dropReply {
				return nil, errors.New("timeout: reply lost")
			}
			return out, err
		}
		return deliver(b, path)
	}
}

func seePeer(d *Daemon, m hbMsg) { d.hb.seen("ha", m) }

func silence(d *Daemon, ago time.Duration) {
	d.hb.mu.Lock()
	defer d.hb.mu.Unlock()
	t := time.Now().Add(-ago)
	d.hb.last = t
	d.hb.pathLast = map[string]time.Time{"ha": t}
}

func bothActive(a, b *Daemon) bool {
	return a.store.Get().State == StateActive && b.store.Get().State == StateActive
}

// ---- P0-A: a lost promote answer must never cause a rollback that creates two active nodes ----

func TestFailoverHappyPath(t *testing.T) {
	a := newHANode(t, activeState(RoleMaster, 7))
	b := newHANode(t, standbyState(RoleStandby, 6))
	var fa, fb fakeSystem
	fa.install(a)
	fb.install(b)
	a.act.peerCall = link(b, false, false, nil)
	seePeer(a, hbMsg{State: StateStandby})

	if _, err := a.failover(true); err != nil {
		t.Fatal(err)
	}
	if a.store.Get().State != StateStandby || b.store.Get().State != StateActive {
		t.Fatalf("a=%s b=%s", a.store.Get().State, b.store.Get().State)
	}
	if fa.promotes.Load() != 0 {
		t.Fatal("no rollback expected")
	}
}

func TestFailoverLostReplyPeerPromotedNoRollback(t *testing.T) {
	fastResolve(t)
	a := newHANode(t, activeState(RoleMaster, 7))
	b := newHANode(t, standbyState(RoleStandby, 6))
	var fa, fb fakeSystem
	fa.install(a)
	fb.install(b)
	a.act.peerCall = link(b, false, true, nil) // b promotes, its answer is lost
	seePeer(a, hbMsg{State: StateStandby})

	if _, err := a.failover(true); err != nil {
		t.Fatalf("the peer is active, the failover succeeded: %v", err)
	}
	if fa.promotes.Load() != 0 {
		t.Fatal("rolled back although the peer had taken over: two active nodes")
	}
	if bothActive(a, b) || a.store.Get().State != StateStandby || b.store.Get().State != StateActive {
		t.Fatalf("a=%s b=%s", a.store.Get().State, b.store.Get().State)
	}
}

func TestFailoverPeerUnreachableStaysStandby(t *testing.T) {
	fastResolve(t)
	a := newHANode(t, activeState(RoleMaster, 7))
	var fa fakeSystem
	fa.install(a)
	a.act.peerCall = func(string, time.Duration) (map[string]any, error) { return nil, errors.New("no route to host") }
	seePeer(a, hbMsg{State: StateStandby})

	if _, err := a.failover(true); err == nil {
		t.Fatal("an unverifiable hand-over must be reported as an error")
	}
	if fa.promotes.Load() != 0 {
		t.Fatal("rollback without knowing what the peer did")
	}
	if a.store.Get().State != StateStandby {
		t.Fatalf("a must stay standby, is %s", a.store.Get().State)
	}
}

func TestFailoverRequestLostPeerConfirmsRollbackAndLateDeliveryIsRejected(t *testing.T) {
	fastResolve(t)
	a := newHANode(t, activeState(RoleMaster, 7))
	b := newHANode(t, standbyState(RoleStandby, 6))
	var fa, fb fakeSystem
	fa.install(a)
	fb.install(b)
	var sent []string
	a.act.peerCall = link(b, true, false, &sent) // the request never arrives
	seePeer(a, hbMsg{State: StateStandby})

	if _, err := a.failover(true); err == nil {
		t.Fatal("the hand-over did not happen: error expected")
	}
	if fa.promotes.Load() != 1 {
		t.Fatalf("a confirmed-safe rollback must happen exactly once, got %d", fa.promotes.Load())
	}
	if a.store.Get().State != StateActive || b.store.Get().State != StateStandby {
		t.Fatalf("a=%s b=%s", a.store.Get().State, b.store.Get().State)
	}
	// the original request shows up late (delayed in a switch queue, retransmitted): it must be refused
	if len(sent) != 1 {
		t.Fatalf("expected one request, got %v", sent)
	}
	if _, err := deliver(b, sent[0]); err == nil {
		t.Fatal("a delayed promote for an aborted transition was accepted")
	}
	if fb.promotes.Load() != 0 || bothActive(a, b) {
		t.Fatalf("late request promoted the peer: a=%s b=%s", a.store.Get().State, b.store.Get().State)
	}
}

func TestFailoverPeerPromoteFailsRollbackIsSafe(t *testing.T) {
	fastResolve(t)
	a := newHANode(t, activeState(RoleMaster, 7))
	b := newHANode(t, standbyState(RoleStandby, 6))
	var fa, fb fakeSystem
	fa.install(a)
	fb.install(b)
	fb.promoteErr = errors.New("packet filter: pf is not enabled") // b cleans up and stays standby
	a.act.peerCall = link(b, false, false, nil)
	seePeer(a, hbMsg{State: StateStandby})

	if _, err := a.failover(true); err == nil {
		t.Fatal("error expected")
	}
	if fa.promotes.Load() != 1 || a.store.Get().State != StateActive || b.store.Get().State != StateStandby {
		t.Fatalf("service must come back on a: a=%s b=%s rollbacks=%d", a.store.Get().State, b.store.Get().State, fa.promotes.Load())
	}
}

func TestFailoverPeerStillPromotingIsWaitedFor(t *testing.T) {
	fastResolve(t)
	a := newHANode(t, activeState(RoleMaster, 7))
	var fa fakeSystem
	fa.install(a)
	var polls atomic.Int32
	a.act.peerCall = func(path string, _ time.Duration) (map[string]any, error) {
		if strings.HasPrefix(path, "/v1/promote") {
			return nil, errors.New("timeout")
		}
		if polls.Add(1) < 4 { // promotion still running on the peer
			return map[string]any{"tx": txStarted, "state": StateStandby}, nil
		}
		return map[string]any{"tx": txDone, "state": StateActive}, nil
	}
	seePeer(a, hbMsg{State: StateStandby})
	if _, err := a.failover(true); err != nil {
		t.Fatal(err)
	}
	if fa.promotes.Load() != 0 || polls.Load() < 4 {
		t.Fatalf("rollbacks=%d polls=%d", fa.promotes.Load(), polls.Load())
	}
}

func TestFailoverAmbiguousPeerAnswerNeverRollsBack(t *testing.T) {
	fastResolve(t)
	for name, answer := range map[string]map[string]any{
		"transition ran but peer is not active": {"tx": txDone, "state": StateStandby},
		"transition still running at the end":   {"tx": txStarted, "state": StateStandby},
		"garbage":                               {},
	} {
		t.Run(name, func(t *testing.T) {
			a := newHANode(t, activeState(RoleMaster, 7))
			var fa fakeSystem
			fa.install(a)
			a.act.peerCall = func(path string, _ time.Duration) (map[string]any, error) {
				if strings.HasPrefix(path, "/v1/promote") {
					return nil, errors.New("timeout")
				}
				return answer, nil
			}
			seePeer(a, hbMsg{State: StateStandby})
			if _, err := a.failover(true); err == nil || fa.promotes.Load() != 0 {
				t.Fatalf("err=%v rollbacks=%d", err, fa.promotes.Load())
			}
		})
	}
}

func TestFailoverAlreadyActiveAnswerCountsAsDone(t *testing.T) {
	a := newHANode(t, activeState(RoleMaster, 7))
	var fa fakeSystem
	fa.install(a)
	a.act.peerCall = func(string, time.Duration) (map[string]any, error) {
		return nil, errors.New("peer answered 409: already active")
	}
	seePeer(a, hbMsg{State: StateStandby})
	if _, err := a.failover(true); err != nil || fa.promotes.Load() != 0 {
		t.Fatalf("err=%v rollbacks=%d", err, fa.promotes.Load())
	}
}

func TestFailoverDemoteFailureBeforeAnythingWasAskedLeavesPeerAlone(t *testing.T) {
	a := newHANode(t, activeState(RoleMaster, 7))
	var asked atomic.Int32
	a.act.demoteHook = func(string, func()) error { return errors.New("no HA settings") } // refuses before the interfaces go down
	a.act.peerCall = func(string, time.Duration) (map[string]any, error) { asked.Add(1); return nil, nil }
	a.act.promote = func(string) error { t.Error("no rollback expected"); return nil }
	seePeer(a, hbMsg{State: StateStandby})
	if _, err := a.failover(true); err == nil || asked.Load() != 0 {
		t.Fatalf("err=%v asked=%d", err, asked.Load())
	}
}

// ---- peer side of the protocol ----

func TestPromoteRequestIsIdempotentPerTransition(t *testing.T) {
	b := newHANode(t, standbyState(RoleStandby, 6))
	var fb fakeSystem
	fb.install(b)
	tid := newTID()
	path := "/v1/promote?tid=" + tid + "&epoch=7"
	if _, err := deliver(b, path); err != nil {
		t.Fatal(err)
	}
	out, err := deliver(b, path) // duplicate
	if err != nil || out["replay"] != true {
		t.Fatalf("duplicate must be acknowledged as a replay: %v %v", out, err)
	}
	if fb.promotes.Load() != 1 {
		t.Fatalf("promoted %d times", fb.promotes.Load())
	}
}

func TestStaleEpochPromoteIsRefused(t *testing.T) {
	b := newHANode(t, standbyState(RoleStandby, 9))
	var fb fakeSystem
	fb.install(b)
	if _, err := deliver(b, "/v1/promote?tid="+newTID()+"&epoch=7"); err == nil {
		t.Fatal("a command from an older leadership generation was obeyed")
	}
	if fb.promotes.Load() != 0 {
		t.Fatal("promoted on a stale command")
	}
}

func TestMalformedTransitionIdIsRefused(t *testing.T) {
	b := newHANode(t, standbyState(RoleStandby, 6))
	var fb fakeSystem
	fb.install(b)
	for _, p := range []string{"/v1/promote?tid=zz", "/v1/promote?tid=../../etc", "/v1/abort?tid=", "/v1/abort?tid=ABC"} {
		if _, err := deliver(b, p); err == nil {
			t.Fatalf("%s accepted", p)
		}
	}
	if fb.promotes.Load() != 0 {
		t.Fatal("promoted")
	}
}

func TestConcurrentPromoteOfTheSameTransitionRunsOnce(t *testing.T) {
	b := newHANode(t, standbyState(RoleStandby, 6))
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	b.act.promote = func(string) error {
		calls.Add(1)
		close(started)
		<-release
		return b.store.Update(func(s *State) { s.State = StateActive; s.Epoch++ })
	}
	path := "/v1/promote?tid=" + newTID() + "&epoch=7"
	first := make(chan error, 1)
	go func() { _, err := deliver(b, path); first <- err }()
	<-started
	if _, err := deliver(b, path); err == nil {
		t.Fatal("a second execution of the same transition was allowed while the first one runs")
	}
	// while it runs, an abort must see it as started and must not cancel it
	out, _ := deliver(b, "/v1/abort?tid="+strings.TrimPrefix(strings.Split(path, "&")[0], "/v1/promote?tid="))
	if out["tx"] != txStarted {
		t.Fatalf("abort during a running promotion: %v", out)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("executed %d times", calls.Load())
	}
}

func TestAbortAfterCompletionReportsDoneAndRole(t *testing.T) {
	b := newHANode(t, standbyState(RoleStandby, 6))
	var fb fakeSystem
	fb.install(b)
	tid := newTID()
	deliver(b, "/v1/promote?tid="+tid+"&epoch=7")
	out, err := deliver(b, "/v1/abort?tid="+tid)
	if err != nil || out["tx"] != txDone || out["state"] != StateActive {
		t.Fatalf("%v %v", out, err)
	}
}

func TestAbortHoldsBackThePeersOwnElection(t *testing.T) {
	b := newHANode(t, standbyState(RoleMaster, 6))
	deliver(b, "/v1/abort?tid="+newTID())
	if !time.Now().Before(b.auto.holdUntil.Get()) {
		t.Fatal("after cancelling a hand-over the peer must not elect itself while the requester takes its role back")
	}
}

func TestTransitionLogIsBounded(t *testing.T) {
	var l txLog
	for i := 0; i < txKeep*3; i++ {
		l.begin(newTID())
	}
	if len(l.m) > txKeep || len(l.order) > txKeep {
		t.Fatalf("log grew to %d", len(l.m))
	}
}

// ---- P0-D: operations in the wrong order or at the same time ----

func TestOnlyOneSwitchoverAtATime(t *testing.T) {
	a := newHANode(t, activeState(RoleMaster, 7))
	b := newHANode(t, standbyState(RoleStandby, 6))
	var fa, fb fakeSystem
	fa.install(a)
	fb.install(b)
	var inFlight, maxInFlight atomic.Int32
	a.act.peerCall = func(path string, to time.Duration) (map[string]any, error) {
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		defer inFlight.Add(-1)
		return link(b, false, false, nil)(path, to)
	}
	seePeer(a, hbMsg{State: StateStandby})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); a.failover(true) }()
	}
	wg.Wait()
	if maxInFlight.Load() > 1 {
		t.Fatalf("%d hand-overs ran in parallel", maxInFlight.Load())
	}
	if bothActive(a, b) {
		t.Fatal("two active nodes")
	}
}

func TestElectionPausedWhileAFailoverRuns(t *testing.T) {
	carrier = func(string) bool { return true }
	t.Cleanup(func() { carrier = carrierUp })
	d := newHANode(t, standbyState(RoleMaster, 5))
	var promoted atomic.Int32
	d.act.promote = func(string) error { promoted.Add(1); return nil }
	d.failing.Store(true)
	silence(d, time.Hour)
	d.auto.lastChange = time.Time{}
	d.autoStep()
	if promoted.Load() != 0 {
		t.Fatal("automatic promotion while this node is orchestrating a failover")
	}
}

// ---- P0-B / P0-C: split-brain and loss of communication ----

func TestSplitBrainIsResolvedDespiteTheAntiFlappingHold(t *testing.T) {
	d := newHANode(t, activeState(RoleStandby, 5))
	var f fakeSystem
	f.install(d)
	seePeer(d, hbMsg{State: StateActive, Epoch: 6}) // the peer has the higher epoch
	d.auto.lastChange = time.Now()                  // a role change a moment ago: minHold must not delay the resolution

	d.autoStep() // first sight of the conflict: confirmation window starts
	d.auto.dualSince = time.Now().Add(-5 * time.Second)
	d.autoStep()
	if f.demotes.Load() != 1 || d.store.Get().State != StateStandby {
		t.Fatalf("demotes=%d state=%s", f.demotes.Load(), d.store.Get().State)
	}
}

func TestSplitBrainWinnerKeepsTheRole(t *testing.T) {
	cases := []struct {
		name         string
		mine, theirs uint64
		role         string
		wantDemote   bool
	}{
		{"higher epoch wins", 9, 6, RoleStandby, false},
		{"lower epoch loses", 5, 6, RoleMaster, true},
		{"tie: master wins", 6, 6, RoleMaster, false},
		{"tie: secondary loses", 6, 6, RoleStandby, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newHANode(t, activeState(c.role, c.mine))
			var f fakeSystem
			f.install(d)
			seePeer(d, hbMsg{State: StateActive, Epoch: c.theirs})
			d.autoStep()
			d.auto.dualSince = time.Now().Add(-5 * time.Second)
			d.autoStep()
			if got := f.demotes.Load() == 1; got != c.wantDemote {
				t.Fatalf("demoted=%v want %v", got, c.wantDemote)
			}
		})
	}
}

func TestSplitBrainResolutionIgnoresAutoFailoverSwitchAndMaintenance(t *testing.T) {
	off := false
	d := newHANode(t, activeState(RoleStandby, 5))
	d.cfg.AutoFailover = &off
	d.store.Update(func(s *State) { s.Maintenance = true })
	var f fakeSystem
	f.install(d)
	seePeer(d, hbMsg{State: StateActive, Epoch: 6})
	d.autoStep()
	d.auto.dualSince = time.Now().Add(-5 * time.Second)
	d.autoStep()
	if f.demotes.Load() != 1 {
		t.Fatal("a double active must be resolved even with automatic failover off and in maintenance mode")
	}
}

func TestSplitBrainBusyPeerGetsAGraceButNotForever(t *testing.T) {
	d := newHANode(t, activeState(RoleStandby, 5))
	var f fakeSystem
	f.install(d)
	seePeer(d, hbMsg{State: StateActive, Epoch: 6, Busy: true})
	d.autoStep()
	d.auto.dualSince = time.Now().Add(-5 * time.Second)
	d.autoStep()
	if f.demotes.Load() != 0 {
		t.Fatal("a peer in the middle of a transition is excused for a moment")
	}
	d.auto.dualBusySince = time.Now().Add(-2 * sbBusyMax) // it has been "busy" for too long: stuck
	d.autoStep()
	d.auto.dualSince = time.Now().Add(-5 * time.Second)
	d.autoStep()
	if f.demotes.Load() != 1 {
		t.Fatal("a stuck 'busy' flag must not keep a double active alive")
	}
}

func TestSplitBrainStandDownIsRetriedAtARateLimit(t *testing.T) {
	d := newHANode(t, activeState(RoleStandby, 5))
	var calls atomic.Int32
	d.act.demoteHook = func(string, func()) error { calls.Add(1); return errors.New("disk full") }
	seePeer(d, hbMsg{State: StateActive, Epoch: 6})
	d.autoStep()
	d.auto.dualSince = time.Now().Add(-5 * time.Second)
	d.autoStep()
	d.autoStep()
	d.autoStep()
	if calls.Load() != 1 {
		t.Fatalf("hammered the failing demote %d times", calls.Load())
	}
	d.auto.lastSBTry = time.Now().Add(-2 * sbRetry)
	d.autoStep()
	if calls.Load() != 2 {
		t.Fatalf("no retry after the delay: %d", calls.Load())
	}
}

func TestNoSplitBrainActionWhenOnlyOneNodeIsActive(t *testing.T) {
	d := newHANode(t, activeState(RoleMaster, 5))
	var f fakeSystem
	f.install(d)
	seePeer(d, hbMsg{State: StateStandby, Epoch: 5})
	d.autoStep()
	d.auto.dualSince = time.Time{}
	d.autoStep()
	if f.demotes.Load() != 0 {
		t.Fatal("A active, B standby: nothing to resolve")
	}
}

func standbyWithSilentPeer(t *testing.T, silent time.Duration, haCarrier bool) (*Daemon, *atomic.Int32) {
	t.Helper()
	carrier = func(string) bool { return haCarrier }
	t.Cleanup(func() { carrier = carrierUp })
	d := newHANode(t, standbyState(RoleStandby, 5))
	d.cfg.HADevice = "ha0"
	var promoted atomic.Int32
	d.act.promote = func(string) error {
		promoted.Add(1)
		return d.store.Update(func(s *State) { s.State = StateActive })
	}
	seePeer(d, hbMsg{State: StateActive, Epoch: 5})
	silence(d, silent)
	return d, &promoted
}

func TestPeerDownIsPromotedAfterTheDetectionTime(t *testing.T) {
	d, promoted := standbyWithSilentPeer(t, hbDead+time.Second, true)
	d.autoStep()
	if promoted.Load() != 1 {
		t.Fatal("a real failure with a healthy standby must fail over")
	}
}

func TestStandbyDownActiveKeepsTheTraffic(t *testing.T) {
	d := newHANode(t, activeState(RoleMaster, 5))
	var f fakeSystem
	f.install(d)
	silence(d, time.Hour) // the standby vanished
	d.autoStep()
	if f.demotes.Load() != 0 || d.store.Get().State != StateActive {
		t.Fatal("the active node must keep serving when the standby is gone")
	}
}

func TestLatencyAndLossBelowTheDetectionTimeDoNotFailOver(t *testing.T) {
	d, promoted := standbyWithSilentPeer(t, hbDead/2, true) // several heartbeats lost, not enough to declare it dead
	d.autoStep()
	if promoted.Load() != 0 {
		t.Fatal("intempestive failover on a short loss")
	}
}

func TestIsolatedStandbyDoesNotPromote(t *testing.T) {
	// the peer is silent but our own HA link has no carrier: we may be the isolated one
	d, promoted := standbyWithSilentPeer(t, hbDead+time.Second, false)
	d.autoStep()
	if promoted.Load() != 0 {
		t.Fatal("promoted without proof that this node is not the isolated one")
	}
	if d.auto.Blocked() == "" {
		t.Fatal("the reason must be visible to the administrator")
	}
}

func TestAntiFlappingHoldDelaysAutomaticPromotion(t *testing.T) {
	d, promoted := standbyWithSilentPeer(t, hbDead+time.Second, true)
	d.auto.lastChange = time.Now()
	d.autoStep()
	if promoted.Load() != 0 {
		t.Fatal("a role change just happened: promotion must wait")
	}
}

func TestMaintenanceModePausesAutomaticPromotion(t *testing.T) {
	d, promoted := standbyWithSilentPeer(t, hbDead+time.Second, true)
	d.store.Update(func(s *State) { s.Maintenance = true })
	d.autoStep()
	if promoted.Load() != 0 {
		t.Fatal("maintenance mode must pause automatic failover")
	}
}

func TestAutoFailoverOffNeverPromotes(t *testing.T) {
	off := false
	d, promoted := standbyWithSilentPeer(t, hbDead+time.Second, true)
	d.cfg.AutoFailover = &off
	d.autoStep()
	if promoted.Load() != 0 {
		t.Fatal("automatic failover is switched off")
	}
}

func TestElectionAfterBothBootedAndHold(t *testing.T) {
	for name, hold := range map[string]bool{"elects the master": false, "hold after an aborted hand-over": true} {
		t.Run(name, func(t *testing.T) {
			d := newHANode(t, standbyState(RoleMaster, 5))
			var promoted atomic.Int32
			d.act.promote = func(string) error { promoted.Add(1); return d.store.Update(func(s *State) { s.State = StateActive }) }
			seePeer(d, hbMsg{State: StateStandby, Epoch: 5})
			d.auto.noActiveSince = time.Now().Add(-time.Minute)
			if hold {
				d.auto.holdUntil.Set(time.Now().Add(time.Minute))
			}
			d.autoStep()
			want := int32(1)
			if hold {
				want = 0
			}
			if promoted.Load() != want {
				t.Fatalf("promoted=%d hold=%v", promoted.Load(), hold)
			}
		})
	}
}

func TestPeerBusyPostponesTheElection(t *testing.T) {
	d := newHANode(t, standbyState(RoleMaster, 5))
	var promoted atomic.Int32
	d.act.promote = func(string) error { promoted.Add(1); return nil }
	seePeer(d, hbMsg{State: StateStandby, Epoch: 5, Busy: true}) // the peer is taking over right now
	d.auto.noActiveSince = time.Now().Add(-time.Minute)
	d.autoStep()
	if promoted.Load() != 0 {
		t.Fatal("raced a promotion in progress on the peer")
	}
}

func TestReturningOldActiveIsNotPromotedBack(t *testing.T) {
	// the old active comes back as standby while the peer is active: it must stay standby (no preemption configured)
	d := newHANode(t, standbyState(RoleMaster, 5))
	var promoted atomic.Int32
	d.act.promote = func(string) error { promoted.Add(1); return nil }
	seePeer(d, hbMsg{State: StateActive, Epoch: 8})
	for i := 0; i < 3; i++ {
		d.autoStep()
	}
	if promoted.Load() != 0 {
		t.Fatal("the returning node took the role back on its own")
	}
}

// ---- helpers of the promotion hardening ----

func TestPfStatusParsing(t *testing.T) {
	if !pfEnabled("Status: Enabled for 0 days 00:10:01           Debug: Urgent\n\nState Table  Total\n") {
		t.Fatal("enabled not recognised")
	}
	for _, s := range []string{"", "Status: Disabled for 0 days 00:00:05", "garbage", "Status:"} {
		if pfEnabled(s) {
			t.Fatalf("%q reported as enabled", s)
		}
	}
}

func TestConfigctlFailureDetection(t *testing.T) {
	for _, s := range []string{"Error: filter reload failed", "Execute error", "Action not found"} {
		if !configctlFailed(s) {
			t.Fatalf("%q not detected", s)
		}
	}
	for _, s := range []string{"OK", "", "reloading filter\nOK"} {
		if configctlFailed(s) {
			t.Fatalf("%q reported as failure", s)
		}
	}
}

func TestPhaseLogRecordsInOrder(t *testing.T) {
	p := &phaseLog{t0: time.Now()}
	p.mark("a")
	time.Sleep(2 * time.Millisecond)
	p.mark("b")
	l := p.list()
	if len(l) != 2 || l[0].Name != "a" || l[1].Name != "b" || l[1].Ms < l[0].Ms {
		t.Fatalf("%+v", l)
	}
}

func TestDegradedIsAccumulatedAndClearable(t *testing.T) {
	d := newHANode(t, activeState(RoleMaster, 1))
	d.markDegraded("routes: %s", "x")
	d.markDegraded("dns")
	if r := d.degradedReason(); !strings.Contains(r, "routes: x") || !strings.Contains(r, "dns") {
		t.Fatalf("%q", r)
	}
	d.degraded.Store("")
	if d.degradedReason() != "" {
		t.Fatal("not cleared")
	}
}

// ---- late delivery and recovery time (found with the sandbox: lab/sandbox, scenarios 8 and 14) ----

func TestExpiredPromoteRequestIsRefused(t *testing.T) {
	b := newHANode(t, standbyState(RoleStandby, 6))
	var fb fakeSystem
	fb.install(b)
	late := time.Now().Add(-time.Minute).UnixMilli() // sat in a frozen peer's socket buffer for a minute
	if _, err := deliver(b, fmt.Sprintf("/v1/promote?tid=%s&epoch=7&exp=%d", newTID(), late)); err == nil {
		t.Fatal("a request whose validity ran out was executed")
	}
	if fb.promotes.Load() != 0 || b.store.Get().State != StateStandby {
		t.Fatal("the late request promoted the node")
	}
	// within the allowed clock difference it is still valid, and a request without validity (older peers) keeps working
	edge := time.Now().Add(-reqSkew / 2).UnixMilli()
	if _, err := deliver(b, fmt.Sprintf("/v1/promote?tid=%s&epoch=7&exp=%d", newTID(), edge)); err != nil {
		t.Fatalf("a request just past its time but within the skew must be accepted: %v", err)
	}
	b2 := newHANode(t, standbyState(RoleStandby, 6))
	var fb2 fakeSystem
	fb2.install(b2)
	if _, err := deliver(b2, "/v1/promote?tid="+newTID()+"&epoch=7"); err != nil {
		t.Fatal(err)
	}
}

func shortRequestValidity(t *testing.T) {
	t.Helper()
	oldTTL, oldSkew := reqTTL, reqSkew
	reqTTL, reqSkew = 5*time.Millisecond, 0
	t.Cleanup(func() { reqTTL, reqSkew = oldTTL, oldSkew })
}

func TestSilentUnreachablePeerEndsTheWaitEarly(t *testing.T) {
	fastResolve(t)
	shortRequestValidity(t)
	resolveWindow = 5 * time.Second // the window itself is long: the early exit must not wait for it
	a := newHANode(t, activeState(RoleMaster, 7))
	a.act.peerCall = func(string, time.Duration) (map[string]any, error) { return nil, errors.New("no route to host") }
	silence(a, time.Hour) // no heartbeat on any path
	t0 := time.Now()
	out, detail := a.resolveTransition(newTID(), time.Now().Add(-5*time.Second)) // the request was sent long enough ago to have expired
	if out != outcomeUnknown || time.Since(t0) > time.Second {
		t.Fatalf("outcome %v after %v (%s): a dead peer must not keep the survivor waiting for the whole window", out, time.Since(t0), detail)
	}
}

func TestSilentPeerIsNotGivenUpOnBeforeItsRequestExpired(t *testing.T) {
	fastResolve(t)
	// default validity (10 s + 5 s): a request sent just now may still be executed by a peer that comes back
	resolveWindow = 300 * time.Millisecond
	a := newHANode(t, activeState(RoleMaster, 7))
	a.act.peerCall = func(string, time.Duration) (map[string]any, error) { return nil, errors.New("timeout") }
	silence(a, time.Hour)
	t0 := time.Now()
	out, _ := a.resolveTransition(newTID(), time.Now())
	if out != outcomeUnknown || time.Since(t0) < resolveWindow {
		t.Fatalf("returned after %v: the wait must last for the window while the request can still be executed", time.Since(t0))
	}
}

func TestAnAnsweringButSlowPeerKeepsBeingAsked(t *testing.T) {
	fastResolve(t)
	shortRequestValidity(t)
	resolveWindow = 200 * time.Millisecond
	a := newHANode(t, activeState(RoleMaster, 7))
	var polls atomic.Int32
	a.act.peerCall = func(string, time.Duration) (map[string]any, error) { polls.Add(1); return nil, errors.New("timeout") }
	seePeer(a, hbMsg{State: StateStandby}) // heartbeats arrive: the peer is alive, only this call fails
	t0 := time.Now()
	out, _ := a.resolveTransition(newTID(), time.Now().Add(-time.Minute))
	if out != outcomeUnknown || time.Since(t0) < resolveWindow || polls.Load() < 3 {
		t.Fatalf("a live peer must be asked until the window is over: %v after %v, %d polls", out, time.Since(t0), polls.Load())
	}
}
