package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
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
	// The installation can take minutes. A promotion of this node must not run next to it (a plugin's install scripts restart
	// configd and reconfigure plugins, which collides with the takeover): stopPackageInstall cancels it, and the whole process group
	// of pkg goes (ownGroup).
	if d.store.Get().State != StateStandby {
		return errors.New("not installing plugins: this node is no longer standby")
	}
	ctx, cancel := context.WithTimeout(context.Background(), pkgInstallTimeout)
	done := make(chan struct{})
	d.pkgr.mu.Lock()
	d.pkgr.cancel, d.pkgr.done = cancel, done
	d.pkgr.mu.Unlock()
	defer func() {
		cancel()
		d.pkgr.mu.Lock()
		d.pkgr.cancel, d.pkgr.done = nil, nil
		d.pkgr.mu.Unlock()
		close(done)
	}()
	env := append(os.Environ(), "HTTP_PROXY="+proxy, "HTTPS_PROXY="+proxy, "ASSUME_ALWAYS_YES=yes")
	out, err := pkgInstall(ctx, env, install)
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		d.event("warning", "plugin installation interrupted: this node is taking over (%s); it is installed again by the next sync", strings.Join(install, ", "))
		return fmt.Errorf("package install interrupted by a takeover: %s", strings.Join(install, ", "))
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
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

// pkgInstallTimeout bounds one installation of replicated plugins.
var pkgInstallTimeout = 8 * time.Minute

// pkgInstall runs pkg install (a variable for the tests).
var pkgInstall = func(ctx context.Context, env, names []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "pkg", append([]string{"install", "-y", "--"}, names...)...)
	cmd.Env = env
	ownGroup(cmd)
	cmd.WaitDelay = 10 * time.Second
	return cmd.CombinedOutput()
}

// pkgRun is the plugin installation in progress, if any.
type pkgRun struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// stopPackageInstall cancels a plugin installation in progress and waits (bounded) until it is over. Called at the start of a
// promotion.
func (d *Daemon) stopPackageInstall() {
	d.pkgr.mu.Lock()
	cancel, done := d.pkgr.cancel, d.pkgr.done
	d.pkgr.mu.Unlock()
	if cancel == nil {
		return
	}
	d.event("warning", "a plugin installation is running: stopping it before the takeover")
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		d.event("error", "the plugin installation did not stop within 15 s: the takeover goes on")
	}
}
