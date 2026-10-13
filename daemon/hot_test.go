package main

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

const macShared3 = "02:aa:bb:cc:dd:03"

// a LAGG (failover) with two ports and two VLANs on it, plus a LACP LAGG that is not assigned (its VLAN is hot) and an assigned LACP LAGG
// (hot with hot_lacp = shared, classic with "own", its VLAN is hot in both cases)
var hotTestPlan = WarmPlan{
	Ifaces: []WarmIface{
		{Key: "lan", Dev: "lagg0", MAC: macShared1, Hot: true, HotV4: "192.168.1.1/24", Descr: "LAN (lan)"},
		{Key: "opt1", Dev: "vlan0.10", Parent: "lagg0", MAC: macShared1, Hot: true, HotV4: "10.0.10.1/24", VIPs: []string{"10.0.10.5/24"}},
		{Key: "opt2", Dev: "vlan0.20", Parent: "lagg1", MAC: macShared2, Hot: true, HotV4: "10.0.20.1/24"},
		{Key: "opt3", Dev: "lagg2", MAC: macShared3, Hot: true, HotV4: "10.0.30.1/24"},
		{Key: "opt4", Dev: "vlan0.40", Parent: "lagg2", MAC: macShared3, Hot: true, HotV4: "10.0.40.1/24"},
		{Key: "opt5", Dev: "em3", MAC: macShared3, Hot: false, HotWhy: "IPv4 address is not static"},
	},
	Laggs: []LaggInfo{
		{Dev: "lagg0", Proto: "failover", Members: []string{"ixl0", "ixl1"}},
		{Dev: "lagg1", Proto: "lacp", Members: []string{"ixl2", "ixl3"}},
		{Dev: "lagg2", Proto: "lacp", Members: []string{"ixl4"}},
	},
}

func eligible(v WarmPlan) map[string]bool {
	got := map[string]bool{}
	for _, i := range v.Ifaces {
		got[i.Key] = i.Eligible
	}
	return got
}

// with an LACP LAGG that is a shared interface: "off" keeps it and its VLANs classic, "shared" makes them hot (a VLAN on an UNASSIGNED LACP
// LAGG is hot in both cases: the LAGG keeps its own MAC and its LACP system id)
func TestHotViewLACPOffAndShared(t *testing.T) {
	off := hotView(hotTestPlan, false)
	if want := map[string]bool{"lan": true, "opt1": true, "opt2": true, "opt3": false, "opt4": false, "opt5": false}; !reflect.DeepEqual(eligible(off), want) {
		t.Fatalf("off: %v\nwant %v", eligible(off), want)
	}
	for _, i := range off.Ifaces {
		if i.Key == "opt3" && !strings.Contains(i.Reason, "LACP") {
			t.Errorf("the reason must say why: %q", i.Reason)
		}
	}
	shared := hotView(hotTestPlan, true)
	if want := map[string]bool{"lan": true, "opt1": true, "opt2": true, "opt3": true, "opt4": true, "opt5": false}; !reflect.DeepEqual(eligible(shared), want) {
		t.Fatalf("shared: %v\nwant %v", eligible(shared), want)
	}
	// a LAGG protocol the plan did not list is refused in Go as well, whatever the PHP said
	p := hotTestPlan
	p.Ifaces = append([]WarmIface(nil), p.Ifaces...)
	p.Laggs = []LaggInfo{{Dev: "lagg0", Proto: "fec"}}
	if hotView(p, true).Ifaces[0].Eligible {
		t.Error("an unknown LAGG protocol must never be hot")
	}
	if i := shared.Ifaces[1]; i.V4 != "10.0.10.1/24" {
		t.Errorf("the hot addresses must be the ones used: %+v", i)
	}
}

