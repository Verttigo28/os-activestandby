package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Package replication: the active node sends its list of installed os-* plugins (and the OPNsense core version)
// with every config revision. Before applying the revision, the standby installs what it is missing, through
// the update proxy on the active node. The core version must already match (updating the OS is a manual,
// ordered operation: update the standby first, then fail over, then update the other node).

func encodePackages(p map[string]string) string {
	b, _ := json.Marshal(p)
	return base64.StdEncoding.EncodeToString(b)
}

func decodePackages(s string) (map[string]string, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	return m, json.Unmarshal(b, &m)
}

// ensurePackages makes the standby's package set cover the active node's.
func (d *Daemon) ensurePackages(want map[string]string) error {
	d.pkgMu.Lock() // one install at a time, whatever the peer sends
	defer d.pkgMu.Unlock()
	if len(want) == 0 {
		return nil
	}
	have := collectInfo().Packages
	var install []string
	for name, ver := range want {
		hv, ok := have[name]
		if name == "opnsense" {
			if ok && hv != ver {
				return fmt.Errorf("OPNsense core version differs (active %s, standby %s): update both nodes to the same version (see Updating in docs/OPERATIONS.md); sync resumes by itself", ver, hv)
			}
			continue
		}
		if name == "os-activestandby" {
			continue // this plugin ships with the node
		}
		if !pkgName.MatchString(name) { // the name comes from the peer: never let it look like an option or contain odd characters
			continue
		}
		if !ok || hv != ver {
			install = append(install, name)
		}
	}
	if len(install) == 0 {
		d.setPkgReport(want, have)
		return nil
	}
	sort.Strings(install)
	st := d.store.Get()
	proxy := "http://" + net.JoinHostPort(st.PeerAddr, fmt.Sprint(proxyPort(d.cfg)))
	d.event("info", "installing %d plugin(s) replicated from the active node: %s", len(install), strings.Join(install, ", "))
	cmd := exec.Command("pkg", append([]string{"install", "-y", "--"}, install...)...)
	cmd.Env = append(os.Environ(), "HTTP_PROXY="+proxy, "HTTPS_PROXY="+proxy, "ASSUME_ALWAYS_YES=yes")
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Minute):
		cmd.Process.Kill()
		return fmt.Errorf("package install timed out: %s", strings.Join(install, ", "))
	}
	if err != nil {
		tail := strings.TrimSpace(string(out))
		if len(tail) > 400 {
			tail = tail[len(tail)-400:]
		}
		return fmt.Errorf("package install failed (%s): %v: %s", strings.Join(install, ", "), err, tail)
	}
	after := collectInfo().Packages
	d.setPkgReport(want, after)
	for _, n := range install {
		if after[n] != want[n] {
			d.event("warning", "package %s is %q on the standby but %q on the active node", n, after[n], want[n])
		}
	}
	d.event("change", "installed on the standby: %s", strings.Join(install, ", "))
	return nil
}

var pkgName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._-]{0,80}$`)

// packageDiff compares the active node's plugin list (want) with what is installed here (have). "different" lists plugins
// that are missing or at another version, "extra" the plugins installed here that the active node does not have. The core
// version and this plugin are checked separately; nothing here ever uninstalls anything.
func packageDiff(want, have map[string]string) (different, extra []string) {
	for n, v := range want {
		if n == "opnsense" || n == "os-activestandby" || !pkgName.MatchString(n) {
			continue
		}
		switch hv, ok := have[n]; {
		case !ok:
			different = append(different, n+" (missing here, active "+v+")")
		case hv != v:
			different = append(different, fmt.Sprintf("%s (active %s, here %s)", n, v, hv))
		}
	}
	for n := range have {
		if _, ok := want[n]; !ok && strings.HasPrefix(n, "os-") && n != "os-activestandby" {
			extra = append(extra, n)
		}
	}
	sort.Strings(different)
	sort.Strings(extra)
	return
}

// setPkgReport records how far the plugin set of this standby is from the active node's, for the status. A difference
// does not block the configuration sync (a failed install does), but a failover onto a node with other plugin versions
// is a risk the administrator must see.
func (d *Daemon) setPkgReport(want, have map[string]string) {
	diff, extra := packageDiff(want, have)
	var parts []string
	if len(diff) > 0 {
		parts = append(parts, "plugins that differ from the active node: "+strings.Join(diff, ", "))
	}
	if len(extra) > 0 {
		parts = append(parts, "plugins only installed here (not removed automatically): "+strings.Join(extra, ", "))
	}
	msg := strings.Join(parts, "; ")
	if prev, _ := d.pkgReport.Load().(string); prev != msg {
		if msg != "" {
			d.event("warning", "package conformity: %s", msg)
		} else if prev != "" {
			d.event("info", "package conformity: both nodes have the same plugins again")
		}
	}
	d.pkgReport.Store(msg)
}

func (d *Daemon) packageReport() string {
	s, _ := d.pkgReport.Load().(string)
	return s
}
