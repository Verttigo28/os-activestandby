package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func demotionNode(t *testing.T, h *fakeHost, devs ...string) *Daemon {
	t.Helper()
	d := newHANode(t, activeState(RoleMaster, 7))
	d.health, d.drift, d.leases, d.sess = newHealthState(), &driftState{}, &leaseState{}, newSessionSync()
	cfg := filepath.Join(d.store.dir, "config.xml")
	t.Setenv("HASYNC_CONFIG", cfg)
	os.WriteFile(cfg, []byte("<?xml version=\"1.0\"?><opnsense>"+strings.Repeat("<x>filler</x>", 30)+"</opnsense>\n"), 0o600)
	old := loadIfacesHook
	loadIfacesHook = func(string) (IfaceSet, error) {
		return IfaceSet{ForceDown: devs, HAKey: "opt9"}, nil
	}
	t.Cleanup(func() { loadIfacesHook = old })
	d.act.sh = h.sh
	d.act.standbyView = func(string) error { return nil }
	return d
}

// Reported by the audit of 09281e5: flushDev ignored the failure of "ifconfig down", and the demotion declared the devices down
// anyway and let the peer take over while this node still had the shared identity on an interface that is up.
func TestDemotionDoesNotLetThePeerTakeOverWhileADeviceIsStillUp(t *testing.T) {
	h := newWire(macShared1, macShared2).host("A", map[string]*fakeDev{
		"vlan0.10": active("10.0.10.1", macShared1),
		"vlan0.20": active("10.0.20.1", macShared2),
	})
	h.devs["vlan0.20"].refuseDown = true // the driver refuses
	d := demotionNode(t, h, "vlan0.10", "vlan0.20")
	released := false
	err := d.demoteHook("test", func() { released = true })
	if released {
		t.Fatal("the peer was allowed to take over while vlan0.20 is still up with the shared address")
	}
	if err == nil || !strings.Contains(err.Error(), "vlan0.20") {
		t.Fatalf("the failure must be reported with the device: %v", err)
	}
	if d.store.Get().State != StateActive {
		t.Fatal("a node that still carries the identity must not call itself standby")
	}
	if h.devs["vlan0.10"].up {
		t.Fatal("the devices that did go down stay down")
	}
}

// every device goes down: the peer is released and the node is standby
func TestDemotionThatWorksReleasesThePeer(t *testing.T) {
	h := newWire(macShared1).host("A", map[string]*fakeDev{"vlan0.10": active("10.0.10.1", macShared1)})
	d := demotionNode(t, h, "vlan0.10")
	released := false
	if err := d.demoteHook("test", func() { released = true }); err != nil {
		t.Fatal(err)
	}
	if !released || d.store.Get().State != StateStandby || h.devs["vlan0.10"].up || len(h.devs["vlan0.10"].inet) != 0 {
		t.Fatalf("released=%v state=%s %+v", released, d.store.Get().State, h.devs["vlan0.10"])
	}
}

// the second try of the demotion works when the first one failed only for a moment
func TestDemotionTriesAFailedDeviceAgain(t *testing.T) {
	h := newWire(macShared1).host("A", map[string]*fakeDev{"vlan0.10": active("10.0.10.1", macShared1)})
	h.devs["vlan0.10"].refuseDown = true
	d := demotionNode(t, h, "vlan0.10")
	calls := 0
	base := d.act.sh
	d.act.sh = func(name string, args ...string) (string, error) {
		if name == "ifconfig" && len(args) == 2 && args[1] == "down" {
			calls++
			if calls >= 3 { // refused twice, then it works
				h.w.mu.Lock()
				h.devs["vlan0.10"].refuseDown = false
				h.w.mu.Unlock()
			}
		}
		return base(name, args...)
	}
	released := false
	if err := d.demoteHook("test", func() { released = true }); err != nil || !released {
		t.Fatalf("err=%v released=%v", err, released)
	}
}

// an active device: up, with its address and the shared MAC
func active(ip, mac string) *fakeDev {
	return &fakeDev{up: true, carrier: true, ether: mac, hw: "00:11:22:33:44:55", inet: []string{ip}}
}