func TestHotDevices(t *testing.T) {
	off := warmDevices(hotView(hotTestPlan, false), nil)
	want := map[string]string{
		"lagg0": macShared1, "vlan0.10": macShared1, // an assigned LAGG carries the shared MAC and so do its VLANs
		"ixl0": "", "ixl1": "", // its ports follow it
		"vlan0.20": macShared2, "lagg1": hotParent, // an unassigned (LACP) LAGG keeps its own MAC and accepts the VLAN's
		"ixl2": "", "ixl3": "",
	}
	if !reflect.DeepEqual(off, want) {
		t.Fatalf("off:    %v\nwant %v", off, want)
	}
	shared := warmDevices(hotView(hotTestPlan, true), nil)
	if shared["lagg2"] != macShared3 || shared["vlan0.40"] != macShared3 {
		t.Errorf("shared: the LACP LAGG carries the shared MAC like any other: %v", shared)
	}
	// the HA/management devices are never touched
	g := warmDevices(hotView(hotTestPlan, true), []string{"vlan0.10", "ixl0"})
	for _, dev := range []string{"vlan0.10", "ixl0"} {
		if _, in := g[dev]; in {
			t.Errorf("%s: a device the daemon must not touch is in the set: %v", dev, g)
		}
	}
}

func flat(c [][]string) []string {
	var out []string
	for _, x := range c {
		out = append(out, strings.Join(x[1:], " "))
	}
	return out
}

func TestHotSilenceCommandsOrder(t *testing.T) {
	st := ifState{Exists: true, Up: true, Ether: "00:11:22:33:44:55", Inet: []string{"10.0.10.1"}, Inet6: []string{"2001:db8::1"}}
	f := flat(hotSilenceCmds("vlan0.10", macShared1, st))
	i6, link, up := indexOf(f, "vlan0.10 inet6 ifdisabled"), indexOf(f, "vlan0.10 link "), indexOf(f, "vlan0.10 up")
	if i6 != 0 || link < 0 || up != len(f)-1 || !(i6 < link && link < up) {
		t.Fatalf("IPv6 off first, the MAC while nothing is sent, up last: %q", f)
	}
	if indexOf(flat(hotSilenceCmds("lagg1", hotParent, ifState{Exists: true})), "lagg1 promisc") < 0 {
		t.Error("a parent must be promiscuous")
	}
	for _, x := range flat(hotSilenceCmds("ixl0", "", ifState{Exists: true})) {
		if strings.Contains(x, "link") {
			t.Errorf("a LAGG port keeps the MAC its LAGG gave it: %v", x)
		}
	}
}

func hotWire(t *testing.T) (*wire, *Daemon, *fakeHost, *Daemon, *fakeHost) {
	t.Helper()
	resetHotGuard()
	t.Cleanup(resetHotGuard)
	w := newWire(macShared1)
	w.hot = true
	mk := func(name string, st State, devs map[string]*fakeDev) (*Daemon, *fakeHost) {
		h := w.host(name, devs)
		d := newHANode(t, st)
		d.cfg.HAMode = "hot"
		d.drift = &driftState{}
		d.act.sh = h.sh
		d.act.flush = h.flush
		d.act.plan = func(string) (WarmPlan, error) { return hotTestPlan, nil }
		xml := "<opnsense>" + strings.Repeat("<x>filler</x>", 30) + "</opnsense>"
		if err := os.WriteFile(activeXML(d.store.dir), []byte(xml), 0o600); err != nil {
			t.Fatal(err)
		}
		h.pfRules = "block drop in log inet all label \"x\"\nblock drop in log inet6 all label \"x\"\n"
		return d, h
	}
	devs := func(own string) map[string]*fakeDev {
		return map[string]*fakeDev{
			"ixl0": {ether: own, hw: own, carrier: true}, "ixl1": {ether: own, hw: own, carrier: true},
			"lagg0": {ether: own, hw: own, carrier: true}, "vlan0.10": {ether: own, hw: own, carrier: true},
			"lagg1": {ether: "00:11:22:33:ff:01", hw: "00:11:22:33:ff:01", carrier: true}, "vlan0.20": {ether: "00:11:22:33:ff:01", hw: "00:11:22:33:ff:01", carrier: true},
			"ixl2": {ether: "00:11:22:33:ff:02", hw: "00:11:22:33:ff:02", carrier: true}, "ixl3": {ether: "00:11:22:33:ff:03", hw: "00:11:22:33:ff:03", carrier: true},
			"lagg2": {ether: "00:11:22:33:ff:04", hw: "00:11:22:33:ff:04", carrier: true}, "vlan0.40": {ether: "00:11:22:33:ff:04", hw: "00:11:22:33:ff:04", carrier: true}, "ixl4": {ether: "00:11:22:33:ff:04", hw: "00:11:22:33:ff:04", carrier: true},
		}
	}
	a, ha := mk("A", activeState(RoleMaster, 5), devs("00:aa:00:00:00:01"))
	b, hb := mk("B", standbyState(RoleStandby, 5), devs("00:bb:00:00:00:01"))
	return w, a, ha, b, hb
}

