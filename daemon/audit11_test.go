package main

import (
	"errors"
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

// kern.boottime cannot be read: no conclusion about the boot (an active node is not taken down for a restart of its daemon), and it
// is said
func TestBootTimeUnreadable(t *testing.T) {
	d := nodeOnDisk(t, activeState(RoleMaster, 7), nil, "")
	d.ev = &Events{path: filepath.Join(d.store.dir, "ev.log")}
	var r recorder
	r.install(d, nil)
	oldTime, oldPath := bootTime, bootMarkerPath
	bootTime = func() string { return "" }
	bootMarkerPath = filepath.Join(d.store.dir, "boot.marker")
	t.Cleanup(func() { bootTime, bootMarkerPath = oldTime, oldPath })
	d.checkBoot()
	if d.store.Get().State != StateActive || r.views != 0 {
		t.Fatal("an active node must not be taken down on a guess")
	}
	said := false
	for _, e := range d.ev.Recent(5) {
		if strings.Contains(e.Msg, "boot time") {
			said = true
		}
	}
	if !said {
		t.Fatal("the skipped check must be reported")
	}
	if _, err := os.Stat(bootMarkerPath); err == nil {
		t.Fatal("no marker without a boot time")
	}
}

// the marker of an EARLIER boot never counts, and a damaged one does not either
func TestBootMarkerOfAnEarlierBootDoesNotCount(t *testing.T) {
	for name, content := range map[string]string{"an earlier boot": "1700000000", "garbage": "\x00\x01 not a time", "empty": ""} {
		t.Run(name, func(t *testing.T) {
			d := nodeOnDisk(t, activeState(RoleMaster, 7), nil, "")
			var r recorder
			r.install(d, nil)
			bootAt(t, d, time.Minute)
			os.WriteFile(bootMarkerPath, []byte(content), 0o600)
			d.checkBoot()
			if d.store.Get().State != StateStandby {
				t.Fatalf("a marker %q is not the marker of this boot", content)
			}
		})
	}
}

// the standby path of the boot fails (the state cannot be saved: /conf full or read-only): no marker is written, so the daemon, at
// its first start, takes the node to standby. Run end to end with the two parts.
func TestFailedBootPathLeavesTheDaemonToDoIt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HASYNC_DIR", dir)
	cfg := filepath.Join(dir, "config.xml")
	t.Setenv("HASYNC_CONFIG", cfg)
	full := "<?xml version=\"1.0\"?><opnsense>" + strings.Repeat("<x>filler</x>", 30) + "</opnsense>\n"
	os.WriteFile(cfg, []byte(full), 0o600)
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"role":"master","state":"active","peer_fp":"fp","peer_addr":"10.0.0.2","peer_port":7690,"epoch":9}`), 0o600)
	oldTime, oldPath := bootTime, bootMarkerPath
	bootTime = func() string { return strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10) }
	bootMarkerPath = filepath.Join(dir, "boot.marker")
	t.Cleanup(func() { bootTime, bootMarkerPath = oldTime, oldPath })
	restore := setFsWrite(func(p string, b []byte, m os.FileMode) error {
		if strings.HasSuffix(p, "state.json") {
			return errors.New("read-only file system")
		}
		return writeFileDurable(p, b, m)
	})
	if err := bootStandbyWith(&actions{standbyView: func(string) error { return nil }}); err == nil {
		t.Fatal("the boot path must report that it failed")
	}
	if _, err := os.Stat(bootMarkerPath); err == nil {
		t.Fatal("a failed boot path must not write the marker")
	}
	restore() // the disk works again when the daemon starts
	st, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	if st.Get().State != StateActive {
		t.Fatal("the failed boot path left 'active' on disk (that is the situation)")
	}
	d := &Daemon{store: st, ops: &Ops{}, hb: newHeartbeat(), auto: &autoState{}, act: &actions{standbyView: func(string) error { return nil }, sh: func(string, ...string) (string, error) { return "", nil }}}
	d.checkBoot()
	if d.store.Get().State != StateStandby {
		t.Fatal("the daemon must take the booted node to standby")
	}
}

// a standby whose boot path stopped between saving the role and writing the view: config.xml may still be the full configuration;
// the daemon writes the view at its first start of the boot
func TestStandbyWhoseBootPathStoppedBeforeTheViewGetsTheView(t *testing.T) {
	d := nodeOnDisk(t, standbyState(RoleStandby, 7), nil, "")
	var r recorder
	r.install(d, nil)
	bootAt(t, d, time.Minute)
	d.checkBoot()
	if r.views != 1 {
		t.Fatalf("standby views written: %d", r.views)
	}
	d.checkBoot() // the boot is checked: no second view
	if r.views != 1 {
		t.Fatal("written again")
	}
}

// kern.boottime moves when the clock is stepped: the marker of this boot still counts, an earlier boot's does not
func TestBootMarkerToleratesAClockStep(t *testing.T) {
	now := time.Now().Unix()
	s := func(v int64) string { return strconv.FormatInt(v, 10) }
	if !sameBoot(s(now), s(now+40)) || !sameBoot(s(now), s(now-40)) {
		t.Fatal("a clock step of a few seconds is the same boot")
	}
	if sameBoot(s(now-3600), s(now)) {
		t.Fatal("a boot an hour earlier is another boot")
	}
	if sameBoot("garbage", s(now)) || sameBoot("", s(now)) {
		t.Fatal("an unreadable marker is not a marker")
	}
}

// the recovery of a node that cannot take a device down is not over: the record stays (the enforcer goes on)
func TestRecoveryIsNotOverWhileADeviceIsStillUp(t *testing.T) {
	h := newWire(macShared1).host("A", map[string]*fakeDev{"vlan0.10": active("10.0.10.1", macShared1)})
	h.devs["vlan0.10"].refuseDown = true
	d := demotionNode(t, h, "vlan0.10")
	d.store.Update(func(s *State) { s.PeerFP = "fp" })
	loadIfacesHook = func(string) (IfaceSet, error) { return IfaceSet{ForceDown: []string{"vlan0.10"}, HAKey: "opt9"}, nil }
	os.WriteFile(activeXML(d.store.dir), []byte("<?xml version=\"1.0\"?><opnsense>"+strings.Repeat("<x>filler</x>", 30)+"</opnsense>\n"), 0o600)
	d.beginIntent(intentDemote, "test")
	d.recoverTransition()
	if d.store.Get().State != StateStandby {
		t.Fatal("standby in memory")
	}
	if !recordExists(d) {
		t.Fatal("a device is still up: the recovery is not over and the record must stay")
	}
	h.w.mu.Lock()
	h.devs["vlan0.10"].refuseDown = false
	h.w.mu.Unlock()
	d.recoverTransition()
	if recordExists(d) {
		t.Fatal("the device went down: the recovery is over")
	}
}

func TestOnlyPluginsAreInstalledOnTheWordOfThePeer(t *testing.T) {
	for name, want := range map[string]bool{
		"os-haproxy": true, "os-node_exporter": true, "os-a": true,
		"sudo": false, "nmap": false, "opnsense": false, "os-": false, "-os-x": false, "os-x y": false, "os-$(id)": false, "../os-x": false, "": false,
		"os-" + strings.Repeat("a", 90): false,
	} {
		if pkgName.MatchString(name) != want {
			t.Errorf("%q: want %v", name, want)
		}
	}
}

// the address that the peer announces for its management interface is data: only an IPv4 literal is kept
func TestPeerManagementAddressIsValidated(t *testing.T) {
	d := newHANode(t, standbyState(RoleStandby, 5))
	for _, bad := range []string{"evil.example.com", "10.0.0.1:7690", "::1", "127.0.0.1", "0.0.0.0", "224.0.0.1", strings.Repeat("a", 60000), "10.0.0.1 ", "999.1.1.1"} {
		d.learnPeerMgmt(bad)
		if got := d.store.Get().PeerMgmt; got != "" {
			t.Fatalf("%q was kept as %q", bad, got)
		}
	}
	d.learnPeerMgmt("192.168.225.212")
	if d.store.Get().PeerMgmt != "192.168.225.212" {
		t.Fatal("a valid address must be learned")
	}
}

// a pf table can read a file as well: table <x> file "/path"
func TestMirroredRulesetMayNotReadFiles(t *testing.T) {
	for _, line := range []string{"table <x> persist file \"/etc/master.passwd\"", "table <x> file \"/etc/ssh/ssh_host_ed25519_key\"", "include \"/tmp/x\"", "load anchor \"a\" from \"/tmp/y\""} {
		if !rsFileDirectiveRe.MatchString(line) {
			t.Errorf("%q must be refused", line)
		}
	}
	for _, line := range []string{"block drop in all", "pass in quick on igb0 proto tcp from any to (self) port 7690", "table <bogons> { 10.0.0.0/8 }"} {
		if rsFileDirectiveRe.MatchString(line) {
			t.Errorf("%q is an ordinary rule", line)
		}
	}
}

// a peer that claims to be active must not be able to walk the clock of the standby: the total moved in an hour is bounded
func TestClockStepsHaveAnHourlyBudget(t *testing.T) {
	clockSteps.mu.Lock()
	clockSteps.steps = nil
	clockSteps.mu.Unlock()
	t.Cleanup(func() { clockSteps.mu.Lock(); clockSteps.steps = nil; clockSteps.mu.Unlock() })
	d := newHANode(t, standbyState(RoleStandby, 5))
	allowed := 0
	for i := 0; i < 20; i++ {
		if d.clockBudgetAllows(maxClockStepMs) { // the largest step the single limit allows
			allowed++
		}
	}
	if allowed != 3 {
		t.Fatalf("%d steps of 5 minutes allowed in an hour, want 3 (15 minutes)", allowed)
	}
	// small drifts keep working while the budget lasts
	clockSteps.mu.Lock()
	clockSteps.steps = nil
	clockSteps.mu.Unlock()
	for i := 0; i < 100; i++ {
		if !d.clockBudgetAllows(3000) {
			t.Fatalf("a normal correction of 3 s was refused at step %d", i)
		}
	}
}
