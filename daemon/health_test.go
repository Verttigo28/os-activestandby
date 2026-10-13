package main

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseLinkState(t *testing.T) {
	cases := []struct {
		name, out         string
		wantUp, wantKnown bool
	}{
		{"up and running", "ixl1: flags=1008843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST,LOWER_UP> metric 0 mtu 1500\n\tstatus: active\n", true, true},
		{"administratively down but the NIC keeps its link (ixl)", "ixl1: flags=1008c02<BROADCAST,DRV_OACTIVE,SIMPLEX,MULTICAST,LOWER_UP> metric 0 mtu 1500\n\tstatus: active\n", true, true},
		{"up, cable unplugged: a real failure", "igb0: flags=8843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST> metric 0 mtu 1500\n\tstatus: no carrier\n", false, true},
		{"administratively down, no carrier: not looked at (a standby port, a VLAN on a LAGG that is down)", "vlan0.10: flags=8802<BROADCAST,SIMPLEX,MULTICAST> metric 0 mtu 1500\n\tstatus: no carrier\n", false, false},
		{"no output", "", false, false},
	}
	for _, c := range cases {
		if up, known := parseLinkState(c.out); up != c.wantUp || known != c.wantKnown {
			t.Errorf("%s: up=%v known=%v, want %v %v", c.name, up, known, c.wantUp, c.wantKnown)
		}
	}
}

// the active node lost the carrier of a monitored interface: does it hand over?
func healthSwitchOutcome(t *testing.T, peer hbMsg) (switched bool, events string) {
	t.Helper()
	d := newHANode(t, activeState(RoleMaster, 7))
	d.health = newHealthState()
	d.ev = &Events{path: t.TempDir() + "/ev.log"}
	var demotes atomic.Int32
	d.act.demoteHook = func(_ string, after func()) error {
		demotes.Add(1)
		if after != nil {
			after()
		}
		return d.store.Update(func(s *State) { s.State = StateStandby })
	}
	d.act.peerCall = func(string, time.Duration) (map[string]any, error) { return map[string]any{"status": "active"}, nil }
	seePeer(d, peer)
	d.healthSwitch([]string{"link:vlan0.10"}, map[string]string{"link:vlan0.10": "no carrier on lan (vlan0.10)"})
	for i := 0; i < 50 && demotes.Load() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	var b strings.Builder
	for _, e := range d.ev.Recent(20) {
		b.WriteString(e.Msg + " | ")
	}
	return demotes.Load() > 0, b.String()
}

func TestHealthSwitchWithStandbyEvidence(t *testing.T) {
	standby := func(m hbMsg) hbMsg { m.State = StateStandby; return m }
	cases := []struct {
		name string
		peer hbMsg
		want bool
	}{
		{"standby reads link up on the same device", standby(hbMsg{Links: map[string]bool{"vlan0.10": true}}), true},
		{"standby READ no link on its port: the outage would only move", standby(hbMsg{Links: map[string]bool{"vlan0.10": false}}), false},
		// the layout that motivated this: every network is a VLAN on a LAGG, the standby keeps LAGG and VLANs down and cannot read them
		{"standby cannot read the link (port kept down): switch anyway", standby(hbMsg{LinksUnknown: []string{"vlan0.10"}}), true},
		{"older standby that reports false for the device: the previous behaviour is kept", standby(hbMsg{Links: map[string]bool{"vlan0.10": false}}), false},
		{"standby says nothing about the device: treated as unknown", standby(hbMsg{Links: map[string]bool{}}), true},
		{"standby itself unhealthy", standby(hbMsg{Links: map[string]bool{"vlan0.10": true}, Health: "no carrier on wan"}), false},
		{"standby cannot reach its management probe targets either", standby(hbMsg{Links: map[string]bool{"vlan0.10": true}, MgmtProbe: "fail"}), false},
		{"peer is not a standby", hbMsg{State: StateActive, Links: map[string]bool{"vlan0.10": true}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ev := healthSwitchOutcome(t, c.peer)
			if got != c.want {
				t.Fatalf("switched=%v want %v (%s)", got, c.want, ev)
			}
			if c.name == "standby cannot read the link (port kept down): switch anyway" && !strings.Contains(ev, "cannot read the link") {
				t.Fatalf("the missing confirmation must be logged: %s", ev)
			}
		})
	}
}

func TestHealthSwitchIsLimitedByTheCooldown(t *testing.T) {
	d := newHANode(t, activeState(RoleMaster, 7))
	d.health = newHealthState()
	d.ev = &Events{path: t.TempDir() + "/ev.log"}
	var demotes atomic.Int32
	d.act.demoteHook = func(_ string, after func()) error {
		demotes.Add(1)
		after()
		return d.store.Update(func(s *State) { s.State = StateActive }) // it comes back active every time: worst case for ping-pong
	}
	d.act.peerCall = func(string, time.Duration) (map[string]any, error) { return nil, errors.New("no") }
	d.act.promote = func(string) error { return nil }
	seePeer(d, hbMsg{State: StateStandby, LinksUnknown: []string{"vlan0.10"}})
	for i := 0; i < 4; i++ {
		d.healthSwitch([]string{"link:vlan0.10"}, map[string]string{"link:vlan0.10": "x"})
		time.Sleep(40 * time.Millisecond)
	}
	if demotes.Load() != 1 {
		t.Fatalf("%d switches within the cool-down: a flapping card must not make the roles ping-pong", demotes.Load())
	}
}

func TestLinkLossIsActedOnSoonerThanServices(t *testing.T) {
	if got := holdFor("link:lagg0", 6*time.Second); got != 2*time.Second {
		t.Fatalf("a link lost for good: %v, want 2s", got)
	}
	if got := holdFor("link:lagg0", 30*time.Second); got != 10*time.Second {
		t.Fatalf("a long configured hold keeps a third of it: %v", got)
	}
	if got := holdFor("service:ipsec", 6*time.Second); got != 6*time.Second {
		t.Fatalf("services keep the whole hold: %v", got)
	}
}