var hotSet = IfaceSet{ForceDown: []string{"lagg0", "ixl0", "ixl1", "vlan0.10", "lagg1", "vlan0.20", "ixl2", "ixl3", "lagg2", "vlan0.40", "ixl4"}}

// A is the active node (addresses on the shared MAC), B the hot standby. The standby is silent: up, the shared MAC, nothing else;
// the takeover never changes a MAC and never drops a link; at no moment do both nodes own the shared MAC together.
func TestHotStandbyThenTakeoverNeverMovesAMAC(t *testing.T) {
	w, a, ha, b, hb := hotWire(t)
	for dev, ip := range map[string]string{"lagg0": "192.168.1.1", "vlan0.10": "10.0.10.1"} {
		d := ha.devs[dev]
		d.ether, d.up, d.inet = macShared1, true, []string{ip}
	}
	b.warmReadiness(b.activeXMLPath(), hotSet)
	if ok, why := b.warmReady(); !ok {
		t.Fatalf("a hot standby must be ready: %s", why)
	}
	for _, dev := range []string{"lagg0", "vlan0.10", "ixl0", "ixl1"} {
		st := b.readIf(dev)
		if !st.Up || !st.Disabled || len(st.Inet) > 0 {
			t.Errorf("%s is not silent and up: %+v", dev, st)
		}
	}
	for _, dev := range []string{"lagg0", "vlan0.10"} {
		if !macEq(b.readIf(dev).Ether, macShared1) {
			t.Errorf("%s must already carry the shared MAC", dev)
		}
	}
	if st := b.readIf("lagg1"); !st.Promisc || macEq(st.Ether, macShared2) {
		t.Errorf("an unassigned LAGG keeps its own MAC and is promiscuous: %+v", st)
	}
	if st := b.readIf("vlan0.20"); !macEq(st.Ether, macShared2) {
		t.Errorf("its VLAN carries the shared MAC: %+v", st)
	}
	if len(w.violations) > 0 {
		t.Fatalf("two owners of the shared MAC: %v", w.violations)
	}
	// A stands down (the addresses go), B takes over
	a.warmSilence("lagg0", macShared1)
	a.warmSilence("vlan0.10", macShared1)
	before := len(hb.commands())
	p, _ := b.planFor("x")
	done, err := b.warmActivate(p, hotSet)
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if !done["lan"] || !done["opt1"] || !done["opt2"] || done["opt3"] || done["opt4"] { // opt3, opt4: an assigned LACP LAGG (hot_lacp off) and its VLAN keep the classic takeover
		t.Fatalf("handled: %v", done)
	}
	for _, c := range hb.commands()[before:] {
		if strings.Contains(c, " link ") || strings.Contains(c, " down") || strings.Contains(c, "-promisc") {
			t.Errorf("the takeover must not change a MAC nor touch a link: %s", c)
		}
	}
	if st := b.readIf("vlan0.10"); !hasAddr(st.Inet, "10.0.10.1") || !hasAddr(st.Inet, "10.0.10.5") || !st.Up {
		t.Errorf("the VLAN did not take its addresses: %+v", st)
	}
	if len(w.violations) > 0 {
		t.Fatalf("two owners of the shared MAC: %v", w.violations)
	}
}

func TestHotTakeoverRefusesAStandbyThatIsNotSilent(t *testing.T) {
	_, _, _, b, hb := hotWire(t)
	b.warmReadiness(b.activeXMLPath(), hotSet)
	hb.devs["vlan0.10"].inet = []string{"10.0.10.99"} // something configured it behind our back
	p, _ := b.planFor("x")
	if _, err := b.warmActivate(p, hotSet); err == nil {
		t.Fatal("a device that is not in the hot state must make the takeover fall back to the classic path")
	}
	if hb.devs["lagg0"].inet != nil {
		t.Error("what was done before the refusal must be undone")
	}
}

