package main

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The daemon of the active node stops (killed, hung, stopped by hand) while OPNsense keeps forwarding with the shared IP and MAC.
// The standby hears nothing on any path. If it took the silence for a dead peer, the identity would be on both nodes. The system of
// the peer still answers ping on the HA link or on management: then it is alive, and the standby must not take over by itself.
func TestStandbyDoesNotTakeOverAPeerWhoseSystemStillAnswers(t *testing.T) {
	old := carrier
	carrier = func(string) bool { return true }
	t.Cleanup(func() { carrier = old })
	for _, tc := range []struct {
		name    string
		answers bool   // the system of the peer answers ping
		fence   string // a fencing command is configured
		want    bool   // the standby takes over
	}{
		{"daemon dead, system alive, no fencing: no takeover", true, "", false},
		{"daemon dead, system alive, fencing configured: fence then take over", true, "/usr/local/bin/fence", true},
		{"system dead (no answer): take over as before", false, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newPairModel(t)
			m.b.cfg.FenceCommand = tc.fence
			m.b.act.fence = func() error {
				m.exposed[m.a].Store(false)
				m.a.store.Update(func(s *State) { s.State = StateStandby })
				return nil
			} // power off
			m.b.act.ping = func(addr string) bool { return tc.answers }
			if !tc.answers { // the whole machine is down: it holds nothing any more
				m.exposed[m.a].Store(false)
				m.a.store.Update(func(s *State) { s.State = StateStandby })
			}
			seePeer(m.b, hbMsg{State: StateActive})
			silence(m.b, hbDead+time.Second)
			m.b.autoStep()
			took := m.exposed[m.b].Load()
			if took != tc.want {
				t.Fatalf("took over = %v, want %v (blocked: %q)", took, tc.want, m.b.auto.Blocked())
			}
			if m.exposed[m.a].Load() && m.exposed[m.b].Load() {
				t.Fatal("the shared identity is on both nodes")
			}
			if !tc.want && !strings.Contains(m.b.auto.Blocked(), "answers") {
				t.Fatalf("the reason must be shown: %q", m.b.auto.Blocked())
			}
		})
	}
}

func TestArpResolvedReadsTheKernelsAnswer(t *testing.T) {
	for out, want := range map[string]bool{
		"? (10.99.0.1) at 0c:c4:7a:1:2:3 on igb0 expires in 1197 seconds [ethernet]": true,
		"? (10.99.0.1) at 0c:c4:7a:aa:bb:cc on igb0 permanent [ethernet]":            true,
		"? (10.99.0.1) at (incomplete) on igb0 expired [ethernet]":                   false,
		"10.99.0.1 (10.99.0.1) -- no entry":                                          false,
		"":                                                                           false,
		"? (10.99.0.1) at zz:zz:zz:zz:zz:zz on igb0":                                 false,
	} {
		if arpResolved(out) != want {
			t.Errorf("%q: want %v", out, want)
		}
	}
}

// Asked by the audit of 09281e5: the probe of the peer system with ICMP filtered, with the HA paths cut, and with a node that is only
// partly reachable.
func TestPeerSystemProbeScenarios(t *testing.T) {
	old := carrier
	carrier = func(string) bool { return true }
	t.Cleanup(func() { carrier = old })
	for _, tc := range []struct {
		name      string
		icmp      map[string]bool // address -> answers ping
		arp       bool            // the kernel of the peer answers ARP on the HA link
		wantAfter bool            // the standby takes over
	}{
		{"the daemon is dead, ICMP answers on the HA link: no takeover", map[string]bool{"10.99.0.1": true}, true, false},
		{"ICMP filtered on the peer, its kernel still answers ARP: no takeover", map[string]bool{}, true, false},
		{"only the management address answers (the HA cable is cut): no takeover", map[string]bool{"192.168.225.212": true}, false, false},
		{"only the HA link answers (management is down): no takeover", map[string]bool{"10.99.0.1": true}, true, false},
		{"nothing answers anywhere: the peer is down or cut off, takeover as before", map[string]bool{}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newPairModel(t)
			m.b.store.Update(func(s *State) { s.PeerAddr, s.PeerMgmt = "10.99.0.1", "192.168.225.212" })
			m.b.act.ping = func(addr string) bool { return tc.icmp[addr] }
			m.b.act.arp = func(addr string) bool { return tc.arp && addr == "10.99.0.1" }
			if tc.wantAfter {
				m.exposed[m.a].Store(false)
				m.a.store.Update(func(s *State) { s.State = StateStandby })
			}
			seePeer(m.b, hbMsg{State: StateActive})
			silence(m.b, hbDead+time.Second)
			m.b.autoStep()
			if took := m.exposed[m.b].Load(); took != tc.wantAfter {
				t.Fatalf("took over = %v, want %v (blocked: %q)", took, tc.wantAfter, m.b.auto.Blocked())
			}
			m.check(1, tc.name)
		})
	}
}

// the step runs twice a second while the peer is silent: the probe (which deletes the ARP entry of the peer on the HA link) must not
// run at every step
func TestProbeOfThePeerSystemIsRateLimited(t *testing.T) {
	d := newHANode(t, standbyState(RoleStandby, 5))
	d.store.Update(func(s *State) { s.PeerAddr = "10.99.0.1" })
	var pings atomic.Int32
	d.act.ping = func(string) bool { pings.Add(1); return true }
	st := d.store.Get()
	for i := 0; i < 20; i++ {
		if d.peerSystemAnswers(st) != "10.99.0.1" {
			t.Fatal("the answer is reused")
		}
	}
	if pings.Load() != 1 {
		t.Fatalf("%d probes in 20 steps", pings.Load())
	}
	d.auto.probeAt = time.Now().Add(-probeEvery - time.Second)
	d.peerSystemAnswers(st)
	if pings.Load() != 2 {
		t.Fatal("after the delay it probes again")
	}
}
