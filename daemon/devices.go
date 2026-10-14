package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"time"
)

// flushDev removes every address from the device and sets it down (with the real commands).
func flushDev(dev string) { flushDevWith(sh, dev) }

// flushDevWith does it with the given command runner and says whether the device really is down and without addresses afterwards:
// a command that failed (a driver that refuses "down", an address that is still configured) is not a device that went down. Every
// step is tried whatever the others returned; the answer comes from reading the device again.
func flushDevWith(run func(string, ...string) (string, error), dev string) error {
	out, _ := run("ifconfig", dev)
	run("ifconfig", dev, "down") // traffic stops here; the rest is cleanup
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && f[0] == "inet" {
			run("ifconfig", dev, "inet", f[1], "-alias")
		} else if len(f) >= 2 && f[0] == "inet6" && !strings.HasPrefix(f[1], "fe80") {
			run("ifconfig", dev, "inet6", f[1], "-alias")
		}
	}
	run("ifconfig", dev, "inet6", "-ifdisabled") // harmless if unsupported
	run("ifconfig", dev, "-promisc")
	run("ifconfig", dev, "down")
	after, err := run("ifconfig", dev)
	if err != nil && strings.TrimSpace(after) == "" {
		return nil // the device is gone (a virtual device that was destroyed): nothing carries the identity
	}
	if st := parseIfconfig(after); st.Exists && (st.Up || len(st.Inet) > 0 || len(st.Inet6) > 0) {
		return fmt.Errorf("%s did not go down (up=%v, addresses %v %v)", dev, st.Up, st.Inet, st.Inet6)
	}
	return nil
}

// standbyDown reports the devices of the list that still carry what they must not on a standby, read from the system: an address,
// or (outside the silent warm/hot devices of the plan) an interface that is up. A device that does not exist is down.
func (d *Daemon) standbyDown(devs []string, warm map[string]string) []string {
	var bad []string
	for _, dev := range devs {
		st := d.readIf(dev)
		if !st.Exists {
			continue
		}
		_, silentKept := warm[dev]
		switch {
		case len(st.Inet) > 0 || len(st.Inet6) > 0:
			bad = append(bad, dev)
		case st.Up && !(silentKept && st.Disabled):
			bad = append(bad, dev)
		}
	}
	return bad
}

var (
	virtualDev = regexp.MustCompile(`^(vlan|lagg|bridge|gre|gif|epair|vxlan|qinq|tap|tun|ovpn[sc]?|wg)[0-9]`)
	asyncDev   = regexp.MustCompile(`^(pppoe|pptp|l2tp|ppp|ovpn[sc])[0-9]`) // created by mpd5 once the session is up
	devSection = regexp.MustCompile(`(?s)<(vlans|laggs|bridges|gres|gifs|qinqs)>.*?</(vlans|laggs|bridges|gres|gifs|qinqs)>`)
)

// devTier orders interface bring-up: plain NICs, then LAGGs, then VLANs (they may sit on a LAGG), then the rest
// (bridges, tunnels: they may sit on VLANs).
func devTier(i Iface) int {
	switch {
	case !i.Virtual && !virtualDev.MatchString(i.Dev):
		return 0
	case strings.HasPrefix(i.Dev, "lagg"):
		return 1
	case strings.HasPrefix(i.Dev, "vlan"), strings.HasPrefix(i.Dev, "qinq"), strings.Contains(i.Dev, "_vlan"), strings.Contains(i.Dev, "."):
		return 2
	}
	return 3
}

// devSectionsHash fingerprints the device definitions (VLAN, LAGG, bridge, tunnel sections) of a config file.
func devSectionsHash(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	h := sha256.New()
	for _, m := range devSection.FindAll(b, -1) {
		h.Write(m)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// virtualDevicesPresent: the devices survive on a standby (they are only taken down), so the slow device script is skipped
// when every virtual device of the data interfaces still exists.
func virtualDevicesPresent(set IfaceSet) bool {
	all, err := sh("ifconfig", "-l")
	if err != nil {
		return false
	}
	have := map[string]bool{}
	for _, n := range strings.Fields(all) {
		have[n] = true
	}
	for _, i := range set.Data {
		if i.Virtual && !have[i.Dev] {
			return false
		}
	}
	return true
}

// prepareDevices keeps the VLAN/LAGG/... devices of a standby in line with the replicated config, so a promote does not
// have to run the slow device script. Whatever the script brings up is taken down again right away. Caller holds ops.mu.
func (d *Daemon) prepareDevices() {
	full := activeXML(d.store.dir)
	h := devSectionsHash(full)
	if h == "" || h == d.ops.devHash {
		return
	}
	set, err := d.ops.ifaces(full)
	if err != nil || len(set.DeviceTypes) == 0 {
		d.ops.devHash = h
		return
	}
	if out, err := sh(devicesScript); err != nil {
		log.Printf("standby: preparing virtual devices: %v: %s", err, out)
		return
	}
	d.enforceOnce()
	d.ops.devHash = h
	log.Printf("standby: virtual devices prepared for a fast takeover")
}

// waitCarrier gives physical NICs time to negotiate: "UP" is only the administrative flag, the link of a real NIC (ixl,
// igb, ...) needs one to a few seconds. Announcing the MAC before that is lost on the switch, and starting gateway
// monitoring before that makes a healthy gateway look dead. Devices that never report a carrier are not waited for.
func waitCarrier(devs []Iface, max time.Duration) {
	deadline := time.Now().Add(max)
	for _, i := range devs {
		if asyncDev.MatchString(i.Dev) || strings.HasPrefix(i.Dev, "bridge") || strings.HasPrefix(i.Dev, "gif") || strings.HasPrefix(i.Dev, "gre") {
			continue
		}
		for !carrierUp(i.Dev) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
	}
}
