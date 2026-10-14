package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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

// execShell runs a fixed script through /bin/sh (never with operator input).
func execShell(script string) (string, error) {
	out, err := boundedCmd(true, "/bin/sh", "-c", script)
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

func writeRoleMarker(st State) { writeMarker(st, nil) }

// writeMarker publishes the role (first line) and, on the second line, flags the status-bar notice turns into warnings:
// maintenance, syncfail, peerdown, drift, versions.
func writeMarker(st State, flags []string) {
	v := "standalone"
	if st.PeerFP != "" && (st.State == StateActive || st.State == StateStandby) {
		v = st.State
	}
	if len(flags) > 0 {
		v += "\n" + strings.Join(flags, " ")
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

// markerStep refreshes the marker with the conditions an administrator should notice on every page.
func (d *Daemon) markerStep() {
	st := d.store.Get()
	var flags []string
	if st.PeerFP != "" && st.State != "" {
		if d.maintenanceOn() {
			flags = append(flags, "maintenance")
		}
		if st.SyncError != "" {
			flags = append(flags, "syncfail")
		}
		if alive, _, _ := d.hb.Snapshot(); !alive {
			flags = append(flags, "peerdown")
		}
		if d.driftReason() != "" {
			flags = append(flags, "drift")
		}
		if _, mism := d.versionInfo(); mism {
			flags = append(flags, "versions")
		}
	}
	writeMarker(st, flags)
}

// peerEndpoint is the address to call the peer on: the HA link, or, when the heartbeat says that path is dead while the
// management path is alive, the management address (the peer API listens on both, with the same pinned certificate).
func (d *Daemon) peerEndpoint(st State) string {
	if st.PeerMgmt != "" && !d.hb.PathAlive("ha") && d.hb.PathAlive("mgmt") {
		return st.PeerMgmt
	}
	return st.PeerAddr
}

var (
	validMu    sync.Mutex
	validCache = map[string]validEntry{}
)

type validEntry struct {
	mtime time.Time
	size  int64
	ok    bool
}

// cachedValid memoises a file check by modification time and size: the heartbeat asks about active.xml several times a
// second and must not read and scan a 60 KB file every time.
func cachedValid(path string, check func() bool) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	validMu.Lock()
	e, ok := validCache[path]
	validMu.Unlock()
	if ok && e.mtime.Equal(fi.ModTime()) && e.size == fi.Size() {
		return e.ok
	}
	r := check()
	validMu.Lock()
	validCache[path] = validEntry{fi.ModTime(), fi.Size(), r}
	validMu.Unlock()
	return r
}
