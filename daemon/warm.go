package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Warm standby (setting "High availability mode" = warm). EXPERIMENTAL: opt-in, classic mode is the default and stays the
// fallback for everything below.
//
// Classic standby: every data interface is administratively down. A takeover has to bring the link up (a real NIC
// renegotiates it with the switch: seconds), run the PHP interface code per interface and wait for the carrier.
//
// Warm standby: the NICs and VLANs of the shared interfaces stay UP with their OWN hardware MAC address, without any IP
// address and with IPv6 disabled, so the standby is silent on the wire (no ARP/ND answers, nothing routed) but its link
// stays up. The shared identity belongs to the active node only:
//
//	takeover: set the shared MAC, add the addresses, announce            (native ifconfig, a few ms)
//	stand down: set the own MAC back FIRST (the NIC stops accepting frames for the shared MAC), then remove the addresses
//
// Only interfaces the plan marks eligible (static IPv4, shared MAC, plain NIC or VLAN) are warm; all others keep the
// classic takeover. Any failure of the fast path falls back to the classic path before traffic is started. What the
// fast path does NOT do is run OPNsense's own interface code: routes, pf, DHCP/DNS and services still run afterwards
// exactly as in the classic takeover.

// ---- plan ----

type WarmIface struct {
	Key      string   `json:"key"`
	Dev      string   `json:"dev"`
	MAC      string   `json:"mac"` // the shared MAC
	MTU      int      `json:"mtu"`
	V4       string   `json:"v4"` // address/prefix
	V6       string   `json:"v6"`
	VIPs     []string `json:"vips"` // IPv4 alias addresses, address/prefix
	Parent   string   `json:"parent"`
	Eligible bool     `json:"eligible"`
	Reason   string   `json:"reason"`

	// fast classic takeover (fastpath.go): a VLAN with a static IPv4 address, independent of the warm eligibility
	Fast    bool   `json:"fast"`
	FastWhy string `json:"fast_why"`
	FastV4  string `json:"fast_v4"`  // address/prefix
	FastMAC string `json:"fast_mac"` // the interface's MAC from the configuration, "" when none is set
	Descr   string `json:"descr"`    // interface description, as OPNsense prints it
	Promisc bool   `json:"promisc"`  // the interface has the promiscuous option

	// hot standby (hot.go): the same questions with LAGG allowed
	Hot    bool   `json:"hot"`
	HotWhy string `json:"hot_why"`
	HotV4  string `json:"hot_v4"`
	HotV6  string `json:"hot_v6"`
}

type WarmPlan struct {
	Ifaces []WarmIface `json:"ifaces"`
	Laggs  []LaggInfo  `json:"laggs"`
	Hot    bool        `json:"-"` // set by hotView: Eligible/V4/V6 then answer the hot standby's questions
}

type LaggInfo struct {
	Dev     string   `json:"dev"`
	Proto   string   `json:"proto"`
	Members []string `json:"members"`
}

func loadPlan(path string) (WarmPlan, error) {
	var p WarmPlan
	out, err := exec.Command(ifacesScript, "plan", path).Output()
	if err != nil {
		return p, fmt.Errorf("ifaces.php plan: %w", err)
	}
	return p, json.Unmarshal(out, &p)
}

// warmDevices returns the devices that stay up on the standby, with their shared MAC ("" for a VLAN parent that is not a
// shared interface itself). Devices the daemon must never touch (HA/management) are excluded.
func warmDevices(p WarmPlan, keepUp []string) map[string]string {
	if p.Hot {
		return hotDevices(p, keepUp)
	}
	keep := map[string]bool{}
	for _, k := range keepUp {
		keep[k] = true
	}
	out := map[string]string{}
	for _, i := range p.Ifaces {
		if !i.Eligible || keep[i.Dev] {
			continue
		}
		out[i.Dev] = i.MAC
	}
	for _, i := range p.Ifaces {
		if i.Eligible && i.Parent != "" && !keep[i.Parent] {
			if _, ok := out[i.Parent]; !ok {
				out[i.Parent] = ""
			}
		}
	}
	return out
}

