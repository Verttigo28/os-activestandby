package main

import "strings"

// A VLAN interface that carries its own MAC (not the one of its parent) only receives unicast frames when the NIC
// is promiscuous: the receive filter of a real NIC (measured on an Intel X710 under a failover LAGG) holds the MAC of
// the parent only, the vlan(4) driver does not program the child's MAC into it. ARP broadcasts still arrive, the
// replies to them do not, and the interface looks fine while no unicast packet ever reaches it. OPNsense knows the
// problem ("promisc ... required for e.g. VLAN MAC spoofing" in interfaces.inc).

// vlanParent returns the parent device from the ifconfig output of a VLAN ("... parent interface: lagg0"), "" for
// anything that is not a VLAN.
func vlanParent(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if i := strings.Index(l, "parent interface:"); i >= 0 {
			return strings.TrimSpace(l[i+len("parent interface:"):])
		}
	}
	return ""
}

// ownMACVLAN: the VLAN device dev exists and its MAC differs from its parent's.
func ownMACVLAN(devOut, parentOut string) bool {
	if vlanParent(devOut) == "" {
		return false
	}
	d, p := parseIfconfig(devOut), parseIfconfig(parentOut)
	return d.Ether != "" && p.Ether != "" && !macEq(d.Ether, p.Ether)
}

// vlanNeedsPromisc reads the live state of dev; true when it is a VLAN with a MAC of its own.
func vlanNeedsPromisc(dev string) bool {
	out, err := sh("ifconfig", dev)
	if err != nil {
		return false
	}
	parent := vlanParent(out)
	if parent == "" {
		return false
	}
	pout, err := sh("ifconfig", parent)
	if err != nil {
		return false
	}
	return ownMACVLAN(out, pout)
}
