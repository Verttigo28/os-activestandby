package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func bootAt(t *testing.T, d *Daemon, ago time.Duration) {
	t.Helper()
	oldTime, oldPath := bootTime, bootMarkerPath
	sec := strconv.FormatInt(time.Now().Add(-ago).Unix(), 10)
	bootTime = func() string { return sec }
	bootMarkerPath = filepath.Join(d.store.dir, "boot.marker")
	t.Cleanup(func() { bootTime, bootMarkerPath = oldTime, oldPath })
}

// Reported by the review of 11372fc: when the standby path of the boot failed (full or read-only /conf, a PHP error), the daemon
// believed the "active" on disk. A paired node never comes up active by itself: at the first start of a boot it goes to standby.
func TestFirstStartOfABootThatSkippedTheStandbyPath(t *testing.T) {
	d := nodeOnDisk(t, activeState(RoleMaster, 7), nil, "")
	var r recorder
	r.install(d, nil)
	bootAt(t, d, time.Minute)
	d.checkBoot()
	if d.store.Get().State != StateStandby || r.views != 1 {
		t.Fatalf("state %s, standby views %d", d.store.Get().State, r.views)
	}
	// the boot is now checked: a later restart of the daemon (the node was promoted again meanwhile) trusts its role
	d.store.Update(func(s *State) { s.State = StateActive })
	d.checkBoot()
	if d.store.Get().State != StateActive {
		t.Fatal("a restart of the daemon later in the same boot must not take the node down")
	}
}

// the daemon of a node that has been running for long is restarted (an upgrade of the plugin that predates the boot marker): that
// is not the start of a boot, the active node stays active
func TestRestartLongAfterTheBootIsNotABoot(t *testing.T) {
	d := nodeOnDisk(t, activeState(RoleMaster, 7), nil, "")
	var r recorder
	r.install(d, nil)
	bootAt(t, d, 3*time.Hour)
	d.checkBoot()
	if d.store.Get().State != StateActive || r.views != 0 {
		t.Fatal("an active node must not be taken down by a restart of its daemon")
	}
}

// the marker written by the standby path of the boot is honoured
func TestBootThatWentThroughTheStandbyPath(t *testing.T) {
	d := nodeOnDisk(t, activeState(RoleMaster, 7), nil, "")
	var r recorder
	r.install(d, nil)
	bootAt(t, d, time.Minute)
	markBootHandled()
	d.checkBoot()
	if d.store.Get().State != StateActive || r.views != 0 {
		t.Fatal("this boot was handled")
	}
}

// Reported by the review: after a restart of this daemon while the peer is silent, a takeover took max(own, heartbeat)+1 although
// the node knows a higher epoch from the leader's pushes; its next graceful failover was then refused as stale.
func TestTakeoverEpochIsAboveTheKnownLeader(t *testing.T) {
	d := newHANode(t, standbyState(RoleStandby, 6))
	d.store.Update(func(s *State) { s.LeaderEpoch = 40 })
	d.commitActive()
	if e := d.store.Get().Epoch; e != 41 {
		t.Fatalf("epoch %d, want 41", e)
	}
}

// Reported by the review: maintenance timestamps were compared as strings; RFC3339Nano drops trailing zeros, so ".1Z" sorted after
// ".15Z" and the older change won.
func TestMaintenanceChangesAreOrderedAsTimes(t *testing.T) {
	d := newHANode(t, standbyState(RoleStandby, 6))
	d.store.Update(func(s *State) { s.Maintenance, s.MaintAt = false, "2026-10-14T08:00:05.1Z" })
	d.reconcileMaintenance(hbMsg{Maint: true, MaintUntil: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), MaintAt: "2026-10-14T08:00:05.15Z"})
	if !d.store.Get().Maintenance {
		t.Fatal("the change of the peer is newer (.15 s after .1 s) and must be adopted")
	}
	d.reconcileMaintenance(hbMsg{Maint: false, MaintAt: "2026-10-14T08:00:05.12Z"})
	if !d.store.Get().Maintenance {
		t.Fatal("an older change must not be adopted")
	}
}

// Reported by the review: the window in which an update may write the configuration of a standby lived in memory only; the restart
// of the daemon by the upgrade of this plugin locked the configuration in the middle of the update.
func TestUnlockWindowSurvivesARestart(t *testing.T) {
	d := newHANode(t, standbyState(RoleStandby, 6))
	until := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	d.setUnlocked(until)
	again := newHANode(t, d.store.Get())
	again.store = d.store
	again.restoreUnlocked()
	if !again.unlockedUntil.Get().Equal(until) {
		t.Fatalf("window %v, want %v", again.unlockedUntil.Get(), until)
	}
	d.setUnlocked(time.Time{})
	third := newHANode(t, d.store.Get())
	third.store = d.store
	third.restoreUnlocked()
	if !third.unlockedUntil.Get().IsZero() {
		t.Fatal("a closed window must stay closed")
	}
}

// Reported by the review: different HA interface keys on the two nodes put the active's HA rules on another interface of the
// standby. Pairing refuses it.
func TestPairingRefusesDifferentInterfaceKeys(t *testing.T) {
	a := Info{Version: "26.7", Packages: map[string]string{}, HAKey: "opt1"}
	b := Info{Version: "26.7", Packages: map[string]string{}, HAKey: "opt2"}
	if d := a.Compare(b); len(d) == 0 || !strings.Contains(strings.Join(d, " "), "HA link") {
		t.Fatalf("diffs %v", d)
	}
	b.HAKey = "opt1"
	if d := a.Compare(b); len(d) != 0 {
		t.Fatalf("same keys: %v", d)
	}
	b.HAKey = "" // a peer of 1.0.4 that does not send its key: no conclusion
	if d := a.Compare(b); len(d) != 0 {
		t.Fatalf("unknown key: %v", d)
	}
}

func TestSettingKeyIsReadFromTheConfiguration(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.xml")
	os.WriteFile(p, []byte("<opnsense><OPNsense><ActiveStandby version=\"1\"><general><ha_interface>opt3</ha_interface><mgmt_interface/></general></ActiveStandby></OPNsense></opnsense>"), 0o600)
	if k := settingKey(p, "ha_interface"); k != "opt3" {
		t.Fatalf("%q", k)
	}
	if k := settingKey(p, "mgmt_interface"); k != "" {
		t.Fatalf("%q", k)
	}
}
