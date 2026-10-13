package main

import "testing"

const lagg0Out = `lagg0: flags=1008843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST,LOWER_UP> metric 0 mtu 1500
	options=48500b8<VLAN_MTU,VLAN_HWTAGGING>
	ether 02:07:11:45:89:81
	hwaddr 00:00:00:00:00:00
	laggproto failover lagghash l2,l3,l4
	laggport: ixl0 flags=1<MASTER>
`

func vlanOut(mac string) string {
	return `vlan0.65: flags=1008843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST,LOWER_UP> metric 0 mtu 1500
	options=4000000<MEXTPG>
	ether ` + mac + `
	inet 192.168.65.254 netmask 0xffffff00 broadcast 192.168.65.255
	groups: vlan
	vlan: 65 vlanproto: 802.1q vlanpcp: 0 parent interface: lagg0
	media: Ethernet autoselect
`
}

func TestVlanParent(t *testing.T) {
	if p := vlanParent(vlanOut("02:5f:92:db:00:83")); p != "lagg0" {
		t.Fatalf("parent = %q", p)
	}
	if p := vlanParent(lagg0Out); p != "" {
		t.Fatalf("a LAGG has no VLAN parent, got %q", p)
	}
}

// the X710 case: the VLAN has its own MAC, the NIC's receive filter only knows the parent's
func TestOwnMACVLAN(t *testing.T) {
	if !ownMACVLAN(vlanOut("02:5f:92:db:00:83"), lagg0Out) {
		t.Fatal("a VLAN with another MAC than its parent needs promiscuous mode")
	}
	if ownMACVLAN(vlanOut("02:07:11:45:89:81"), lagg0Out) {
		t.Fatal("a VLAN sharing the MAC of its parent needs nothing")
	}
	if ownMACVLAN(vlanOut("02:07:11:45:89:81"), "") || ownMACVLAN(lagg0Out, lagg0Out) {
		t.Fatal("unknown parent or no VLAN: nothing to do")
	}
	if ownMACVLAN("", lagg0Out) {
		t.Fatal("missing device: nothing to do")
	}
}
