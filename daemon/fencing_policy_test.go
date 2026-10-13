package main

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

// The fencing policy of an automatic takeover. A silent peer may still be alive: what may be done about it is a configuration
// choice (a fencing command, and whether a takeover without one is allowed), and each combination must behave the same way every time.

type fenceCase struct {
	name     string
	command  string // FenceCommand
	required bool   // FenceRequired
	fenceErr error  // what the fencing command does
	silent   bool   // the takeover is caused by a silent peer (fence=true) rather than by an election
	promote  bool   // must the node take over?
	fenced   bool   // must the fencing command run?
	blocked  string // part of the reason shown to the operator, "" = none
}

func TestFencingPolicyOfAnAutomaticTakeover(t *testing.T) {
	for _, tc := range []fenceCase{
		{name: "silent peer, no fencing, not required: takes over (documented risk)", silent: true, promote: true},
		{name: "silent peer, fencing succeeds: takes over after fencing", command: "/usr/local/bin/fence", silent: true, promote: true, fenced: true},
		{name: "silent peer, fencing fails: does not take over", command: "/usr/local/bin/fence", fenceErr: errors.New("ipmi unreachable"), silent: true, fenced: true, blocked: "fencing failed"},
		{name: "silent peer, fencing required, none configured: does not take over", required: true, silent: true, blocked: "fencing is required"},
		{name: "silent peer, fencing required and configured, succeeds: takes over", command: "/usr/local/bin/fence", required: true, silent: true, promote: true, fenced: true},
		{name: "silent peer, fencing required and configured, fails: does not take over", command: "/usr/local/bin/fence", required: true, fenceErr: errors.New("timeout"), silent: true, fenced: true, blocked: "fencing failed"},
		// the peer answers (nobody is active after a boot): nothing to fence, the policy about silence does not apply
		{name: "election of a booting pair, fencing required and none configured: still takes over", required: true, silent: false, promote: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newHANode(t, standbyState(RoleStandby, 6))
			d.cfg.FenceCommand, d.cfg.FenceRequired = tc.command, tc.required
			var fenced, promoted atomic.Int32
			d.act.fence = func() error { fenced.Add(1); return tc.fenceErr }
			d.act.promote = func(string) error { promoted.Add(1); return nil }
			d.autoPromote("test", tc.silent)
			if (promoted.Load() == 1) != tc.promote {
				t.Fatalf("promoted %d times, want promotion=%v", promoted.Load(), tc.promote)
			}
			if (fenced.Load() == 1) != tc.fenced {
				t.Fatalf("fencing ran %d times, want %v", fenced.Load(), tc.fenced)
			}
			if tc.fenced && tc.fenceErr == nil && !tc.promote {
				t.Fatal("fencing succeeded but the node did not take over")
			}
			if got := d.auto.Blocked(); tc.blocked == "" && got != "" || tc.blocked != "" && !strings.Contains(got, tc.blocked) {
				t.Fatalf("operator message %q, want it to contain %q", got, tc.blocked)
			}
		})
	}
}

// a takeover that is refused for lack of fencing is refused again at every following step, and the reason stays visible
func TestRefusedTakeoverStaysRefusedAndQuiet(t *testing.T) {
	d := newHANode(t, standbyState(RoleStandby, 6))
	d.cfg.FenceRequired = true
	var promoted atomic.Int32
	d.act.promote = func(string) error { promoted.Add(1); return nil }
	for i := 0; i < 10; i++ {
		d.autoPromote("peer lost", true)
	}
	if promoted.Load() != 0 {
		t.Fatal("took over without fencing although it is required")
	}
	if !strings.Contains(d.auto.Blocked(), "fencing is required") {
		t.Fatalf("not shown to the operator: %q", d.auto.Blocked())
	}
}
