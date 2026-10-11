package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"
)

func readFile(p string) ([]byte, error) { return os.ReadFile(p) }
func removeFile(p string)               { os.Remove(p) }

func osStat(path string) (time.Time, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

func peerHostPort(st State, port int) string { return net.JoinHostPort(st.PeerAddr, fmt.Sprint(port)) }

func peerURL(st State, path string) string { return "https://" + peerHostPort(st, st.PeerPort) + path }

// execShell runs a fixed script through /bin/sh (never with operator input).
func execShell(script string) (string, error) {
	out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput()
	return string(out), err
}

// hardenFiles keeps the saved copies of the full configuration (password hashes, VPN keys) readable by root only and
// stops the "unreplicated-*.xml" safety copies from piling up.
func hardenFiles(dir string) {
	if fi, err := os.Stat("/usr/local/etc/hasyncd.json"); err == nil && fi.Mode().Perm()&0o077 != 0 {
		os.Chmod("/usr/local/etc/hasyncd.json", 0o600) // holds the kit token and the webhook URL
	}
	for _, n := range []string{"active.xml", "active.xml.prev", "last-incoming.xml", "state.json", "state.json.bak"} {
		if fi, err := os.Stat(filepath.Join(dir, n)); err == nil && fi.Mode().Perm()&0o077 != 0 {
			os.Chmod(filepath.Join(dir, n), 0o600)
		}
	}
	old, _ := filepath.Glob(filepath.Join(dir, "unreplicated-*.xml"))
	sort.Strings(old) // the name carries a unix timestamp
	for len(old) > 5 {
		os.Remove(old[0])
		old = old[1:]
	}
}

// roleMarker is a world-readable copy of this node's HA state ("standby", "active", "standalone") for the GUI, which
// runs unprivileged and cannot read the state directory.
const roleMarker = "/var/run/activestandby.state"

func writeRoleMarker(st State) {
	v := "standalone"
	if st.PeerFP != "" && (st.State == StateActive || st.State == StateStandby) {
		v = st.State
	}
	if b, err := os.ReadFile(roleMarker); err == nil && string(b) == v {
		return
	}
	os.WriteFile(roleMarker, []byte(v), 0o644)
}

// atomicTime is a time stamp that goroutines can read and write without a lock (zero value = unset).
type atomicTime struct{ v atomic.Int64 }

func (a *atomicTime) Set(t time.Time) {
	if t.IsZero() {
		a.v.Store(0)
		return
	}
	a.v.Store(t.UnixNano())
}

func (a *atomicTime) Get() time.Time {
	n := a.v.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// hostName is this node's name as the GUI shows it.
func hostName() string {
	h, _ := os.Hostname()
	return h
}
