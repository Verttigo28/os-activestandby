package main

import (
	"strings"
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
