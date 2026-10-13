package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// The warm standby tests run the real code against a model of two hosts with NICs on the same switch (a "wire"). The model
// records every command and watches the one invariant that matters: the shared MAC address is never configured on two
// ports at the same time. It models ifconfig, not a driver or a switch: what a real NIC does when its MAC changes with
// the link up is exactly what docs/validation.md section H has to establish on hardware.

const (
	macShared1 = "02:aa:bb:cc:dd:01"
	macShared2 = "02:aa:bb:cc:dd:02"
)

type fakeDev struct {
	up, carrier, promisc, nd6off bool
	ether, hw                    string
	inet, inet6                  []string
	refuseMAC                    bool // the driver ignores a MAC change
	noHWLine                     bool // ifconfig does not print "hwaddr"
}

type wire struct {
	mu         sync.Mutex
	hosts      []*fakeHost
	shared     map[string]bool
	violations []string
}

type fakeHost struct {
	w    *wire
	name string
	devs map[string]*fakeDev
	log  []string
}

func newWire(shared ...string) *wire {
	w := &wire{shared: map[string]bool{}}
	for _, s := range shared {
		w.shared[s] = true
	}
	return w
}

func (w *wire) host(name string, devs map[string]*fakeDev) *fakeHost {
	h := &fakeHost{w: w, name: name, devs: devs}
	w.hosts = append(w.hosts, h)
	return h
}

// nic is a link-up port with its own hardware MAC and nothing else (the warm standby condition).
func nic(hw string) *fakeDev {
	return &fakeDev{up: true, carrier: true, ether: hw, hw: hw, nd6off: true}
}

func (w *wire) check(what string) { // caller holds w.mu
	count := map[string]int{}
	for _, h := range w.hosts {
		for _, d := range h.devs {
			if w.shared[d.ether] && d.up {
				count[d.ether]++
			}
		}
	}
	for m, n := range count {
		if n > 1 {
			w.violations = append(w.violations, fmt.Sprintf("%s: shared MAC %s on %d ports", what, m, n))
		}
	}
}