// eligibleOrdered: parents before the VLANs on them.
func eligibleOrdered(p WarmPlan) []WarmIface {
	var out []WarmIface
	for _, i := range p.Ifaces {
		if i.Eligible && i.Parent == "" {
			out = append(out, i)
		}
	}
	for _, i := range p.Ifaces {
		if i.Eligible && i.Parent != "" {
			out = append(out, i)
		}
	}
	return out
}

// ---- reading the interface ----

type ifState struct {
	Exists   bool
	Up       bool
	Promisc  bool
	Carrier  bool
	Ether    string   // current MAC
	HW       string   // permanent MAC when ifconfig prints it (only when it differs from Ether)
	Inet     []string // IPv4 addresses
	Inet6    []string // global/unique IPv6 addresses (link-local ones are not listed)
	Disabled bool     // IPv6 disabled (IFDISABLED)
}

func parseIfconfig(out string) ifState {
	var s ifState
	lines := strings.Split(out, "\n")
	if len(lines) == 0 || strings.TrimSpace(out) == "" {
		return s
	}
	s.Exists = true
	if i, j := strings.Index(lines[0], "<"), strings.Index(lines[0], ">"); i >= 0 && j > i {
		for _, f := range strings.Split(lines[0][i+1:j], ",") {
			switch f {
			case "UP":
				s.Up = true
			case "PROMISC":
				s.Promisc = true
			}
		}
	}
	nd6 := false
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "ether":
			if s.Ether == "" {
				s.Ether = strings.ToLower(f[1])
			}
		case "hwaddr":
			s.HW = strings.ToLower(f[1])
		case "inet":
			s.Inet = append(s.Inet, f[1])
		case "inet6":
			if !strings.HasPrefix(strings.ToLower(f[1]), "fe80") {
				s.Inet6 = append(s.Inet6, f[1])
			}
		case "status:":
			s.Carrier = f[1] == "active"
		case "nd6":
			nd6 = true
			s.Disabled = strings.Contains(l, "IFDISABLED")
		}
	}
	if !nd6 {
		s.Disabled = true // no IPv6 on this device: nothing to disable
	}
	return s
}

func macEq(a, b string) bool { return a != "" && strings.EqualFold(a, b) }

// ---- command sequences (pure: the order is what keeps the shared identity on one node) ----

// activateCmds gives the shared identity to dev: MAC first (so nothing is announced with the old one), then the addresses.
func activateCmds(i WarmIface) [][]string {
	c := [][]string{
		{"ifconfig", i.Dev, "link", i.MAC},
		{"ifconfig", i.Dev, "inet6", "-ifdisabled"},
	}
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
	return append(c, []string{"ifconfig", i.Dev, "up"})
}

// silenceCmds takes the shared identity away from dev but keeps the link up: the own MAC goes back FIRST, so the NIC stops
// accepting frames addressed to the shared MAC before anything else changes; then every address goes, then IPv6 is disabled.
func silenceCmds(dev, hw string, st ifState) [][]string {
	var c [][]string
	if hw != "" && !macEq(st.Ether, hw) {
		c = append(c, []string{"ifconfig", dev, "link", hw})
	}
	c = append(c, []string{"ifconfig", dev, "-promisc"})
	for _, a := range st.Inet {
		c = append(c, []string{"ifconfig", dev, "inet", a, "-alias"})
	}
	for _, a := range st.Inet6 {
		c = append(c, []string{"ifconfig", dev, "inet6", a, "-alias"})
	}
	return append(c, []string{"ifconfig", dev, "inet6", "ifdisabled"}, []string{"ifconfig", dev, "up"})
}

// silent: nothing of the shared identity is left on the device and the link is up.
func (s ifState) silent(hw string) bool {
	return s.Exists && s.Up && !s.Promisc && macEq(s.Ether, hw) && len(s.Inet) == 0 && len(s.Inet6) == 0 && s.Disabled
}

// ---- hardware MAC memory ----

type hwMacs struct {
	mu   sync.Mutex
	path string
	m    map[string]string
}

