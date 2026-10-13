package main

import (
	"reflect"
	"strings"
	"testing"
)

func vlanDev() *fakeDev {
	return &fakeDev{up: false, ether: macShared1, hw: macShared1, nd6off: false}
}

var fastPlan = WarmPlan{Ifaces: []WarmIface{
	{Key: "opt1", Dev: "vlan0.10", Parent: "lagg0", Fast: true, FastV4: "10.0.10.1/24", FastMAC: macShared1, Descr: "SERVERS (opt1)"},
	{Key: "wan", Dev: "vlan0.20", Parent: "lagg0", Fast: true, FastV4: "203.0.113.2/29", FastMAC: macShared1, Descr: "WAN (wan)",
		VIPs: []string{"203.0.113.3/32", "203.0.113.4/32"}, Promisc: true},
	{Key: "opt2", Dev: "vlan0.30", Parent: "lagg0", Fast: false, FastWhy: "an MTU is configured", FastV4: "10.0.30.1/24", Descr: "VOIP (opt2)"},
	{Key: "opt3", Dev: "vlan0.40", Parent: "lagg0", Fast: true, FastV4: "10.0.40.1/24", FastMAC: macShared1, Descr: "STALE (opt3)"},
	{Key: "opt4", Dev: "vlan0.50", Parent: "lagg0", Fast: true, FastV4: "10.0.50.1/24", FastMAC: "02:aa:bb:cc:dd:50", Descr: "OWNMAC (opt4)"},
}}

func fastNode(t *testing.T) (*Daemon, *fakeHost) {
	t.Helper()
	w := newWire()
	h := w.host("fast", map[string]*fakeDev{
		"vlan0.10": vlanDev(), "vlan0.20": vlanDev(), "vlan0.30": vlanDev(),
		"vlan0.40": {up: false, ether: macShared1, hw: macShared1, inet: []string{"10.0.40.99"}}, // an address left behind
		"vlan0.50": {up: false, ether: "02:00:00:00:00:01", hw: "02:00:00:00:00:01"},             // another MAC than the plan's
	})
	d := newHANode(t, standbyState(RoleStandby, 5))
	d.act.sh = h.sh
	d.act.plan = func(string) (WarmPlan, error) { return fastPlan, nil }
	return d, h
}

func ifaces(keys ...string) []Iface {
	var g []Iface
	for _, k := range keys {
		for _, e := range fastPlan.Ifaces {
			if e.Key == k {
				g = append(g, Iface{Key: k, Dev: e.Dev, Enabled: true})
			}
		}
	}
	return g
}

func TestFastCmds(t *testing.T) {
	got := fastCmds(fastPlan.Ifaces[1])
	want := [][]string{
		{"vlan0.20", "inet", "203.0.113.2/29"},
		{"vlan0.20", "inet6", "-accept_rtadv", "-no_dad", "description", "WAN (wan)", "up", "promisc"},
		{"vlan0.20", "inet", "203.0.113.3/32", "alias"},
		{"vlan0.20", "inet", "203.0.113.4/32", "alias"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q want %q", got, want)
	}
	// "inet <address>" must end every invocation: ifconfig reads whatever follows as a peer address
	for _, c := range got {
		for i, w := range c {
			if w == "inet" && i+2 < len(c) && c[i+2] != "alias" {
				t.Fatalf("something follows the address in %q", c)
			}
		}
	}
}

func TestFastOKRefusesWhatIsNotSafe(t *testing.T) {
	ok := fastPlan.Ifaces[0]
	if !fastOK(ok) {
		t.Fatal("the plain case must be accepted")
	}
	bad := map[string]func(*WarmIface){
		"not marked fast":     func(i *WarmIface) { i.Fast = false },
		"no parent (a NIC)":   func(i *WarmIface) { i.Parent = "" },
		"device with a space": func(i *WarmIface) { i.Dev = "vlan0 .10" },
		"address garbage":     func(i *WarmIface) { i.FastV4 = "10.0.10.1; reboot" },
		"alias garbage":       func(i *WarmIface) { i.VIPs = []string{"x"} },
		"mac garbage":         func(i *WarmIface) { i.FastMAC = "zz" },
		"description":         func(i *WarmIface) { i.Descr = "a\nb" },
	}
	for name, f := range bad {
		c := ok
		f(&c)
		if fastOK(c) {
			t.Errorf("%s must be refused", name)
		}
	}
}

func TestFastTierBringsVLANsUpAndLeavesTheRestToTheStockCode(t *testing.T) {
	d, h := fastNode(t)
	rest := d.fastTier("x", ifaces("opt1", "wan", "opt2", "opt3", "opt4"))
	var keys []string
	for _, i := range rest {
		keys = append(keys, i.Key)
	}
	// opt2 is not fast in the plan, opt3 still had an address (the stock code flushes it); opt4 gets its MAC and is fast
	if !reflect.DeepEqual(keys, []string{"opt2", "opt3"}) {
		t.Fatalf("left to the stock code: %v", keys)
	}
	for _, dev := range []string{"vlan0.10", "vlan0.20", "vlan0.50"} {
		st := d.readIf(dev)
		if !st.Up {
			t.Errorf("%s is not up", dev)
		}
	}
	if st := d.readIf("vlan0.20"); !hasAddr(st.Inet, "203.0.113.2") || !hasAddr(st.Inet, "203.0.113.3") || !hasAddr(st.Inet, "203.0.113.4") || !st.Promisc {
		t.Errorf("wan: addresses or promiscuous mode missing: %+v", st)
	}
	if st := d.readIf("vlan0.50"); !macEq(st.Ether, "02:aa:bb:cc:dd:50") {
		t.Errorf("the planned MAC was not set: %s", st.Ether)
	}
	for _, dev := range []string{"vlan0.30", "vlan0.40"} {
		for _, c := range h.commands() {
			if strings.HasPrefix(c, dev+" ") && strings.Contains(c, " up") {
				t.Errorf("%s must not be touched by the fast path: %s", dev, c)
			}
		}
	}
	// the description travels as ONE argument
	if i := indexOf(h.commands(), "vlan0.10 inet6"); i < 0 || !strings.HasSuffix(h.commands()[i], "description SERVERS (opt1) up") {
		t.Errorf("one ifconfig with the description missing: %v", h.commands())
	}
}

func TestFastTierOffChangesNothing(t *testing.T) {
	d, h := fastNode(t)
	off := false
	d.cfg.FastIfaces = &off
	g := ifaces("opt1", "wan")
	if rest := d.fastTier("x", g); len(rest) != len(g) || len(h.commands()) != 0 {
		t.Fatalf("with the setting off nothing may happen: rest %d, commands %v", len(rest), h.commands())
	}
}

func TestFastTierVerifiesTheResult(t *testing.T) {
	d, h := fastNode(t)
	h.devs["vlan0.10"].refuseMAC = true
	h.devs["vlan0.10"].ether = "02:00:00:00:00:99" // the driver will not take the planned MAC
	if rest := d.fastTier("x", ifaces("opt1")); len(rest) != 1 {
		t.Fatalf("a VLAN that did not take the planned MAC must go to the stock code, got rest=%d", len(rest))
	}
}
