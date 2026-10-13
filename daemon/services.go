package main

import (
	"encoding/json"
	"log"
	"os"
	"strings"
)

// Services that OPNsense runs on the node that is active. A standby cannot run what binds to the shared addresses (SNMP bound to the
// address of the LAN, a node exporter listening on a VLAN address, ...), or never got the configuration of a plugin that was set up
// after it booted: such a service is enabled in the replicated configuration but not running when the node becomes active. Found when
// the configuration of a pfSense (SNMP, node exporter, LLDP) was replicated onto the test pair: after the first failover none of them
// ran on the new active node.
//
// After a takeover every service OPNsense lists as enabled and that is not running is started, except the ones that have their own
// handling here (IPsec, OpenVPN, WireGuard, the gateway monitor, DHCP and DNS) or that must never be touched.

// serviceSkip are the services the takeover treats on its own.
var serviceSkip = map[string]bool{
	"configd": true, "ipsec": true, "strongswan": true, "openvpn": true, "wireguard": true, "dpinger": true, "monitor": true,
	"dnsmasq": true, "unbound": true, "dhcpd": true, "dhcrelay": true, "dhcp6": true, "kea": true, "radvd": true,
	"ntpd": true, "captiveportal": true, "suricata": true, "haproxy": true, "ddclient": true, // restarted by restartBoundServices
}

type serviceEntry struct {
	Name   string          `json:"name"`
	Locked bool            `json:"locked"`
	Status json.RawMessage `json:"status"` // a string, but an array for a few services
}

// stoppedServices reads the output of `pluginctl -S` and returns the services that are listed, not locked, not handled elsewhere and
// reported as not running.
func stoppedServices(pluginctl string) []string {
	var all []serviceEntry
	if err := json.Unmarshal([]byte(pluginctl), &all); err != nil {
		return nil
	}
	var out []string
	for _, s := range all {
		if s.Locked || serviceSkip[s.Name] || !devNameLike(s.Name) {
			continue
		}
		if strings.Contains(strings.ToLower(string(s.Status)), "not running") {
			out = append(out, s.Name)
		}
	}
	return out
}

// devNameLike: a service name that is safe as an argument (the output of pluginctl is local, but nothing is passed to a shell anyway).
func devNameLike(n string) bool {
	if n == "" || len(n) > 64 {
		return false
	}
	for _, r := range n {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return !strings.HasPrefix(n, "-")
}

// startStoppedServices starts, after a takeover, the services that are enabled but not running, then checks that they are running:
// a start command that returned is not a service that works. It returns the services that are running now and the ones that are not.
func (d *Daemon) startStoppedServices() (started, failed []string) {
	out, err := sh("pluginctl", "-S")
	if err != nil {
		log.Printf("  services after the takeover: pluginctl -S: %v", err)
		return nil, nil
	}
	stopped := stoppedServices(out)
	if len(stopped) == 0 {
		return nil, nil
	}
	d.reloadPluginTemplates()
	for _, name := range stopped {
		if o, err := sh("pluginctl", "-s", name, "start"); err != nil {
			log.Printf("  start %s after takeover: %v: %s", name, err, strings.TrimSpace(o))
		}
	}
	after, _ := sh("pluginctl", "-S")
	failed = stillStopped(stopped, after)
	for _, name := range stopped {
		if !contains(failed, name) {
			started = append(started, name)
		}
	}
	if len(started) > 0 {
		d.event("info", "services that were enabled but not running were started after the takeover: %s", strings.Join(started, ", "))
	}
	if len(failed) > 0 {
		d.event("error", "services that are enabled but could not be started after the takeover: %s", strings.Join(failed, ", "))
	}
	return started, failed
}

// stillStopped returns the services of want that pluginctl -S still reports as not running (all of them if it cannot be read).
func stillStopped(want []string, pluginctl string) []string {
	var all []serviceEntry
	if json.Unmarshal([]byte(pluginctl), &all) != nil {
		return want
	}
	var out []string
	for _, w := range want {
		running := false
		for _, s := range all {
			if s.Name == w && !strings.Contains(strings.ToLower(string(s.Status)), "not running") {
				running = true
			}
		}
		if !running {
			out = append(out, w)
		}
	}
	return out
}

// The files that tell rc.d whether a plugin service is enabled (/etc/rc.conf.d/snmpd, ...) are generated from templates, and a
// standby does not always regenerate them when it receives a configuration: on the test pair snmpd, node_exporter and lldpd were
// "not running" on the new active node because their rc.conf.d said NO, and starting them did nothing. The templates of the
// plugins are regenerated before the services are started. The ones of the base system (packet filter, VPN, DHCP, DNS, web GUI,
// authentication, this plugin) are not touched: the takeover handles them.
var templateSkip = map[string]bool{
	"Macros": true, "Filter": true, "Shaper": true, "IPsec": true, "Auth": true, "WebGui": true, "ActiveStandby": true,
	"Trust": true, "IPFW": true, "Unbound": true, "Dnsmasq": true, "Kea": true, "Wireguard": true, "Syslog": true, "Cron": true,
}

const templateDir = "/usr/local/opnsense/service/templates/OPNsense"

// pluginTemplates returns the template modules (OPNsense/<name>) to regenerate from the names found in the template directory.
func pluginTemplates(names []string) []string {
	var out []string
	for _, n := range names {
		if !templateSkip[n] && devNameLike(n) {
			out = append(out, "OPNsense/"+n)
		}
	}
	return out
}

func (d *Daemon) reloadPluginTemplates() {
	ents, err := os.ReadDir(templateDir)
	if err != nil {
		log.Printf("  templates: %v", err)
		return
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	for _, m := range pluginTemplates(names) {
		if o, err := sh("configctl", "template", "reload", m); err != nil {
			log.Printf("  template reload %s: %v: %s", m, err, strings.TrimSpace(o))
		}
	}
}
