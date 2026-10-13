package main

import (
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Hot standby (setting "High availability mode" = hot). EXPERIMENTAL, opt-in.
//
// The warm standby keeps the links of the shared interfaces up with the OWN MAC of each port and takes the shared MAC at the
// takeover. On the hardware this was measured on (Intel X710) changing the MAC of a port makes its link drop for 0.74 s, so
// that takeover is no faster than the classic one. The hot standby does the one thing that costs a link event ahead of time:
//
//	standby:  LAGG, its ports and the VLANs are up, carry the SHARED MAC, have no address and IPv6 off  -> silent
//	takeover: add the addresses (a few ms), announce                                                   -> no link event
//	stand down: remove the addresses, nothing else
//
// Why this is safe on the wire: a port that is up with a MAC but no address sends nothing with it (no ARP, no ND, no MLD
// because IPv6 is off before the port comes up). Measured on the target hardware with the switch as observer: 45 s of a
// standby with the LAGG and both X710 ports up and carrying the shared MAC: 2381 frames, all LLDP from the firmware agent
// with the permanent MAC of the port, none with the shared MAC, and the switch kept the shared MAC on the active node's
// port. Not every NIC behaves like this, so the daemon watches the software transmit counter of every silent device on the
// standby: a device that transmits anything is shut down, the standby reports "not ready" and the takeover is classic.
// The pf ruleset of a standby must have its default deny (a standby that receives frames for the shared MAC, for example
// when the switch floods an unknown unicast, must not route them): the readiness checks it.
//
// What it cannot do: LACP. A standby LAGG in LACP mode would send LACP PDUs with the shared MAC as system id and a switch
// could merge both firewalls into one LAG, so a LACP LAGG is never hot (the plan says so); its VLANs can still be hot, with
// the shared MAC on the VLAN only and the LAGG keeping its own MAC (it then runs promiscuous so that it accepts it).

const hotParent = "promisc" // value of hotDevices for a device that stays up with its own MAC and must accept the VLAN MAC

// a device that transmitted while it had to be silent stays down this long (a device that leaks every few seconds must not come
// back "ready" at the next pass of the enforcer)
const hotLeakLatch = 10 * time.Minute

var hotLaggProto = map[string]bool{"failover": true, "loadbalance": true, "roundrobin": true, "lacp": true}

// hotView answers the hot standby's questions from the plan: which interfaces take the shared MAC and addresses natively.
func hotView(p WarmPlan, lacpShared bool) WarmPlan {
	out := WarmPlan{Ifaces: make([]WarmIface, len(p.Ifaces)), Laggs: p.Laggs, Hot: true}
	copy(out.Ifaces, p.Ifaces)
	proto := map[string]string{}
	for _, l := range p.Laggs {
		proto[l.Dev] = l.Proto
	}
	for i := range out.Ifaces {
		e := &out.Ifaces[i]
		e.Eligible, e.Reason = false, e.HotWhy
		e.V4, e.V6 = e.HotV4, e.HotV6
		if !e.Hot {
			continue
		}
		pr, isLagg := proto[e.Dev]
		switch {
		case !fastDevRe.MatchString(e.Dev):
			e.Reason = "unusable device name"
		case e.Parent == "" && !plainNIC(e.Dev) && !isLagg:
			e.Reason = e.Dev + " is neither a NIC nor a LAGG"
		case isLagg && !hotLaggProto[pr]:
			e.Reason = "the LAGG protocol " + pr + " is not supported by the hot standby"
		case isLagg && pr == "lacp" && !lacpShared:
			e.Reason = "LACP LAGG: the setting Hot standby and LACP is off (a hot LACP standby needs one static port-channel per firewall)"
		case e.Parent != "" && !plainNIC(e.Parent) && !strings.HasPrefix(e.Parent, "lagg"):
			e.Reason = "the VLAN sits on " + e.Parent
		case !validMAC(e.MAC) || e.V4 == "":
			e.Reason = "no usable shared MAC or static IPv4 address"
		case !validCIDR(e.V4, 4) || (e.V6 != "" && !validCIDR(e.V6, 6)) || !allCIDR(e.VIPs, 4):
			e.Reason = "an address of the plan is not a valid address/prefix"
		case e.MTU != 0 && (e.MTU < 576 || e.MTU > 9216):
			e.Reason = "the MTU is out of range"
		default:
			e.Eligible, e.Reason = true, ""
		}
	}
	// a VLAN is only as hot as an assigned parent: that parent must be hot itself
	byDev := map[string]int{}
	for i, e := range out.Ifaces {
		byDev[e.Dev] = i
	}
	for i := range out.Ifaces {
		e := &out.Ifaces[i]
		if j, ok := byDev[e.Parent]; ok && e.Eligible && !out.Ifaces[j].Eligible {
			e.Eligible, e.Reason = false, "its parent "+e.Parent+" cannot be hot: "+out.Ifaces[j].Reason
		}
	}
	return out
}

// hotDevices: the devices that stay up on a hot standby. The value is the shared MAC for an interface of the plan, hotParent
// for a device that keeps its own MAC and accepts the VLAN MAC (a NIC or LAGG that is not itself a shared interface) and
// "" for a LAGG port (it follows the MAC of its LAGG).
func hotDevices(p WarmPlan, keepUp []string) map[string]string {
	keep := map[string]bool{}
	for _, k := range keepUp {
		keep[k] = true
	}
	out := map[string]string{}
	elig := map[string]bool{}
	for _, i := range p.Ifaces {
		if i.Eligible && !keep[i.Dev] {
			out[i.Dev] = i.MAC
			elig[i.Dev] = true
		}
	}
	for _, i := range p.Ifaces {
		if i.Eligible && i.Parent != "" && !keep[i.Parent] && !elig[i.Parent] && devNameRe.MatchString(i.Parent) {
			out[i.Parent] = hotParent
		}
	}
	for _, l := range p.Laggs {
		if _, ok := out[l.Dev]; !ok {
			continue
		}
		for _, m := range l.Members {
			// a name from the replicated configuration becomes an argument of ifconfig: plain names only
			if _, ok := out[m]; !ok && !keep[m] && devNameRe.MatchString(m) {
				out[m] = ""
			}
		}
	}
	return out
}

// silentHot: nothing of the identity is on the device but the MAC, and the link is up.
func (s ifState) silentHot(shared string) bool {
	if !s.Exists || !s.Up || !s.Disabled || len(s.Inet) > 0 || len(s.Inet6) > 0 {
		return false
	}
	return shared == "" || shared == hotParent || macEq(s.Ether, shared)
}

// hotSilenceCmds brings a device to the hot standby condition. IPv6 goes off BEFORE the device comes up, and the shared MAC is
// set while it is not transmitting anything, so nothing leaves the port with a MAC that the active node owns.
func hotSilenceCmds(dev, shared string, st ifState) [][]string {
	c := [][]string{{"ifconfig", dev, "inet6", "ifdisabled"}}
	for _, a := range st.Inet {
		c = append(c, []string{"ifconfig", dev, "inet", a, "-alias"})
	}
	for _, a := range st.Inet6 {
		c = append(c, []string{"ifconfig", dev, "inet6", a, "-alias"})
	}
	switch shared {
	case "":
	case hotParent:
		c = append(c, []string{"ifconfig", dev, "promisc"})
	default:
		if !macEq(st.Ether, shared) {
			c = append(c, []string{"ifconfig", dev, "link", shared})
		}
	}
	return append(c, []string{"ifconfig", dev, "up"})
}

func (d *Daemon) hotSilence(dev, shared string) (ifState, error) {
	st := d.readIf(dev)
	if !st.Exists {
		return st, fmt.Errorf("%s does not exist", dev)
	}
	// a device that transmitted while silent is not brought up again for a while: the caller keeps it down (classic standby)
	hotTxMu.Lock()
	at, latched := hotLeak[dev]
	if latched && time.Since(at) > hotLeakLatch {
		delete(hotLeak, dev)
		latched = false
	}
	hotTxMu.Unlock()
	if latched {
		return st, fmt.Errorf("%s transmitted while it had to be silent (%s): kept down, the takeover will be classic", dev, at.Format("15:04:05"))
	}
	if st.silentHot(shared) && (shared != hotParent || st.Promisc) {
		return st, nil
	}
	log.Printf("hot standby: silencing %s", dev)
	if err := d.runAll(hotSilenceCmds(dev, shared, st)); err != nil {
		return st, err
	}
	d.hotRebase(dev) // whatever these commands sent (or the node sent while it was active) is not a leak
	after := d.readIf(dev)
	if !after.silentHot(shared) {
		return after, fmt.Errorf("%s is not silent after the standby commands (up %v, mac %s, addresses %v)", dev, after.Up, after.Ether, after.Inet)
	}
	return after, nil
}

// hotActivateCmds gives the addresses to a device that already has the shared MAC.
func hotActivateCmds(i WarmIface) [][]string {
	c := [][]string{{"ifconfig", i.Dev, "inet6", "-ifdisabled"}}
	if i.MTU > 0 {
		c = append(c, []string{"ifconfig", i.Dev, "mtu", strconv.Itoa(i.MTU)})
	}
	c = append(c, []string{"ifconfig", i.Dev, "inet", i.V4})
	for _, v := range i.VIPs {
		c = append(c, []string{"ifconfig", i.Dev, "inet", v, "alias"})
	}
	if i.V6 != "" {
		c = append(c, []string{"ifconfig", i.Dev, "inet6", i.V6})
	}
	return c
}

// hotActivate takes the shared addresses on every interface of the plan. Verify, never assume: a device that is not in the hot
// state, or does not have the planned state afterwards, undoes what was done here and makes the takeover classic.
func (d *Daemon) hotActivate(p *WarmPlan, set IfaceSet) (map[string]bool, error) {
	if ok, why := d.warmReady(); !ok {
		return nil, fmt.Errorf("standby not ready: %s", why)
	}
	keep := map[string]bool{}
	for _, k := range set.KeepUp {
		keep[k] = true
	}
	done := map[string]bool{}
	var activated []WarmIface
	revert := func() {
		for i := len(activated) - 1; i >= 0; i-- {
			d.hotSilence(activated[i].Dev, activated[i].MAC)
		}
	}
	for _, i := range eligibleOrdered(*p) {
		if keep[i.Dev] {
			continue
		}
		st := d.readIf(i.Dev)
		if !st.silentHot(i.MAC) {
			revert()
			return nil, fmt.Errorf("%s is not in the hot state (up, shared MAC, no address) at takeover", i.Dev)
		}
		activated = append(activated, i)
		if err := d.runAll(hotActivateCmds(i)); err != nil {
			revert()
			return nil, err
		}
		after := d.readIf(i.Dev)
		if !macEq(after.Ether, i.MAC) || !after.Up || !hasAddr(after.Inet, i.V4) {
			revert()
			return nil, fmt.Errorf("%s did not take its addresses (mac %s, up %v, addresses %v)", i.Dev, after.Ether, after.Up, after.Inet)
		}
		done[i.Key] = true
		hotTxMu.Lock()
		delete(hotTxLast, i.Dev) // an active device transmits: its counter starts a new baseline when it is silent again
		hotTxMu.Unlock()
	}
	if len(done) == 0 {
		return nil, fmt.Errorf("no interface to activate natively")
	}
	return done, nil
}

// ---- watching the silence ----

// hotRebase records the current transmit counter of dev as the silent baseline.
func (d *Daemon) hotRebase(dev string) {
	out, err := d.run("netstat", "-I", dev, "-n", "-b")
	if err != nil {
		return
	}
	if n, ok := txPackets(out); ok {
		hotTxMu.Lock()
		hotTxLast[dev] = n
		hotTxMu.Unlock()
	}
}

var (
	// the loaded form ("block drop in log inet all") and the generated source ("block in log inet from {any} to {any}")
	pfDefaultDenyRe = regexp.MustCompile(`(?m)^block (drop )?in (log )?(inet6? )?(all|from \{?any\}? to \{?any\}?)( |$)`)
	hotTxMu         sync.Mutex
	hotTxLast       = map[string]uint64{}
	hotLeak         = map[string]time.Time{} // devices that transmitted while silent: kept down for hotLeakLatch (guarded by hotTxMu)
)

// pfDefaultDeny: the loaded ruleset blocks what no rule passes. pf passes by default, so without this line a standby that
// receives a frame for the shared MAC would route it.
func pfDefaultDeny(rules string) bool { return pfDefaultDenyRe.MatchString(rules) }

// txPackets reads the software transmit counter of a device from `netstat -I dev -n -b`: the Opkts column of the link row.
func txPackets(out string) (uint64, bool) {
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) >= 11 && strings.Contains(l, "<Link#") {
			n, err := strconv.ParseUint(f[len(f)-4], 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// hotChecks runs on the standby after the devices were made silent. It returns the problems that make the standby "not ready".
func (d *Daemon) hotChecks(p *WarmPlan, devs map[string]string) []string {
	lacp := map[string]bool{} // an LACP LAGG sends its protocol frames (LACPDUs, link-local: a bridge does not forward or learn them): not a leak
	for _, l := range p.Laggs {
		if l.Proto == "lacp" {
			lacp[l.Dev] = true
		}
	}
	var problems []string
	// a safety check fails closed: what cannot be read is not "fine"
	if out, err := d.run("pfctl", "-sr"); err != nil {
		problems = append(problems, "the packet filter of this standby cannot be read: "+strings.TrimSpace(out))
	} else if !pfDefaultDeny(out) {
		problems = append(problems, "the packet filter of this standby has no default deny rule: a frame for the shared MAC would be routed")
	}
	names := make([]string, 0, len(devs))
	for dev, v := range devs {
		if v != "" && v != hotParent && !lacp[dev] { // the interfaces that carry the shared MAC (a LAGG port or a parent has none to leak)
			names = append(names, dev)
		}
	}
	sort.Strings(names)
	hotTxMu.Lock()
	defer hotTxMu.Unlock()
	for _, dev := range names {
		out, err := d.run("netstat", "-I", dev, "-n", "-b")
		n, ok := txPackets(out)
		if err != nil || !ok {
			problems = append(problems, dev+": its transmit counter cannot be read")
			continue
		}
		if last, seen := hotTxLast[dev]; seen && n > last {
			problems = append(problems, fmt.Sprintf("%s transmitted %d packet(s) while silent: shut down, the takeover will be classic", dev, n-last))
			d.event("error", "hot standby: %s transmitted %d packet(s) while it should be silent: shutting it down for %s", dev, n-last, hotLeakLatch)
			d.flush(dev)
			delete(hotTxLast, dev)
			hotLeak[dev] = time.Now() // latched: the next pass of the enforcer must not bring it back
			continue
		}
		hotTxLast[dev] = n
	}
	return problems
}