func TestHotTransmitGuardShutsDownADeviceThatSpeaks(t *testing.T) {
	_, _, _, b, hb := hotWire(t)
	hotTxMu.Lock()
	hotTxLast = map[string]uint64{}
	hotTxMu.Unlock()
	b.warmReadiness(b.activeXMLPath(), hotSet) // baseline
	if ok, why := b.warmReady(); !ok {
		t.Fatalf("must be ready first: %s", why)
	}
	hb.devs["vlan0.10"].tx += 3 // it sent something while it had to be silent
	b.warmReadiness(b.activeXMLPath(), hotSet)
	if ok, why := b.warmReady(); ok || !strings.Contains(why, "vlan0.10 transmitted 3") {
		t.Fatalf("not ready, with the reason: %v %q", ok, why)
	}
	if hb.devs["vlan0.10"].up {
		t.Error("the device that transmitted must be shut down")
	}
}

func TestHotNeedsADefaultDenyOnTheStandby(t *testing.T) {
	_, _, _, b, hb := hotWire(t)
	hb.pfRules = "pass in quick on igb1 inet proto tcp from any to any port = ssh keep state\n" // no block all: pf passes by default
	b.warmReadiness(b.activeXMLPath(), hotSet)
	if ok, why := b.warmReady(); ok || !strings.Contains(why, "default deny") {
		t.Fatalf("a standby without default deny must not be ready: %v %q", ok, why)
	}
}

func TestPfDefaultDenyAndTxParsing(t *testing.T) {
	if !pfDefaultDeny("block drop in log inet all label \"02f4\"\n") || !pfDefaultDeny("block drop in log all\n") {
		t.Error("the OPNsense default deny line must be recognised")
	}
	if pfDefaultDeny("block drop in log on ! igb0 inet from 10.0.0.0/24 to any\n") || pfDefaultDeny("pass in all\n") {
		t.Error("an anti-spoofing rule is not a default deny")
	}
	out := "Name Mtu Network Address Ipkts Ierrs Idrop Ibytes Opkts Oerrs Obytes Coll\nvlan0.65* 1500 <Link#29> 3c:fd:fe:2d:bc:00 0 0 0 0 17 0 1190 0\nvlan0.65* - fe80::%vlan0.65/64 fe80::1 0 - - 0 3 - 288 -\n"
	if n, ok := txPackets(out); !ok || n != 17 {
		t.Errorf("Opkts of the link row: %d %v", n, ok)
	}
}

// an LACP LAGG transmits its protocol frames (LACPDUs): they are not a leak, the guard must leave it alone
func TestHotLACPLagIsNotShutDownForItsLACPDUs(t *testing.T) {
	_, _, _, b, hb := hotWire(t)
	b.cfg.HotLACP = "shared"
	hotTxMu.Lock()
	hotTxLast = map[string]uint64{}
	hotTxMu.Unlock()
	b.warmReadiness(b.activeXMLPath(), hotSet)
	if ok, why := b.warmReady(); !ok {
		t.Fatalf("must be ready: %s", why)
	}
	hb.devs["lagg2"].tx += 22
	b.warmReadiness(b.activeXMLPath(), hotSet)
	if ok, why := b.warmReady(); !ok || !hb.devs["lagg2"].up {
		t.Fatalf("the LACP LAGG must stay up and the standby ready: %v %q", ok, why)
	}
	hb.devs["vlan0.40"].tx += 1 // a VLAN on it must still be silent
	b.warmReadiness(b.activeXMLPath(), hotSet)
	if ok, _ := b.warmReady(); ok || hb.devs["vlan0.40"].up {
		t.Fatal("a VLAN that transmits is shut down even on an LACP LAGG")
	}
}

