package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Pre-flight and dry-run. `preflight` collects the health of both nodes; the dry-run is the standby proving that it could take
// over (saved config valid, merge works, every data interface enabled and its device present or creatable) WITHOUT
// touching any interface. Results are pass / warn / fail.

type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // pass | warn | fail
	Detail string `json:"detail"`
}

func pass(n, d string) Check { return Check{n, "pass", d} }
func warn(n, d string) Check { return Check{n, "warn", d} }
func fail(n, d string) Check { return Check{n, "fail", d} }

func worst(cs ...[]Check) string {
	w := "pass"
	for _, l := range cs {
		for _, c := range l {
			if c.Status == "fail" {
				return "fail"
			}
			if c.Status == "warn" {
				w = "warn"
			}
		}
	}
	return w
}

// diskFree parses `df -k <path>` and returns the used percentage.
func diskUsedPct(path string) int {
	out, err := sh("df", "-k", path)
	if err != nil {
		return -1
	}
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		return -1
	}
	for _, f := range strings.Fields(lines[1]) {
		if strings.HasSuffix(f, "%") {
			n, _ := strconv.Atoi(strings.TrimSuffix(f, "%"))
			return n
		}
	}
	return -1
}

// localChecks is this node's own view; it needs no cooperation from the peer except the last heartbeat.
func (d *Daemon) localChecks() []Check {
	st := d.store.Get()
	var cs []Check
	if st.PeerFP == "" {
		return []Check{fail("pairing", "this node is not paired")}
	}
	alive, ago, peer := d.hb.Snapshot()
	if d.hb.PathAlive("ha") {
		rtt := d.hb.RTT("ha")
		if rtt > 50*time.Millisecond {
			cs = append(cs, warn("HA link", fmt.Sprintf("alive but slow (%s round trip)", rtt.Round(time.Millisecond))))
		} else {
			cs = append(cs, pass("HA link", fmt.Sprintf("heartbeat alive, %s round trip", rtt.Round(100*time.Microsecond))))
		}
	} else {
		cs = append(cs, fail("HA link", "no heartbeat on the HA link"))
	}
	switch {
	case d.mgmtAddr() == "":
		cs = append(cs, warn("management path", "no management interface configured: split-brain protection is weaker"))
	case d.hb.PathAlive("mgmt"):
		cs = append(cs, pass("management path", "second heartbeat path alive"))
	default:
		cs = append(cs, warn("management path", "second heartbeat path is not alive"))
	}
	switch {
	case !alive:
		cs = append(cs, fail("peer", "peer is not answering"))
	case st.State == StateActive && peer.State == StateActive, st.State == StateStandby && peer.State == StateStandby:
		cs = append(cs, fail("roles", fmt.Sprintf("this node is %s and the peer is %s: exactly one must be active", st.State, peer.State)))
	default:
		cs = append(cs, pass("roles", fmt.Sprintf("this node %s, peer %s", st.State, peer.State)))
	}
	if _, mism := d.versionInfo(); mism {
		cs = append(cs, warn("versions", "the nodes run different OPNsense or plugin versions (normal only while updating)"))
	} else {
		cs = append(cs, pass("versions", d.localVer+" on both nodes"))
	}
	sync := d.syncStatus(st)
	switch sync["state"] {
	case "in_sync":
		cs = append(cs, pass("config sync", fmt.Sprintf("in sync (revision %d)", st.SyncRev)))
	case "behind":
		cs = append(cs, warn("config sync", "saved changes are not replicated yet"))
	default:
		cs = append(cs, fail("config sync", "failed: "+st.SyncError))
	}
	if alive && peer.TimeMs != 0 {
		skew := math.Abs(float64(time.Now().Add(-ago).UnixMilli()-peer.TimeMs)) / 1000
		switch {
		case skew > 30:
			cs = append(cs, fail("clock", fmt.Sprintf("clocks differ by %.0f s", skew)))
		case skew > 2:
			cs = append(cs, warn("clock", fmt.Sprintf("clocks differ by %.1f s", skew)))
		default:
			cs = append(cs, pass("clock", "clocks agree"))
		}
	}
	locked := isImmutable(configPath())
	switch {
	case st.State == StateStandby && !locked:
		cs = append(cs, warn("config lock", "standby config.xml is not locked (unlock window or manual change)"))
	case st.State == StateActive && locked:
		cs = append(cs, fail("config lock", "active config.xml is locked"))
	default:
		cs = append(cs, pass("config lock", "config.xml lock matches the role"))
	}
	if out, _ := sh("ifconfig", "pfsync0"); strings.Contains(out, "syncpeer: "+st.PeerAddr) {
		cs = append(cs, pass("state sync (pfsync)", "pfsync0 syncing with "+st.PeerAddr))
	} else {
		cs = append(cs, fail("state sync (pfsync)", "pfsync0 is not configured towards the peer"))
	}
	for _, p := range []string{"/conf", "/var"} {
		if u := diskUsedPct(p); u >= 95 {
			cs = append(cs, fail("disk "+p, fmt.Sprintf("%d%% used", u)))
		} else if u >= 85 {
			cs = append(cs, warn("disk "+p, fmt.Sprintf("%d%% used", u)))
		}
	}
	if d.driftDetected() {
		cs = append(cs, fail("drift", d.driftReason()))
	} else {
		cs = append(cs, pass("drift", "no configuration drift detected"))
	}
	if r := d.healthReason(); r != "" {
		cs = append(cs, fail("health", r))
	}
	if d.maintenanceOn() {
		cs = append(cs, warn("maintenance", "maintenance mode is on: automatic failover is paused"))
	}
	if !d.cfg.autoOn() {
		cs = append(cs, warn("automatic failover", "disabled in the settings"))
	}
	if d.cfg.FenceCommand != "" {
		if err := validateFenceCommand(d.cfg.FenceCommand); err != nil {
			cs = append(cs, fail("fencing", err.Error()))
		} else {
			cs = append(cs, pass("fencing", "command configured and acceptable"))
		}
	}
	switch st.State {
	case StateStandby:
		set, err := d.health.ifaces(activeXML(d.store.dir))
		if err == nil {
			up, _ := sh("ifconfig", "-l", "-u")
			var loud []string
			for _, dev := range set.ForceDown {
				if strings.Contains(" "+up+" ", " "+dev+" ") {
					loud = append(loud, dev)
				}
			}
			if len(loud) > 0 {
				cs = append(cs, fail("standby silence", "data devices are up on the standby: "+strings.Join(loud, ", ")))
			} else {
				cs = append(cs, pass("standby silence", "all data devices are down"))
			}
		}
		var running []string
		for _, p := range []string{"charon", "dpinger", "openvpn"} {
			if _, err := sh("pgrep", "-x", p); err == nil {
				running = append(running, p)
			}
		}
		if len(running) > 0 {
			cs = append(cs, fail("standby services", "running on the standby: "+strings.Join(running, ", ")))
		} else {
			cs = append(cs, pass("standby services", "IPsec, gateway monitoring and VPN daemons are stopped"))
		}
		if !activeValid(d.store.dir) {
			cs = append(cs, fail("saved config", "the standby's saved configuration is missing or damaged"))
		}
	case StateActive:
		if out, err := sh("pfctl", "-si"); err == nil && !strings.Contains(out, "Status: Enabled") {
			cs = append(cs, fail("packet filter", "pf is not enabled"))
		}
		if raw, err := readFile(configPath()); err == nil && shaHex(raw) != st.AckedSHA {
			cs = append(cs, warn("unreplicated changes", "the active node holds configuration the standby does not have yet"))
		}
	}
	return cs
}