func openHWMacs(dir string) *hwMacs {
	h := &hwMacs{path: filepath.Join(dir, "hwmacs.json"), m: map[string]string{}}
	if b, err := os.ReadFile(h.path); err == nil {
		json.Unmarshal(b, &h.m)
	}
	return h
}

// resolve returns the permanent MAC of dev. ifconfig prints it ("hwaddr") when it differs from the current one; otherwise the
// current MAC is the permanent one, unless it is the shared MAC. What is learned is remembered on disk, so a device that
// carries the shared MAC (an active node) can still be given its own back.
func (h *hwMacs) resolve(dev, shared string, st ifState) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	learn := func(mac string) (string, bool) {
		if h.m[dev] != mac {
			h.m[dev] = mac
			if b, err := json.Marshal(h.m); err == nil {
				writeFileDurable(h.path, b, 0o600)
			}
		}
		return mac, true
	}
	switch {
	case validMAC(st.HW):
		return learn(st.HW)
	case validMAC(st.Ether) && !macEq(st.Ether, shared):
		return learn(st.Ether)
	}
	mac, ok := h.m[dev]
	return mac, ok
}

// ---- daemon side ----

// warmStatus is what the status, the heartbeat and the pre-flight report.
type warmStatus struct {
	mu      sync.Mutex
	ready   bool
	reason  string
	warm    []string
	classic []string
	at      time.Time
}

func (d *Daemon) warmOn() bool { return d.cfg.HAMode == "warm" || d.cfg.HAMode == "hot" }

// hotOn: the standby keeps its links up WITH the shared MAC already in place (hot.go).
func (d *Daemon) hotOn() bool { return d.cfg.HAMode == "hot" }

// modeView: the plan as the standby mode asks the questions.
func (d *Daemon) modeView(p WarmPlan) WarmPlan {
	if d.hotOn() {
		return hotView(p, d.cfg.HotLACP == "shared")
	}
	return sanitize(p)
}

func (d *Daemon) run(name string, args ...string) (string, error) {
	if d.act != nil && d.act.sh != nil {
		return d.act.sh(name, args...)
	}
	return sh(name, args...)
}

func (d *Daemon) runAll(cmds [][]string) error {
	var first error
	for _, c := range cmds {
		if out, err := d.run(c[0], c[1:]...); err != nil && first == nil {
			first = fmt.Errorf("%s: %v: %s", strings.Join(c, " "), err, out)
		}
	}
	return first
}

func (d *Daemon) readIf(dev string) ifState {
	out, _ := d.run("ifconfig", dev)
	return parseIfconfig(out)
}

// warmPlanFor returns the plan of the full configuration (cached by file time). nil when the warm standby is off.
func (d *Daemon) warmPlanFor(path string) (*WarmPlan, error) {
	if !d.warmOn() {
		return nil, nil
	}
	return d.planFor(path)
}

// planFor returns the plan of the full configuration (cached by file time), whatever the standby mode.
func (d *Daemon) planFor(path string) (*WarmPlan, error) {
	if d.act != nil && d.act.plan != nil {
		p, err := d.act.plan(path)
		p = d.modeView(p)
		return &p, err
	}
	d.ops.planMu.Lock()
	defer d.ops.planMu.Unlock()
	fi, err := os.Stat(path)
	if err == nil && d.ops.plan != nil && d.ops.planPath == path && fi.ModTime().Equal(d.ops.planMtime) {
		return d.ops.plan, nil
	}
	p, err := loadPlan(path)
	if err != nil {
		return nil, err
	}
	p = d.modeView(p)
	if fi != nil {
		d.ops.plan, d.ops.planMtime, d.ops.planPath = &p, fi.ModTime(), path
	}
	return &p, nil
}

