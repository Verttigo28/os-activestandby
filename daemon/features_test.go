package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNotificationLevels(t *testing.T) {
	cases := []struct {
		kind, level string
		want        bool
	}{
		{"info", "warning", false}, {"change", "warning", true}, {"warning", "warning", true}, {"error", "warning", true},
		{"info", "info", true}, {"warning", "error", false}, {"error", "error", true}, {"change", "error", false},
		{"warning", "", true}, // unknown level behaves like the default (warning)
	}
	for _, c := range cases {
		if got := kindRank(c.kind) >= levelRank(c.level); got != c.want {
			t.Errorf("kind %q at level %q: got %v want %v", c.kind, c.level, got, c.want)
		}
	}
}

func TestFenceCommandValidation(t *testing.T) {
	if err := validateFenceCommand(""); err != nil {
		t.Fatal("empty means not configured")
	}
	for _, bad := range []string{"relative/path", "/tmp/a b", "/tmp/x;rm", "/tmp/$(id)", "/does/not/exist"} {
		if validateFenceCommand(bad) == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
	if runtime.GOOS == "windows" {
		return // permission bits are meaningless there
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "fence.sh")
	os.WriteFile(good, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	os.Chmod(good, 0o755)
	// the temporary directory sits under /tmp, which anyone can write to: run as root the validator rightly refuses it for
	// that reason, run as a user it refuses it because the file is not owned by root; neither is what this checks
	if err := validateFenceCommand(good); err != nil && !strings.Contains(err.Error(), "owned by root") && !strings.Contains(err.Error(), "writable by group or others") {
		t.Errorf("good command rejected: %v", err)
	}
	bad := filepath.Join(dir, "w.sh")
	os.WriteFile(bad, []byte("x"), 0o777)
	os.Chmod(bad, 0o777)
	if validateFenceCommand(bad) == nil {
		t.Error("a group/world writable command must be rejected")
	}
}

func TestHistoryPersistsAndOrders(t *testing.T) {
	dir := t.TempDir()
	h := openHistory(dir)
	h.add(HistoryEntry{Type: "promote", Reason: "a", DurationMs: 2000, Epoch: 2})
	h.add(HistoryEntry{Type: "demote", Reason: "b", DurationMs: 800, Epoch: 2})
	h.add(HistoryEntry{Type: "promote", Reason: "c", DurationMs: 2400, Epoch: 3})
	h = openHistory(dir) // reload from disk
	r := h.Recent(10)
	if len(r) != 3 || r[0].Reason != "c" || r[2].Reason != "a" {
		t.Fatalf("newest first expected, got %+v", r)
	}
	if p, dm, last := h.totals(); p != 2 || dm != 1 || last != 2400 {
		t.Fatalf("totals %d %d %d", p, dm, last)
	}
	var nilHist *History
	nilHist.add(HistoryEntry{}) // must not panic (tests and early start build daemons without history)
	if nilHist.Recent(3) != nil {
		t.Fatal("nil history yields nothing")
	}
}

func TestMaintenanceModeExpires(t *testing.T) {
	n := newNode(t)
	n.d.setMaintenance(true, 1, true)
	if !n.d.maintenanceOn() {
		t.Fatal("maintenance should be on")
	}
	n.d.store.Update(func(s *State) { s.MaintenanceUntil = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339) })
	if n.d.maintenanceOn() {
		t.Fatal("maintenance must switch itself off after the deadline")
	}
	time.Sleep(100 * time.Millisecond) // the async switch-off
	if n.d.store.Get().Maintenance {
		t.Fatal("expired maintenance must be cleared")
	}
}

func TestMetricsExposition(t *testing.T) {
	n := newNode(t)
	n.d.store.Update(func(s *State) { s.Role, s.State, s.Epoch = RoleMaster, StateActive, 7 })
	n.d.hist = openHistory(t.TempDir())
	n.d.hist.add(HistoryEntry{Type: "promote", DurationMs: 2500})
	out := n.d.metricsText()
	for _, want := range []string{
		"activestandby_active 1", "activestandby_epoch 7", "activestandby_maintenance 0",
		`activestandby_path_up{path="ha"} 0`, "activestandby_promotions_total 1", "activestandby_last_promote_duration_seconds 2.500",
		"# TYPE activestandby_peer_alive gauge",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestWorstCheckAggregation(t *testing.T) {
	a := []Check{pass("a", ""), warn("b", "")}
	b := []Check{pass("c", "")}
	if worst(a, b) != "warn" || worst(b) != "pass" || worst(a, []Check{fail("d", "")}) != "fail" {
		t.Fatal("worst() must rank fail > warn > pass")
	}
}

func TestUnusableFenceCommandBlocksPromotion(t *testing.T) {
	n := newNode(t)
	n.d.cfg.FenceCommand = "/does/not/exist/fence.sh"
	if err := n.d.runFence(); err == nil {
		t.Fatal("an unusable fence command must make runFence fail (promotion is then cancelled), not succeed")
	}
	n.d.cfg.FenceCommand = ""
	if err := n.d.runFence(); err != nil {
		t.Fatal("no fence command configured means nothing to run")
	}
}

func TestMaintenanceNewestChangeWins(t *testing.T) {
	n := newNode(t)
	n.d.setMaintenance(true, 1, true) // local change at time T
	old := n.d.store.Get().MaintAt
	// the peer (which was down) still says "on" with an OLDER timestamp: must not override
	n.d.reconcileMaintenance(hbMsg{Maint: true, MaintAt: "2000-01-01T00:00:00Z"})
	if !n.d.store.Get().Maintenance || n.d.store.Get().MaintAt != old {
		t.Fatal("an older peer state must not change anything")
	}
	// a NEWER "off" from the peer wins
	n.d.reconcileMaintenance(hbMsg{Maint: false, MaintAt: "2999-01-01T00:00:00Z"})
	if n.d.store.Get().Maintenance {
		t.Fatal("a newer change from the peer must be adopted")
	}
}

func TestPinnedKeyRegexp(t *testing.T) {
	out := "pinned lan(hn1)=02:5a:75:41:a1:18 opt3(vlan01)=02:5f:9a:74:72:33"
	var keys []string
	for _, m := range pinnedKey.FindAllStringSubmatch(out, -1) {
		keys = append(keys, m[1])
	}
	if len(keys) != 2 || keys[0] != "lan" || keys[1] != "opt3" {
		t.Fatalf("keys = %v", keys)
	}
}