// dryRun is executed on the STANDBY: could it take over right now? Nothing is changed.
func (d *Daemon) dryRun() []Check {
	st := d.store.Get()
	if st.State != StateStandby {
		return []Check{pass("dry run", "this node is active; the dry run applies to the standby")}
	}
	var cs []Check
	if !activeValid(d.store.dir) {
		return []Check{fail("saved config", "active.xml is missing or damaged: promote would be refused")}
	}
	cs = append(cs, pass("saved config", "active.xml is a complete configuration"))
	// would the merge of local settings work?
	tmp := d.store.dir + "/dryrun.xml"
	if out, err := sh(mergeScript, activeXML(d.store.dir), configPath(), tmp); err != nil {
		cs = append(cs, fail("local settings merge", out))
	} else {
		cs = append(cs, pass("local settings merge", "node-local settings can be re-applied on promote"))
	}
	removeFile(tmp)
	set, err := loadIfaces(activeXML(d.store.dir))
	if err != nil {
		return append(cs, fail("interfaces", err.Error()))
	}
	for _, i := range set.Data {
		name := fmt.Sprintf("interface %s (%s)", i.Key, i.Dev)
		switch {
		case !i.Enabled:
			cs = append(cs, fail(name, "not enabled in the saved configuration: promote would not bring it up"))
		case !i.Virtual && !deviceExists(i.Dev):
			cs = append(cs, fail(name, "network device does not exist on this node"))
		case i.Spoofmac == "":
			cs = append(cs, warn(name, "no shared MAC pinned: the node would use its own hardware MAC"))
		default:
			cs = append(cs, pass(name, "enabled, device present or creatable, shared MAC "+i.Spoofmac))
		}
	}
	for _, bin := range []string{"configctl", "pluginctl", "pfctl", "ifconfig"} {
		if _, err := exec.LookPath(bin); err != nil {
			cs = append(cs, fail("tool "+bin, "not found"))
		}
	}
	return cs
}

func deviceExists(dev string) bool {
	_, err := sh("ifconfig", dev)
	return err == nil
}

func (d *Daemon) handlePreflight(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"checks": d.localChecks()})
}

func (d *Daemon) handleDryRun(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"checks": d.dryRun()})
}

// preflight combines both nodes and the dry run of whichever node is standby.
func (d *Daemon) preflight() map[string]any {
	local := d.localChecks()
	var peerChecks, dry []Check
	st := d.store.Get()
	if st.PeerFP != "" {
		if m, err := d.peerCall("/v1/preflight", 15*time.Second); err == nil {
			peerChecks = decodeChecks(m)
		} else {
			peerChecks = []Check{fail("peer", "cannot ask the peer: "+err.Error())}
		}
		if st.State == StateStandby {
			dry = d.dryRun()
		} else if m, err := d.peerCall("/v1/dryrun", 30*time.Second); err == nil {
			dry = decodeChecks(m)
		} else {
			dry = []Check{fail("dry run", "cannot ask the standby: "+err.Error())}
		}
	}
	return map[string]any{"summary": worst(local, peerChecks, dry), "this_node": local, "peer": peerChecks, "dry_run": dry}
}

func decodeChecks(m map[string]any) []Check {
	b, _ := json.Marshal(m["checks"])
	var cs []Check
	json.Unmarshal(b, &cs)
	return cs
}

var _ = io.EOF