// found on hardware: a node that was active transmits a lot; once it stands down that is not "a standby that speaks"
func TestHotGuardBaselineSurvivesATermAsActiveNode(t *testing.T) {
	_, _, _, b, hb := hotWire(t)
	hotTxMu.Lock()
	hotTxLast = map[string]uint64{}
	hotTxMu.Unlock()
	b.warmReadiness(b.activeXMLPath(), hotSet) // the baseline of the standby
	p, _ := b.planFor("x")
	if _, err := b.warmActivate(p, hotSet); err != nil {
		t.Fatal(err)
	}
	hb.devs["vlan0.10"].tx += 2575 // it carried traffic as the active node
	hb.devs["lagg0"].tx += 100
	warm := b.warmSet(b.activeXMLPath(), hotSet)
	for _, dev := range hotSet.ForceDown { // the stand-down
		b.standbyDevice(dev, warm)
	}
	b.warmReadiness(b.activeXMLPath(), hotSet)
	if ok, why := b.warmReady(); !ok {
		t.Fatalf("a node that just stood down must be a ready standby, not a guilty one: %s", why)
	}
	if !hb.devs["vlan0.10"].up {
		t.Error("the device must not have been shut down")
	}
}

func TestPfDefaultDenyInTheGeneratedSource(t *testing.T) {
	src := "block in log inet from {any} to {any} label \"02f4\" # Default deny / state violation rule\n"
	if !pfDefaultDeny(src) {
		t.Error("the default deny of the generated ruleset (rules.debug) must be recognised")
	}
	if pfDefaultDeny("block in log quick inet proto {tcp udp} from {any} port {0} to {any} label \"b4\" # block all targeting port 0\n") {
		t.Error("a rule that blocks port 0 is not the default deny")
	}
}

func resetHotGuard() {
	hotTxMu.Lock()
	hotTxLast = map[string]uint64{}
	hotLeak = map[string]time.Time{}
	hotTxMu.Unlock()
}

// found by the review: the first version brought a device that had leaked back up at the next pass of the enforcer
func TestHotLeakIsLatched(t *testing.T) {
	_, _, _, b, hb := hotWire(t)
	b.warmReadiness(b.activeXMLPath(), hotSet) // baseline
	hb.devs["vlan0.10"].tx += 3
	for pass := 0; pass < 3; pass++ { // the enforcer runs every second
		b.warmReadiness(b.activeXMLPath(), hotSet)
		if ok, _ := b.warmReady(); ok {
			t.Fatalf("pass %d: a device that leaked must keep the standby not ready", pass)
		}
		if hb.devs["vlan0.10"].up {
			t.Fatalf("pass %d: it must stay down", pass)
		}
	}
	// the latch ends after a while (or the operator fixes the NIC)
	hotTxMu.Lock()
	hotLeak["vlan0.10"] = time.Now().Add(-hotLeakLatch - time.Second)
	hotTxMu.Unlock()
	b.warmReadiness(b.activeXMLPath(), hotSet)
	b.warmReadiness(b.activeXMLPath(), hotSet)
	if ok, why := b.warmReady(); !ok {
		t.Fatalf("after the latch the standby is ready again: %s", why)
	}
}

// a safety check that cannot be made is not passed
func TestHotChecksFailClosed(t *testing.T) {
	_, _, _, b, hb := hotWire(t)
	hb.pfRules = "" // pfctl cannot be read
	b.warmReadiness(b.activeXMLPath(), hotSet)
	if ok, why := b.warmReady(); ok || !strings.Contains(why, "cannot be read") {
		t.Fatalf("an unreadable packet filter is not a pass: %v %q", ok, why)
	}
}

func TestHotDevicesRefuseOddNames(t *testing.T) {
	p := hotView(hotTestPlan, true)
	p.Laggs = append([]LaggInfo(nil), p.Laggs...)
	p.Laggs[0].Members = []string{"ixl0", "-promisc", "ixl1 up"}
	for _, dev := range []string{"-promisc", "ixl1 up"} {
		if _, in := warmDevices(p, nil)[dev]; in {
			t.Errorf("%q must never reach ifconfig", dev)
		}
	}
	if _, in := warmDevices(p, nil)["ixl0"]; !in {
		t.Error("a normal member stays")
	}
}