// warmSilence makes one warm device silent (own MAC, no addresses, IPv6 off, link up). It returns an error when the
// permanent MAC is not known: the caller then treats the device the classic way (down).
func (d *Daemon) warmSilence(dev, shared string) (ifState, error) {
	if d.hotOn() {
		return d.hotSilence(dev, shared)
	}
	st := d.readIf(dev)
	if !st.Exists {
		return st, fmt.Errorf("%s does not exist", dev)
	}
	hw, ok := d.hwm().resolve(dev, shared, st)
	if !ok {
		return st, fmt.Errorf("the permanent MAC address of %s is not known (it carries the shared one)", dev)
	}
	if st.silent(hw) {
		return st, nil
	}
	log.Printf("warm standby: silencing %s (own MAC %s)", dev, hw)
	if err := d.runAll(silenceCmds(dev, hw, st)); err != nil {
		return st, err
	}
	return d.readIf(dev), nil
}

// standbyDevice puts a device in its standby condition: silent and up when it is warm, flushed and down otherwise.
func (d *Daemon) standbyDevice(dev string, warm map[string]string) {
	if shared, ok := warm[dev]; ok {
		_, err := d.warmSilence(dev, shared)
		if err == nil {
			return
		}
		log.Printf("warm standby: %v: using the classic standby for %s", err, dev)
	}
	d.flush(dev)
}

func (d *Daemon) flush(dev string) {
	if d.act != nil && d.act.flush != nil {
		d.act.flush(dev)
		return
	}
	flushDev(dev)
}

// warmSet is the set of devices to keep up for this config (empty in classic mode or when no plan can be made).
func (d *Daemon) warmSet(path string, set IfaceSet) map[string]string {
	p, err := d.warmPlanFor(path)
	if err != nil || p == nil {
		if err != nil {
			d.setWarm(false, "no warm plan: "+err.Error(), nil, nil)
		}
		return nil
	}
	return warmDevices(*p, set.KeepUp)
}

func (d *Daemon) setWarm(ready bool, reason string, warm, classic []string) {
	d.wst.mu.Lock()
	d.wst.ready, d.wst.reason, d.wst.warm, d.wst.classic, d.wst.at = ready, reason, warm, classic, time.Now()
	d.wst.mu.Unlock()
}

// warmReadiness evaluates, on the standby, whether a fast takeover is possible right now, and keeps the devices silent.
// Caller holds ops.mu. The result is "not ready" with the reason whenever any precondition fails; the takeover then uses the
// classic path.
func (d *Daemon) warmReadiness(path string, set IfaceSet) {
	if !d.warmOn() {
		d.setWarm(false, "", nil, nil)
		return
	}
	p, err := d.warmPlanFor(path)
	if err != nil || p == nil {
		d.setWarm(false, fmt.Sprintf("no warm plan: %v", err), nil, nil)
		return
	}
	var warm, classic, problems []string
	for _, i := range p.Ifaces {
		if !i.Eligible {
			classic = append(classic, i.Key+": "+i.Reason)
		}
	}
	devs := warmDevices(*p, set.KeepUp)
	names := make([]string, 0, len(devs))
	for dev := range devs {
		names = append(names, dev)
	}
	sort.Strings(names)
	for _, dev := range names {
		st, err := d.warmSilence(dev, devs[dev])
		if err != nil {
			problems = append(problems, err.Error())
			if st.Up { // it cannot be made silent: classic standby for this device
				d.flush(dev)
			}
			continue
		}
		if !st.Carrier {
			problems = append(problems, dev+" has no link")
			continue
		}
		warm = append(warm, dev)
	}
	if d.hotOn() && len(problems) == 0 {
		problems = append(problems, d.hotChecks(p, devs)...)
	}
	switch {
	case len(devs) == 0:
		d.setWarm(false, "no interface qualifies for the warm standby (all use the classic takeover)", nil, classic)
	case len(problems) > 0:
		d.setWarm(false, strings.Join(problems, "; "), warm, classic)
	case !activeValid(d.store.dir):
		d.setWarm(false, "the saved configuration of the active node is missing or damaged", warm, classic)
	case d.driftReason() != "":
		d.setWarm(false, "configuration drift: "+d.driftReason(), warm, classic)
	default:
		d.setWarm(true, "", warm, classic)
	}
}

// warmFresh: the readiness result is recent enough to base a takeover on.
const warmFresh = 15 * time.Second

