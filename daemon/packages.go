package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
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
		if !ok || hv != ver {
			install = append(install, name)
		}
	}
	if len(install) == 0 {
		return nil
	}
	sort.Strings(install)
	st := d.store.Get()
	proxy := "http://" + net.JoinHostPort(st.PeerAddr, fmt.Sprint(proxyPort(d.cfg)))
	d.event("info", "installing %d plugin(s) replicated from the active node: %s", len(install), strings.Join(install, ", "))
	cmd := exec.Command("pkg", append([]string{"install", "-y"}, install...)...)
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
	for _, n := range install {
		if after[n] != want[n] {
			d.event("warning", "package %s is %q on the standby but %q on the active node", n, after[n], want[n])
		}
	}
	d.event("change", "installed on the standby: %s", strings.Join(install, ", "))
	return nil
}
