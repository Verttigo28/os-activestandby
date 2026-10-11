package main

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"os"
	"regexp"
	"strings"
)

// flushDev removes every address from the device and sets it down.
func flushDev(dev string) {
	out, _ := sh("ifconfig", dev)
	sh("ifconfig", dev, "down") // traffic stops here; the rest is cleanup
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && f[0] == "inet" {
			sh("ifconfig", dev, "inet", f[1], "-alias")
		} else if len(f) >= 2 && f[0] == "inet6" && !strings.HasPrefix(f[1], "fe80") {
			sh("ifconfig", dev, "inet6", f[1], "-alias")
		}
	}
	sh("ifconfig", dev, "inet6", "-ifdisabled") // harmless if unsupported
	sh("ifconfig", dev, "-promisc")
	sh("ifconfig", dev, "down")
}

var (
	virtualDev = regexp.MustCompile(`^(vlan|lagg|bridge|gre|gif|epair|vxlan|qinq|tap|tun|ovpn|wg)[0-9]`)
	asyncDev   = regexp.MustCompile(`^(pppoe|pptp|l2tp|ppp)[0-9]`) // created by mpd5 once the session is up
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