func (d *Daemon) warmReady() (bool, string) {
	d.wst.mu.Lock()
	defer d.wst.mu.Unlock()
	if !d.warmOn() {
		return false, "warm standby is off"
	}
	if time.Since(d.wst.at) > warmFresh {
		return false, "readiness not evaluated recently"
	}
	return d.wst.ready, d.wst.reason
}

// warmSummary: "" (classic mode), "ready" or the reason it is not ready; carried in the heartbeat.
func (d *Daemon) warmSummary() string {
	if !d.warmOn() {
		return ""
	}
	if ok, why := d.warmReady(); ok {
		return "ready"
	} else if why != "" {
		return why
	}
	return "not ready"
}

func (d *Daemon) warmInfo() map[string]any {
	if !d.warmOn() {
		return map[string]any{"mode": "classic"}
	}
	ok, why := d.warmReady()
	d.wst.mu.Lock()
	defer d.wst.mu.Unlock()
	return map[string]any{"mode": d.cfg.HAMode, "ready": ok, "reason": why, "warm_ifaces": d.wst.warm, "classic_ifaces": d.wst.classic}
}

// warmActivate gives the shared identity to every eligible interface natively. It returns the keys it handled. Any failure
// reverts what it did and returns an error: the caller then runs the classic path for everything. Caller holds ops.mu.
func (d *Daemon) warmActivate(p *WarmPlan, set IfaceSet) (map[string]bool, error) {
	if d.hotOn() {
		return d.hotActivate(p, set)
	}
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
			a := activated[i]
			d.warmSilence(a.Dev, a.MAC)
		}
	}
	for _, i := range eligibleOrdered(*p) {
		if keep[i.Dev] {
			continue
		}
		st := d.readIf(i.Dev)
		hw, ok := d.hwm().resolve(i.Dev, i.MAC, st)
		if !ok || !st.silent(hw) {
			revert()
			return nil, fmt.Errorf("%s is not in the warm state (up, own MAC, no address) at takeover", i.Dev)
		}
		activated = append(activated, i)
		if err := d.runAll(activateCmds(i)); err != nil {
			revert()
			return nil, err
		}
		// verify, never assume: the MAC must have changed (a driver that refuses the change leaves the old one) and the address be there
		after := d.readIf(i.Dev)
		if !macEq(after.Ether, i.MAC) || !after.Up || !hasAddr(after.Inet, i.V4) {
			revert()
			return nil, fmt.Errorf("%s did not take the shared identity (mac %s, up %v, addresses %v)", i.Dev, after.Ether, after.Up, after.Inet)
		}
		done[i.Key] = true
	}
	if len(done) == 0 {
		return nil, errors.New("no interface to activate natively")
	}
	return done, nil
}

func hasAddr(have []string, cidr string) bool {
	ip := strings.SplitN(cidr, "/", 2)[0]
	for _, h := range have {
		if h == ip {
			return true
		}
	}
	return false
}

func (d *Daemon) hwm() *hwMacs {
	d.hwOnce.Do(func() { d.hwv = openHWMacs(d.store.dir) })
	return d.hwv
}

// warmLearn remembers the permanent MAC of the warm devices while it is still visible (an active node carries the shared MAC,
// ifconfig may not print the permanent one on every driver). Cheap; called now and then on a node that is not standby.
func (d *Daemon) warmLearn(path string, set IfaceSet) {
	if d.hotOn() {
		return // the permanent MAC is never needed: the shared one stays on the devices
	}
	p, err := d.warmPlanFor(path)
	if err != nil || p == nil {
		return
	}
	for dev, shared := range warmDevices(*p, set.KeepUp) {
		d.hwm().resolve(dev, shared, d.readIf(dev))
	}
}

