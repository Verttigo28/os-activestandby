package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Operational state, separate from the pairing role:
//
//	StateActive  - config.xml is the full config, data interfaces are up, services run.
//	StateStandby - config.xml is the "standby view" (no <enable> on data interfaces), interfaces are
//	               down and without addresses, IPsec/dpinger stopped. The full config is kept in active.xml.
//
// The ACTIVE node is the configuration source (edit it, it replicates to the standby).
const (
	StateActive  = "active"
	StateStandby = "standby"

	configXML = "/conf/config.xml"
)

// Services that must only run on the active node. Stopped on demote, (re)started on promote.
// The configd actions are used because the pluginctl service names differ (strongswan, not ipsec) and dpinger
// is only a service while gateways exist; `pluginctl -c monitor` is the hook that (re)starts gateway monitoring.
var (
	haStop = [][]string{
		{"configctl", "ipsec", "stop"},
		{"configctl", "openvpn", "stop"},
		{"configctl", "wireguard", "stop"},
		{"pkill", "-x", "dpinger"},
		// An LLDP agent sends its frames from the MAC of the device it listens on: on a standby that is the shared MAC (seen on the lab: lldpd
		// sent every 30 s from the standby, with the shared MAC as source). It is killed, not stopped: a stopping lldpd sends a goodbye frame
		// on every device (captured on the lab at a demotion, from the shared MAC, after the device was silenced). Started again with the
		// other enabled services at a takeover.
		{"pkill", "-9", "-x", "lldpd"},
	}
	haStart = [][]string{
		{"configctl", "ipsec", "start"},
		{"pluginctl", "-c", "monitor"},
		{"configctl", "openvpn", "start"},
		{"configctl", "wireguard", "start"},
	}
)

type Iface struct {
	Key      string `json:"key"`
	Dev      string `json:"dev"`
	Enabled  bool   `json:"enabled"`
	Spoofmac string `json:"spoofmac"`
	Virtual  bool   `json:"virtual"` // a VLAN/LAGG/bridge/... device defined in the config (created on promote)
}

type IfaceSet struct {
	Data        []Iface  `json:"data"`
	ForceDown   []string `json:"force_down"`
	KeepUp      []string `json:"keep_up"`
	DeviceTypes []string `json:"device_types"`
	HAKey       string   `json:"ha_key"`
	MgmtKey     string   `json:"mgmt_key"`
}

type Ops struct {
	mu           sync.Mutex // serialises promote/demote
	cache        IfaceSet
	mtime        time.Time
	devHash      string         // hash of the device definitions create_devices.php last applied here
	bg           sync.WaitGroup // background work started by promote (DHCP/DNS restart); demote waits for it
	planMu       sync.Mutex     // the warm plan of the full configuration, cached by file time
	plan         *WarmPlan
	planMtime    time.Time
	planPath     string
	enforceErrAt time.Time // the last time an unreadable interface list on a standby was reported (at most every ten minutes)
}

func configPath() string {
	if p := os.Getenv("HASYNC_CONFIG"); p != "" {
		return p
	}
	return configXML
}

func activeXML(dir string) string { return filepath.Join(dir, "active.xml") }

// loadIfacesHook replaces the PHP helper in the tests.
var loadIfacesHook func(path string) (IfaceSet, error)

func loadIfaces(path string) (IfaceSet, error) {
	if loadIfacesHook != nil {
		return loadIfacesHook(path)
	}
	var s IfaceSet
	out, err := boundedCmd(false, ifacesScript, "list", path)
	if err != nil {
		return s, fmt.Errorf("ifaces.php list: %w", err)
	}
	return s, json.Unmarshal(out, &s)
}

// ifaces returns the interface set, re-reading only when the config on disk changed.
func (o *Ops) ifaces(path string) (IfaceSet, error) {
	fi, err := os.Stat(path)
	if err == nil && fi.ModTime().Equal(o.mtime) && len(o.cache.ForceDown) > 0 {
		return o.cache, nil
	}
	s, err := loadIfaces(path)
	if err == nil && fi != nil {
		o.cache, o.mtime = s, fi.ModTime()
	}
	return s, err
}

func sh(name string, args ...string) (string, error) {
	out, err := boundedCmd(true, name, args...)
	return strings.TrimSpace(string(out)), err
}

func step(t0 time.Time, what string) { log.Printf("  +%4dms %s", time.Since(t0).Milliseconds(), what) }

func copyFile(src, dst string, perm os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFileDurable(dst, b, perm)
}

// writeStandbyView derives the standby config.xml from the full config.
func (d *Daemon) writeStandbyView(full string) error {
	unlockConfig()
	defer lockConfig()
	tmp := configPath() + ".sbv"
	// the helper writes into an existing file, so the standby view is never world-readable; created exclusively, so that nothing
	// planted at that name (a symbolic link to another file) is followed and truncated
	os.Remove(tmp)
	if err := writeExcl(tmp, nil, 0o600); err != nil {
		return fmt.Errorf("stdview: %w", err)
	}
	if out, err := sh(ifacesScript, "stdview", full, tmp); err != nil {
		return fmt.Errorf("stdview: %v: %s", err, out)
	}
	os.Chmod(tmp, 0o640)
	syncFile(tmp)
	if err := os.Rename(tmp, configPath()); err != nil {
		return err
	}
	return syncDir(filepath.Dir(configPath()))
}

// activeValid reports whether active.xml looks like a complete OPNsense config (not empty/torn).
func activeValid(dir string) bool {
	return cachedValid(activeXML(dir), func() bool {
		b, err := os.ReadFile(activeXML(dir))
		return err == nil && len(b) > 200 && strings.Contains(string(b[:min(len(b), 4096)]), "<opnsense") &&
			strings.Contains(string(b[max(0, len(b)-64):]), "</opnsense>")
	})
}

// The helper scripts of the plugin. HASYNC_SCRIPTS (and HASYNC_PATH for the search path of the external commands) exist
// only for the sandbox in lab/sandbox, where the daemons run against stub scripts and shims; unset in production.
var (
	scriptDir     = envDir("HASYNC_SCRIPTS", "/usr/local/opnsense/scripts/OPNsense/ActiveStandby")
	ifacesScript  = scriptDir + "/ifaces.php"
	reconfScript  = scriptDir + "/reconfigure_ifaces.php"
	devicesScript = scriptDir + "/create_devices.php"
)