func (h *fakeHost) render(name string, d *fakeDev) string {
	flags := []string{"BROADCAST", "MULTICAST"}
	if d.up {
		flags = append([]string{"UP"}, flags...)
	}
	if d.promisc {
		flags = append(flags, "PROMISC")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: flags=8843<%s> metric 0 mtu 1500\n\tether %s\n", name, strings.Join(flags, ","), d.ether)
	if d.ether != d.hw && !d.noHWLine {
		fmt.Fprintf(&b, "\thwaddr %s\n", d.hw)
	}
	for _, a := range d.inet {
		fmt.Fprintf(&b, "\tinet %s netmask 0xffffff00 broadcast 10.255.255.255\n", a)
	}
	fmt.Fprintf(&b, "\tinet6 fe80::1%%%s prefixlen 64 scopeid 0x1\n", name)
	for _, a := range d.inet6 {
		fmt.Fprintf(&b, "\tinet6 %s prefixlen 64\n", a)
	}
	if d.nd6off {
		b.WriteString("\tnd6 options=29<PERFORMNUD,IFDISABLED,AUTO_LINKLOCAL>\n")
	} else {
		b.WriteString("\tnd6 options=21<PERFORMNUD,AUTO_LINKLOCAL>\n")
	}
	if d.carrier && d.up {
		b.WriteString("\tstatus: active\n")
	} else {
		b.WriteString("\tstatus: no carrier\n")
	}
	return b.String()
}

func without(l []string, ip string) []string {
	var out []string
	for _, a := range l {
		if a != ip {
			out = append(out, a)
		}
	}
	return out
}

func (h *fakeHost) sh(name string, args ...string) (string, error) {
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	if name != "ifconfig" {
		return "", fmt.Errorf("unexpected command %s", name)
	}
	if len(args) == 2 && args[0] == "-l" && args[1] == "-u" {
		var up []string
		for n, d := range h.devs {
			if d.up {
				up = append(up, n)
			}
		}
		return strings.Join(up, " "), nil
	}
	d, ok := h.devs[args[0]]
	if !ok {
		return "ifconfig: interface " + args[0] + " does not exist", fmt.Errorf("no such interface")
	}
	if len(args) == 1 {
		return h.render(args[0], d), nil
	}
	h.log = append(h.log, strings.Join(args, " "))
	rest := args[1:]
	for len(rest) > 0 {
		switch rest[0] {
		case "link":
			if !d.refuseMAC {
				d.ether = strings.ToLower(rest[1])
			}
			rest = rest[2:]
		case "inet":
			ip := strings.SplitN(rest[1], "/", 2)[0]
			rest = rest[2:]
			if len(rest) > 0 && rest[0] == "-alias" {
				d.inet = without(d.inet, ip)
				rest = rest[1:]
			} else {
				if len(rest) > 0 && rest[0] == "alias" {
					rest = rest[1:]
				}
				d.inet = append(without(d.inet, ip), ip)
			}
		case "inet6":
			switch rest[1] {
			case "ifdisabled":
				d.nd6off = true
				rest = rest[2:]
			case "-ifdisabled":
				d.nd6off = false
				rest = rest[2:]
			case "-accept_rtadv", "accept_rtadv":
				rest = rest[2:]
			default:
				ip := strings.SplitN(rest[1], "/", 2)[0]
				rest = rest[2:]
				if len(rest) > 0 && rest[0] == "-alias" {
					d.inet6 = without(d.inet6, ip)
					rest = rest[1:]
				} else {
					d.inet6 = append(without(d.inet6, ip), ip)
				}
			}
		case "-no_dad", "no_dad":
			rest = rest[1:]
		case "-promisc":
			d.promisc = false
			rest = rest[1:]
		case "promisc":
			d.promisc = true
			rest = rest[1:]
		case "up":
			d.up = true
			rest = rest[1:]
		case "down":
			d.up = false
			rest = rest[1:]
		case "mtu", "description":
			rest = rest[2:]
		default:
			return "", fmt.Errorf("unexpected ifconfig argument %q", rest[0])
		}
	}
	h.w.check(h.name + ": ifconfig " + strings.Join(args, " "))
	return "", nil
}

func (h *fakeHost) flush(dev string) {
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	d := h.devs[dev]
	h.log = append(h.log, "flush "+dev)
	d.up, d.inet, d.inet6, d.promisc = false, nil, nil, false
}

func (h *fakeHost) commands() []string {
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	return append([]string(nil), h.log...)
}

func indexOf(l []string, prefix string) int {
	for i, s := range l {
		if strings.HasPrefix(s, prefix) {
			return i
		}
	}
	return -1
}

var testPlan = WarmPlan{Ifaces: []WarmIface{
	{Key: "lan", Dev: "em1", MAC: macShared1, V4: "192.168.1.1/24", Eligible: true},
	{Key: "wan", Dev: "em0", MAC: macShared2, V4: "10.0.0.1/24", VIPs: []string{"10.0.0.5/24"}, V6: "2001:db8::1/64", MTU: 1492, Eligible: true},
	{Key: "opt1", Dev: "em2", MAC: "02:aa:bb:cc:dd:03", Eligible: false, Reason: "IPv4 address is not static"},
}}

var testSet = IfaceSet{ForceDown: []string{"em0", "em1", "em2"}, KeepUp: []string{"hn3"}}

func warmNode(t *testing.T, w *wire, name string, st State, devs map[string]*fakeDev) (*Daemon, *fakeHost) {
	t.Helper()
	h := w.host(name, devs)
	d := newHANode(t, st)
	d.cfg.HAMode = "warm"
	d.drift = &driftState{}
	d.act.sh = h.sh
	d.act.flush = h.flush
	d.act.plan = func(string) (WarmPlan, error) { return testPlan, nil }
	// a valid-looking saved full configuration, so the readiness does not fail on it
	xml := "<opnsense>" + strings.Repeat("<x>filler</x>", 30) + "</opnsense>"
	if err := os.WriteFile(activeXML(d.store.dir), []byte(xml), 0o600); err != nil {
		t.Fatal(err)
	}
	return d, h
}

func standbyDevs() map[string]*fakeDev {
	return map[string]*fakeDev{"em0": nic("00:11:22:33:00:00"), "em1": nic("00:11:22:33:00:01"), "em2": {up: false, ether: "00:11:22:33:00:02", hw: "00:11:22:33:00:02", nd6off: true}, "hn3": nic("00:11:22:33:00:09")}
}

func (d *Daemon) activeXMLPath() string { return activeXML(d.store.dir) }

// ---- pure parts ----

func TestParseIfconfig(t *testing.T) {
	out := "em1: flags=8843<UP,BROADCAST,RUNNING,PROMISC,MULTICAST> metric 0 mtu 1500\n\tether 02:aa:bb:cc:dd:01\n\thwaddr 00:11:22:33:00:01\n\tinet 192.168.1.1 netmask 0xffffff00 broadcast 192.168.1.255\n\tinet6 fe80::1%em1 prefixlen 64 scopeid 0x1\n\tinet6 2001:db8::1 prefixlen 64\n\tnd6 options=21<PERFORMNUD,AUTO_LINKLOCAL>\n\tstatus: active\n"
	s := parseIfconfig(out)
	if !s.Exists || !s.Up || !s.Promisc || !s.Carrier || s.Disabled || s.Ether != macShared1 || s.HW != "00:11:22:33:00:01" ||
		!reflect.DeepEqual(s.Inet, []string{"192.168.1.1"}) || !reflect.DeepEqual(s.Inet6, []string{"2001:db8::1"}) {
		t.Fatalf("%+v", s)
	}
	if parseIfconfig("").Exists {
		t.Fatal("empty output is not an interface")
	}
	if s := parseIfconfig("lo9: flags=8049<UP,LOOPBACK> metric 0 mtu 16384\n\tether 00:00:00:00:00:01\n"); !s.Disabled {
		t.Fatal("a device without IPv6 has nothing to disable")
	}
}

func TestActivateOrderMACFirstUpLast(t *testing.T) {
	c := activateCmds(testPlan.Ifaces[1])
	var flat []string
	for _, x := range c {
		flat = append(flat, strings.Join(x, " "))
	}
	if flat[0] != "ifconfig em0 link "+macShared2 {
		t.Fatalf("the shared MAC must be set before anything else: %q", flat)
	}
	if indexOf(flat, "ifconfig em0 inet 10.0.0.1/24") < 1 || indexOf(flat, "ifconfig em0 inet 10.0.0.5/24 alias") < 0 ||
		indexOf(flat, "ifconfig em0 inet6 2001:db8::1/64") < 0 || indexOf(flat, "ifconfig em0 mtu 1492") < 0 {
		t.Fatalf("%q", flat)
	}
	if flat[len(flat)-1] != "ifconfig em0 up" {
		t.Fatalf("%q", flat)
	}
}

func TestSilenceOrderOwnMACFirstAndLinkStaysUp(t *testing.T) {
	st := ifState{Exists: true, Up: true, Ether: macShared1, Inet: []string{"192.168.1.1"}, Inet6: []string{"2001:db8::1"}, Promisc: true}
	var flat []string
	for _, x := range silenceCmds("em1", "00:11:22:33:00:01", st) {
		flat = append(flat, strings.Join(x, " "))
	}
	if flat[0] != "ifconfig em1 link 00:11:22:33:00:01" {
		t.Fatalf("the own MAC must come back before any address is removed: %q", flat)
	}
	if indexOf(flat, "ifconfig em1 inet 192.168.1.1 -alias") < 1 || indexOf(flat, "ifconfig em1 inet6 2001:db8::1 -alias") < 1 {
		t.Fatalf("%q", flat)
	}
	for _, f := range flat {
		if strings.HasSuffix(f, " down") {
			t.Fatalf("a warm standby never takes the link down: %q", flat)
		}
	}
	if flat[len(flat)-1] != "ifconfig em1 up" || indexOf(flat, "ifconfig em1 inet6 ifdisabled") < 0 {
		t.Fatalf("%q", flat)
	}
}

func TestWarmDevicesParentsAndExclusions(t *testing.T) {
	p := WarmPlan{Ifaces: []WarmIface{
		{Key: "lan", Dev: "em1", MAC: macShared1, Eligible: true},
		{Key: "v10", Dev: "vlan0.10", MAC: macShared2, Parent: "em3", Eligible: true},
		{Key: "v20", Dev: "vlan0.20", Parent: "em3", Eligible: false},
		{Key: "hav", Dev: "vlan0.30", MAC: "02:aa:bb:cc:dd:09", Parent: "em4", Eligible: true},
	}}
	got := warmDevices(p, []string{"em4"}) // em4 carries the HA link: never touched
	want := map[string]string{"em1": macShared1, "vlan0.10": macShared2, "em3": "", "vlan0.30": "02:aa:bb:cc:dd:09"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	o := eligibleOrdered(p)
	if len(o) != 3 || o[0].Dev != "em1" || o[1].Parent == "" {
		t.Fatalf("physical interfaces must be activated before the VLANs on them: %+v", o)
	}
}

func TestHWMacsLearnAndPersist(t *testing.T) {
	dir := t.TempDir()
	h := openHWMacs(dir)
	// shared MAC on the port and no hwaddr line: nothing known
	if _, ok := h.resolve("em1", macShared1, ifState{Ether: macShared1}); ok {
		t.Fatal("the shared MAC must not be taken for the hardware one")
	}
	// ifconfig prints it
	if mac, ok := h.resolve("em1", macShared1, ifState{Ether: macShared1, HW: "00:11:22:33:00:01"}); !ok || mac != "00:11:22:33:00:01" {
		t.Fatalf("%v %v", mac, ok)
	}
	// later the line is not printed any more: the memory answers, also after a restart of the daemon
	h2 := openHWMacs(dir)
	if mac, ok := h2.resolve("em1", macShared1, ifState{Ether: macShared1}); !ok || mac != "00:11:22:33:00:01" {
		t.Fatalf("not remembered: %v %v", mac, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, "hwmacs.json")); err != nil {
		t.Fatal(err)
	}
	// a current MAC that is not the shared one is the hardware one
	if mac, ok := h2.resolve("em5", macShared1, ifState{Ether: "00:11:22:33:00:55"}); !ok || mac != "00:11:22:33:00:55" {
		t.Fatalf("%v %v", mac, ok)
	}
}

// ---- behaviour against the model ----

func TestWarmStandbyBecomesReadyAndSilencesLeftovers(t *testing.T) {
	w := newWire(macShared1, macShared2)
	devs := standbyDevs()
	// em1 still carries the identity of the previous active period: shared MAC, an address, promiscuous mode
	devs["em1"].ether = macShared1
	devs["em1"].inet = []string{"192.168.1.1"}
	devs["em1"].promisc = true
	devs["em1"].nd6off = false
	d, h := warmNode(t, w, "B", standbyState(RoleStandby, 5), devs)

	d.warmReadiness(d.activeXMLPath(), testSet)
	if ok, why := d.warmReady(); !ok {
		t.Fatalf("not ready: %s", why)
	}
	if got := devs["em1"]; got.ether != "00:11:22:33:00:01" || len(got.inet) != 0 || got.promisc || !got.up || !got.nd6off {
		t.Fatalf("em1 not silent: %+v", got)
	}
	if !devs["em0"].up || devs["em0"].ether != "00:11:22:33:00:00" {
		t.Fatalf("em0 must stay up with its own MAC: %+v", devs["em0"])
	}
	cmds := h.commands()
	if i, j := indexOf(cmds, "em1 link 00:11:22:33:00:01"), indexOf(cmds, "em1 inet 192.168.1.1 -alias"); i < 0 || j < 0 || i > j {
		t.Fatalf("the own MAC must be restored before the address is removed: %q", cmds)
	}
	if len(w.violations) > 0 {
		t.Fatal(w.violations)
	}
	info := d.warmInfo()
	if info["mode"] != "warm" || info["ready"] != true {
		t.Fatalf("%v", info)
	}
	if got := fmt.Sprint(info["classic_ifaces"]); !strings.Contains(got, "IPv4 address is not static") {
		t.Fatalf("the interface that keeps the classic takeover must be named: %v", got)
	}
	if d.warmSummary() != "ready" {
		t.Fatalf("%q", d.warmSummary())
	}
}

func TestWarmNotReadyWithoutLinkOrConfig(t *testing.T) {
	w := newWire(macShared1, macShared2)
	devs := standbyDevs()
	devs["em0"].carrier = false
	d, _ := warmNode(t, w, "B", standbyState(RoleStandby, 5), devs)
	d.warmReadiness(d.activeXMLPath(), testSet)
	if ok, why := d.warmReady(); ok || !strings.Contains(why, "em0 has no link") {
		t.Fatalf("ok=%v why=%q", ok, why)
	}
	if !strings.Contains(d.warmSummary(), "em0 has no link") {
		t.Fatalf("the peer must learn why: %q", d.warmSummary())
	}

	devs["em0"].carrier = true
	os.Remove(d.activeXMLPath())
	d.warmReadiness(d.activeXMLPath(), testSet)
	if ok, why := d.warmReady(); ok || !strings.Contains(why, "saved configuration") {
		t.Fatalf("ok=%v why=%q", ok, why)
	}
}

func TestClassicModeTouchesNothing(t *testing.T) {
	w := newWire(macShared1)
	d, h := warmNode(t, w, "B", standbyState(RoleStandby, 5), standbyDevs())
	d.cfg.HAMode = "classic"
	if got := d.warmSet(d.activeXMLPath(), testSet); got != nil {
		t.Fatalf("%v", got)
	}
	d.warmReadiness(d.activeXMLPath(), testSet)
	if len(h.commands()) != 0 {
		t.Fatalf("classic mode ran %q", h.commands())
	}
	if d.warmSummary() != "" || d.warmInfo()["mode"] != "classic" {
		t.Fatal("classic mode must not report a warm state")
	}
	if _, err := d.warmActivate(&testPlan, testSet); err == nil {
		t.Fatal("no fast takeover in classic mode")
	}
}

func TestWarmTakeoverAndStandDownNeverShareTheMAC(t *testing.T) {
	w := newWire(macShared1, macShared2)
	a, ha := warmNode(t, w, "A", activeState(RoleMaster, 6), map[string]*fakeDev{
		// A is the active node: shared MACs and addresses, hwaddr visible
		"em0": {up: true, carrier: true, ether: macShared2, hw: "00:aa:00:00:00:00", inet: []string{"10.0.0.1"}, inet6: []string{"2001:db8::1"}},
		"em1": {up: true, carrier: true, ether: macShared1, hw: "00:aa:00:00:00:01", inet: []string{"192.168.1.1"}},
		"em2": {up: true, carrier: true, ether: "00:aa:00:00:00:02", hw: "00:aa:00:00:00:02"},
		"hn3": nic("00:aa:00:00:00:09"),
	})
	b, hb := warmNode(t, w, "B", standbyState(RoleStandby, 5), standbyDevs())
	b.warmReadiness(b.activeXMLPath(), testSet)
	if ok, why := b.warmReady(); !ok {
		t.Fatalf("standby not ready: %s", why)
	}

	// stand down on A: the demote runs this for every device
	warmA := warmDevices(testPlan, testSet.KeepUp)
	var wg sync.WaitGroup
	for _, dev := range testSet.ForceDown {
		wg.Add(1)
		go func(dev string) { defer wg.Done(); a.standbyDevice(dev, warmA) }(dev)
	}
	wg.Wait()
	if len(w.violations) > 0 {
		t.Fatal(w.violations)
	}
	for _, dev := range []string{"em0", "em1"} {
		if got := ha.devs[dev]; !got.up || len(got.inet) != 0 || len(got.inet6) != 0 || got.ether != got.hw {
			t.Fatalf("%s must be up, silent and on its own MAC after the stand down: %+v", dev, got)
		}
	}
	if ha.devs["em2"].up {
		t.Fatal("the classic interface goes down as before")
	}
	if i, j := indexOf(ha.commands(), "em1 link 00:aa:00:00:00:01"), indexOf(ha.commands(), "em1 inet 192.168.1.1 -alias"); i < 0 || i > j {
		t.Fatalf("stand down order: %q", ha.commands())
	}

	// takeover on B
	done, err := b.warmActivate(&testPlan, testSet)
	if err != nil {
		t.Fatal(err)
	}
	if !done["lan"] || !done["wan"] || done["opt1"] {
		t.Fatalf("natively handled: %v", done)
	}
	if got := hb.devs["em1"]; got.ether != macShared1 || !reflect.DeepEqual(got.inet, []string{"192.168.1.1"}) || !got.up {
		t.Fatalf("em1: %+v", got)
	}
	if got := hb.devs["em0"]; got.ether != macShared2 || len(got.inet) != 2 || len(got.inet6) != 1 || got.nd6off {
		t.Fatalf("em0 must carry the address, its alias and IPv6: %+v", got)
	}
	if hb.devs["em2"].up {
		t.Fatal("the classic interface is left to the classic path")
	}
	if len(w.violations) > 0 {
		t.Fatal(w.violations)
	}

	// the other way round: B stands down, A takes over (a failback)
	warmB := warmDevices(testPlan, testSet.KeepUp)
	for _, dev := range testSet.ForceDown {
		b.standbyDevice(dev, warmB)
	}
	a.warmReadiness(a.activeXMLPath(), testSet)
	if ok, why := a.warmReady(); !ok {
		t.Fatalf("A not ready after standing down: %s", why)
	}
	if _, err := a.warmActivate(&testPlan, testSet); err != nil {
		t.Fatal(err)
	}
	if len(w.violations) > 0 {
		t.Fatal(w.violations)
	}
}

func TestWarmTakeoverRefusedDriverFallsBackAndLeavesNoAddress(t *testing.T) {
	w := newWire(macShared1, macShared2)
	devs := standbyDevs()
	devs["em1"].refuseMAC = true // the driver ignores the MAC change
	d, h := warmNode(t, w, "B", standbyState(RoleStandby, 5), devs)
	d.warmReadiness(d.activeXMLPath(), testSet)
	if ok, why := d.warmReady(); !ok {
		t.Fatal(why)
	}
	if _, err := d.warmActivate(&testPlan, testSet); err == nil {
		t.Fatal("a takeover whose MAC did not change must not be reported as done")
	}
	for _, dev := range []string{"em0", "em1"} {
		got := devs[dev]
		if len(got.inet) != 0 || len(got.inet6) != 0 || got.ether != got.hw {
			t.Fatalf("%s must be back to silent after a failed fast path (the classic path starts from there): %+v\n%q", dev, got, h.commands())
		}
	}
	if len(w.violations) > 0 {
		t.Fatal(w.violations)
	}
}

func TestWarmTakeoverRefusedWhenNotReadyOrNotSilent(t *testing.T) {
	w := newWire(macShared1, macShared2)
	devs := standbyDevs()
	d, _ := warmNode(t, w, "B", standbyState(RoleStandby, 5), devs)
	if _, err := d.warmActivate(&testPlan, testSet); err == nil {
		t.Fatal("readiness was never evaluated")
	}
	d.warmReadiness(d.activeXMLPath(), testSet)
	devs["em1"].inet = []string{"192.168.1.9"} // something configured the port behind our back since the last check
	if _, err := d.warmActivate(&testPlan, testSet); err == nil {
		t.Fatal("the port is not silent: the fast path must not run")
	}
	if devs["em1"].ether == macShared1 {
		t.Fatal("shared MAC set on a port that was not verified")
	}
}

func TestUnknownHardwareMACFallsBackToClassicDown(t *testing.T) {
	// this node carries the shared MAC, the driver prints no hwaddr and nothing was learned: it cannot be given its own back
	w := newWire(macShared1, macShared2)
	devs := standbyDevs()
	devs["em1"].ether, devs["em1"].noHWLine = macShared1, true
	devs["em1"].inet = []string{"192.168.1.1"}
	d, h := warmNode(t, w, "A", activeState(RoleMaster, 6), devs)
	warm := warmDevices(testPlan, testSet.KeepUp)
	d.standbyDevice("em1", warm)
	if devs["em1"].up || len(devs["em1"].inet) != 0 {
		t.Fatalf("classic fallback expected (down, flushed): %+v", devs["em1"])
	}
	if !strings.Contains(strings.Join(h.commands(), "|"), "flush em1") {
		t.Fatalf("%q", h.commands())
	}
	// as a standby the same device is not warm-ready either, and is taken down rather than left on the shared MAC
	devs["em1"].up = true
	d.store.Update(func(s *State) { s.State = StateStandby })
	d.warmReadiness(d.activeXMLPath(), testSet)
	if ok, why := d.warmReady(); ok || !strings.Contains(why, "permanent MAC") {
		t.Fatalf("ok=%v why=%q", ok, why)
	}
	if devs["em1"].up {
		t.Fatal("a device that cannot be made silent must not stay up")
	}
}

func TestConfigRejectsUnknownMode(t *testing.T) {
	dir := t.TempDir()
	for in, want := range map[string]string{`{"ha_mode":"warm"}`: "warm", `{"ha_mode":"turbo"}`: "classic", `{}`: "classic"} {
		p := filepath.Join(dir, "c.json")
		os.WriteFile(p, []byte(in), 0o600)
		t.Setenv("HASYNC_CONF", p)
		if got := loadConfig().HAMode; got != want {
			t.Fatalf("%s -> %q, want %q", in, got, want)
		}
	}
}

// The production layout of one of the users: every network is a VLAN on a LACP LAGG, the WAN too.
func TestVLANsOnALAGGNeverEnterTheWarmSet(t *testing.T) {
	p := sanitize(WarmPlan{Ifaces: []WarmIface{
		{Key: "wan", Dev: "vlan0.20", Parent: "lagg0", MAC: macShared1, V4: "10.0.0.1/24", Eligible: true},
		{Key: "lan", Dev: "vlan0.10", Parent: "lagg0", MAC: macShared2, V4: "192.168.1.1/24", Eligible: true},
		{Key: "x", Dev: "lagg1", MAC: "02:aa:bb:cc:dd:07", V4: "172.16.0.1/24", Eligible: true},
		{Key: "b", Dev: "bridge0", MAC: "02:aa:bb:cc:dd:08", V4: "172.17.0.1/24", Eligible: true},
		{Key: "nested", Dev: "vlan0.30", Parent: "vlan0.10", MAC: "02:aa:bb:cc:dd:09", V4: "172.18.0.1/24", Eligible: true},
		{Key: "nomac", Dev: "igb2", MAC: "00:00:00:00:00:00", V4: "172.19.0.1/24", Eligible: true},
		{Key: "ok", Dev: "igb3", MAC: "02:aa:bb:cc:dd:0a", V4: "172.20.0.1/24", Eligible: true},
	}})
	got := warmDevices(p, nil)
	if !reflect.DeepEqual(got, map[string]string{"igb3": "02:aa:bb:cc:dd:0a"}) {
		t.Fatalf("only the plain NIC may be warm, got %v", got)
	}
	for _, i := range p.Ifaces {
		if i.Key != "ok" && (i.Eligible || i.Reason == "") {
			t.Fatalf("%s must be ineligible with a reason: %+v", i.Key, i)
		}
	}
	// and nothing at all is ever run against the LAGG
	w := newWire(macShared1)
	d, h := warmNode(t, w, "B", standbyState(RoleStandby, 5), map[string]*fakeDev{"lagg0": nic("00:00:00:00:00:00")})
	d.act.plan = func(string) (WarmPlan, error) {
		return WarmPlan{Ifaces: []WarmIface{{Key: "wan", Dev: "vlan0.20", Parent: "lagg0", MAC: macShared1, V4: "10.0.0.1/24", Eligible: true}}}, nil
	}
	d.warmReadiness(d.activeXMLPath(), IfaceSet{ForceDown: []string{"lagg0", "vlan0.20"}})
	if len(h.commands()) != 0 {
		t.Fatalf("commands run against a LAGG: %q", h.commands())
	}
	if ok, _ := d.warmReady(); ok {
		t.Fatal("no interface qualifies: not ready")
	}
}

func TestZeroMACIsNeverAHardwareMAC(t *testing.T) {
	h := openHWMacs(t.TempDir())
	if mac, ok := h.resolve("lagg0", "", ifState{Ether: "3c:fd:fe:2d:bc:40", HW: "00:00:00:00:00:00"}); !ok || mac != "3c:fd:fe:2d:bc:40" {
		t.Fatalf("an all-zero hwaddr must be ignored: %v %v", mac, ok)
	}
	if _, ok := h.resolve("x0", "", ifState{Ether: "00:00:00:00:00:00"}); ok {
		t.Fatal("an all-zero MAC is not usable")
	}
	for in, want := range map[string]bool{"02:aa:bb:cc:dd:01": true, "00:11:22:33:44:55": true, "01:00:5e:00:00:01": false, "00:00:00:00:00:00": false, "zz": false, "": false} {
		if validMAC(in) != want {
			t.Fatalf("validMAC(%q) != %v", in, want)
		}
	}
}

func TestPlanWithMalformedAddressesIsNotWarm(t *testing.T) {
	good := WarmIface{Key: "k", Dev: "igb3", MAC: "02:aa:bb:cc:dd:0a", V4: "172.20.0.1/24", Eligible: true}
	if p := sanitize(WarmPlan{Ifaces: []WarmIface{good}}); !p.Ifaces[0].Eligible {
		t.Fatalf("a good entry was refused: %+v", p.Ifaces[0])
	}
	bad := map[string]func(WarmIface) WarmIface{
		"argument smuggled into the IPv4": func(i WarmIface) WarmIface { i.V4 = "172.20.0.1/24 vhid 5"; return i },
		"no prefix":                       func(i WarmIface) WarmIface { i.V4 = "172.20.0.1"; return i },
		"IPv6 in the IPv4 field":          func(i WarmIface) WarmIface { i.V4 = "2001:db8::1/64"; return i },
		"IPv4 in the IPv6 field":          func(i WarmIface) WarmIface { i.V6 = "172.20.0.2/24"; return i },
		"option in an alias":              func(i WarmIface) WarmIface { i.VIPs = []string{"172.20.0.9/24", "-alias"}; return i },
		"prefix 0":                        func(i WarmIface) WarmIface { i.V4 = "0.0.0.0/0"; return i },
		"absurd MTU":                      func(i WarmIface) WarmIface { i.MTU = 70000; return i },
		"tiny MTU":                        func(i WarmIface) WarmIface { i.MTU = 20; return i },
		"multicast MAC":                   func(i WarmIface) WarmIface { i.MAC = "01:00:5e:00:00:01"; return i },
		"MAC with a trailing argument":    func(i WarmIface) WarmIface { i.MAC = "02:aa:bb:cc:dd:0a up"; return i },
	}
	for name, mutate := range bad {
		p := sanitize(WarmPlan{Ifaces: []WarmIface{mutate(good)}})
		if p.Ifaces[0].Eligible || p.Ifaces[0].Reason == "" {
			t.Errorf("%s was accepted: %+v", name, p.Ifaces[0])
		}
	}
}