// warmChecks is the pre-flight view of the warm standby. Not ready is a warning, not a failure: the takeover then simply
// uses the classic path.
func (d *Daemon) warmChecks() []Check {
	var cs []Check
	mode := "warm standby"
	if d.hotOn() {
		mode = "hot standby"
	}
	d.wst.mu.Lock()
	warm, classic := append([]string(nil), d.wst.warm...), append([]string(nil), d.wst.classic...)
	d.wst.mu.Unlock()
	st := d.store.Get()
	if st.State == StateActive {
		// the standby reports its own readiness in its heartbeat
		_, _, peer := d.hb.Snapshot()
		switch peer.Warm {
		case "ready":
			cs = append(cs, pass(mode, "the standby is ready for a fast takeover"))
		case "":
			cs = append(cs, warn(mode, "the peer runs in classic mode (or has not reported yet): set the same mode on both firewalls"))
		default:
			cs = append(cs, warn(mode, "the standby is not ready, a takeover would use the classic path: "+peer.Warm))
		}
	} else if ok, why := d.warmReady(); ok {
		cs = append(cs, pass(mode, fmt.Sprintf("ready: %s up and silent", strings.Join(warm, ", "))))
	} else {
		cs = append(cs, warn(mode, "not ready, a takeover would use the classic path: "+why))
	}
	if len(classic) > 0 {
		cs = append(cs, warn(mode+": classic interfaces", "these interfaces keep the classic takeover: "+strings.Join(classic, "; ")))
	}
	return cs
}

var (
	plainNICRe  = regexp.MustCompile(`^[a-z]+[0-9]+$`)
	virtualNICs = regexp.MustCompile(`^(lagg|bridge|gre|gif|vxlan|qinq|epair|ppp|pppoe|pptp|l2tp|vlan|lo|enc|pflog|pfsync|tun|tap|wg|ovpn[sc]?|ipsec)[0-9]*$`)
)

// plainNIC: a physical-style Ethernet device (em0, igb1, ixl0, hn0, vtnet0 ...). LAGG, bridges, tunnels and nested VLANs are not.
func plainNIC(dev string) bool { return plainNICRe.MatchString(dev) && !virtualNICs.MatchString(dev) }

// sanitize enforces in Go what ifaces.php already decided, so a plan from another version or a bug there can never put a
// LAGG, a bridge or a VLAN on a LAGG in the warm set (their MAC and link are managed by other code).
func sanitize(p WarmPlan) WarmPlan {
	out := WarmPlan{Ifaces: make([]WarmIface, len(p.Ifaces))}
	copy(out.Ifaces, p.Ifaces)
	for i := range out.Ifaces {
		e := &out.Ifaces[i]
		if !e.Eligible {
			continue
		}
		switch {
		case e.Parent == "" && !plainNIC(e.Dev):
			e.Eligible, e.Reason = false, e.Dev+" is not a plain NIC"
		case e.Parent != "" && !plainNIC(e.Parent):
			e.Eligible, e.Reason = false, "the VLAN sits on "+e.Parent+", which is not a plain NIC"
		case !validMAC(e.MAC) || e.V4 == "":
			e.Eligible, e.Reason = false, "no usable shared MAC or static IPv4 address"
		case !validCIDR(e.V4, 4) || (e.V6 != "" && !validCIDR(e.V6, 6)) || !allCIDR(e.VIPs, 4):
			e.Eligible, e.Reason = false, "an address of the plan is not a valid address/prefix"
		case e.MTU != 0 && (e.MTU < 576 || e.MTU > 9216):
			e.Eligible, e.Reason = false, "the MTU is out of range"
		}
	}
	return out
}

var macRe = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)

// validMAC: a unicast, non-zero Ethernet address.
func validMAC(m string) bool {
	m = strings.ToLower(m)
	if !macRe.MatchString(m) || m == "00:00:00:00:00:00" {
		return false
	}
	b, _ := strconv.ParseUint(m[:2], 16, 8)
	return b&1 == 0
}

// validCIDR: an address with a prefix of the given IP version (4 or 6). The strings end up as arguments of ifconfig (never in a shell),
// but only well-formed addresses are accepted.
func validCIDR(s string, version int) bool {
	ip, n, err := net.ParseCIDR(s)
	if err != nil || strings.ContainsAny(s, " \t\n") {
		return false
	}
	if version == 4 {
		ones, _ := n.Mask.Size()
		return ip.To4() != nil && ones >= 1
	}
	return ip.To4() == nil
}

func allCIDR(l []string, version int) bool {
	for _, s := range l {
		if !validCIDR(s, version) {
			return false
		}
	}
	return true
}
