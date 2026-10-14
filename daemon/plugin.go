package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Plugin version synchronisation.
//
// Both nodes must run the same version of this plugin: the daemons speak to each other, and a pair that runs two versions is
// reported as a mismatch (and refuses what needs both sides to agree, see proto.go). The plugin is not in the OPNsense repositories,
// so "pkg upgrade" and the firmware update of OPNsense never update it. The node that has the newer version builds a package from
// what is installed (pkg create, the same files and scripts) and the node with the older version fetches it from the pinned peer
// connection, checks it and installs it (pkg add). The install script restarts the daemon inside a maintenance window.
//
// Trust: a paired peer is root-equivalent on the other node (it replicates the configuration, SSH keys and cron jobs, and asks for
// package installs); the package it sends is no more than that. It is checked anyway: the checksum, the name and origin of the
// package, and that it is NEWER than the one installed (a downgrade is never done by this path).

const (
	pluginPkg       = "os-activestandby"
	pluginPkgOrigin = "opnsense/os-activestandby"
	pluginMaxSize   = 64 << 20
)

// compareVersions compares two plugin versions ("1.0.5", "1.0.5-dev", "1.0.5.d1"): -1, 0, 1. The numbers of the leading dotted part
// compare as numbers; at the same numbers a pre-release (a suffix after '-' or a letter part such as ".d1") is older than the release.
func compareVersions(a, b string) int {
	an, ap := splitVersion(a)
	bn, bp := splitVersion(b)
	for i := 0; i < len(an) || i < len(bn); i++ {
		var x, y int
		if i < len(an) {
			x = an[i]
		}
		if i < len(bn) {
			y = bn[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case ap == bp:
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	case ap < bp:
		return -1
	}
	return 1
}

// splitVersion returns the leading numeric parts and the rest ("" for a release).
func splitVersion(v string) (nums []int, pre string) {
	v = strings.TrimSpace(v)
	parts := strings.Split(v, ".")
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			// "5-dev" or "d1": the number before a '-', then the pre-release part
			if j := strings.IndexByte(p, '-'); j > 0 {
				if n, err := strconv.Atoi(p[:j]); err == nil {
					nums = append(nums, n)
					pre = p[j+1:] + strings.Join(parts[i+1:], ".")
					return
				}
			}
			pre = strings.Join(parts[i:], ".")
			return
		}
		nums = append(nums, n)
	}
	return
}

// ---- the node that has the package ----

var pluginBuild struct {
	mu   sync.Mutex
	last time.Time
}

// buildPluginPackage creates a package of the installed plugin in dir and returns its path (a variable for the tests).
var buildPluginPackage = func(dir string) (string, error) {
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if out, err := boundedCmd(true, "pkg", "create", "-o", dir, pluginPkg); err != nil {
		return "", fmt.Errorf("pkg create: %v: %s", err, strings.TrimSpace(string(out)))
	}
	m, _ := filepath.Glob(filepath.Join(dir, pluginPkg+"-*.pkg"))
	if len(m) != 1 {
		return "", errors.New("pkg create produced no package")
	}
	return m[0], nil
}

// handlePluginGet gives the paired peer a package of the plugin installed here. At most one build at a time and one every 10 s (a
// build reads every file of the plugin and compresses them).
func (d *Daemon) handlePluginGet(w http.ResponseWriter, r *http.Request) {
	if !pluginBuild.mu.TryLock() {
		http.Error(w, "a package is being built", http.StatusTooManyRequests)
		return
	}
	defer pluginBuild.mu.Unlock()
	if time.Since(pluginBuild.last) < 10*time.Second {
		http.Error(w, "too soon", http.StatusTooManyRequests)
		return
	}
	pluginBuild.last = time.Now()
	dir := filepath.Join(d.store.dir, "plugin-out")
	defer os.RemoveAll(dir)
	path, err := buildPluginPackage(dir)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) > pluginMaxSize {
		writeJSON(w, 500, map[string]string{"error": "the package cannot be read or is too large"})
		return
	}
	w.Header().Set("X-Plugin-Version", d.pluginVersion())
	w.Header().Set("X-SHA256", shaHex(b))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(b)
}

// ---- the node with the older version ----

// handlePluginPull: the peer (the node with the newer version) asks this node to fetch and install the plugin from it. The answer is
// immediate: the installation restarts this daemon.
func (d *Daemon) handlePluginPull(w http.ResponseWriter, r *http.Request) {
	alive, _, peer := d.hb.Snapshot()
	if !alive || peer.Plug == "" {
		writeJSON(w, 409, map[string]string{"error": "the peer is not heard, its plugin version is unknown"})
		return
	}
	if compareVersions(peer.Plug, d.pluginVersion()) <= 0 {
		writeJSON(w, 409, map[string]string{"error": fmt.Sprintf("the peer runs %s, this node %s: nothing newer to install", peer.Plug, d.pluginVersion())})
		return
	}
	if d.failing.Load() || d.opsBusy() {
		writeJSON(w, 409, map[string]string{"error": "a role change is in progress"})
		return
	}
	go func() {
		if msg, err := d.pullPlugin(); err != nil {
			d.event("error", "plugin synchronisation: %v", err)
		} else {
			d.event("change", "plugin synchronisation: %s", msg)
		}
	}()
	writeJSON(w, 200, map[string]string{"status": "started", "from": peer.Plug, "to": d.pluginVersion()})
}

var pkgInfoField = regexp.MustCompile(`(?m)^(Name|Version|Origin|Architecture)\s*:\s*(.+)$`)

// verifyPluginPackage reads the metadata of a package file and accepts only this plugin (a variable for the tests).
var verifyPluginPackage = func(path string) (ver string, err error) {
	out, err := boundedCmd(false, "pkg", "info", "-F", path)
	if err != nil {
		return "", fmt.Errorf("pkg info -F: %v", err)
	}
	f := map[string]string{}
	for _, m := range pkgInfoField.FindAllStringSubmatch(string(out), -1) {
		f[m[1]] = strings.TrimSpace(m[2])
	}
	if f["Name"] != pluginPkg || f["Origin"] != pluginPkgOrigin {
		return "", fmt.Errorf("the package is %q (%q), not %s", f["Name"], f["Origin"], pluginPkg)
	}
	if !strings.HasPrefix(f["Architecture"], "FreeBSD:") {
		return "", fmt.Errorf("unexpected architecture %q", f["Architecture"])
	}
	return f["Version"], nil
}

// addPluginPackage installs a package file (a variable for the tests). The install script restarts the daemon.
var addPluginPackage = func(ctx context.Context, path string) ([]byte, error) {
	return boundedCmdCtx(ctx, true, "pkg", "add", "-f", path)
}

// pullPlugin fetches the package of the plugin from the peer, checks it and installs it.
func (d *Daemon) pullPlugin() (string, error) {
	st := d.store.Get()
	if st.PeerFP == "" {
		return "", errors.New("not paired")
	}
	cl := d.peerClient(st.PeerFP, nil)
	cl.Timeout = 5 * time.Minute
	resp, err := cl.Post("https://"+net.JoinHostPort(d.peerEndpoint(st), fmt.Sprint(st.PeerPort))+"/v1/plugin/get", "application/json", nil)
	if err != nil {
		return "", fmt.Errorf("fetching the package from the peer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		if resp.StatusCode == http.StatusNotFound {
			return "", errors.New("the peer has no plugin synchronisation (its plugin is older than this function): install this version on it once with pkg add")
		}
		return "", fmt.Errorf("the peer answered %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, pluginMaxSize+1))
	if err != nil || len(b) > pluginMaxSize {
		return "", errors.New("the package of the peer is unreadable or too large")
	}
	if shaHex(b) != resp.Header.Get("X-SHA256") {
		return "", errors.New("the checksum of the package does not match")
	}
	peerVer := resp.Header.Get("X-Plugin-Version")
	if compareVersions(peerVer, d.pluginVersion()) <= 0 {
		return fmt.Sprintf("the peer runs %s, this node %s: nothing to install", peerVer, d.pluginVersion()), nil
	}
	path := filepath.Join(d.store.dir, "plugin-in.pkg")
	os.Remove(path)
	if err := writeExcl(path, b, 0o600); err != nil {
		return "", err
	}
	defer os.Remove(path)
	pkgVer, err := verifyPluginPackage(path)
	if err != nil {
		return "", err
	}
	ctx, release, err := d.registerInstall(5*time.Minute, false)
	if err != nil {
		return "", err
	}
	defer release()
	d.event("info", "installing the plugin %s (package %s) fetched from the peer", peerVer, pkgVer)
	if out, err := addPluginPackage(ctx, path); err != nil {
		return "", fmt.Errorf("pkg add: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return fmt.Sprintf("plugin %s installed (it was %s)", peerVer, d.pluginVersion()), nil
}

// pluginSync brings both nodes to the newer plugin version: the node that has it makes the other fetch it, or this node fetches it.
// It returns when the peer reports the new version, or says what happened.
func (d *Daemon) pluginSync() (string, error) {
	st := d.store.Get()
	alive, _, peer := d.hb.Snapshot()
	if st.PeerFP == "" || !alive || peer.Plug == "" {
		return "", errors.New("the peer must be paired and heard to compare plugin versions")
	}
	switch c := compareVersions(d.pluginVersion(), peer.Plug); {
	case c == 0 && d.pluginVersion() == peer.Plug:
		return fmt.Sprintf("both nodes run plugin %s", d.pluginVersion()), nil
	case c == 0:
		return "", fmt.Errorf("the versions %s and %s cannot be ordered: install the same package on both nodes", d.pluginVersion(), peer.Plug)
	case c < 0:
		// the peer is newer: this node fetches (its daemon restarts during the installation)
		return d.pullPlugin()
	}
	// this node is newer: the peer fetches from here
	if _, err := d.peerCall("/v1/plugin/pull", 20*time.Second); err != nil {
		if strings.Contains(err.Error(), "answered 404") {
			return "", errors.New("the peer runs a plugin older than this function (it has no plugin synchronisation): install the package of this version on it once, with pkg add; the next updates are then automatic")
		}
		return "", fmt.Errorf("the peer refused to fetch the plugin: %w", err)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		if a, _, p := d.hb.Snapshot(); a && p.Plug == d.pluginVersion() {
			return fmt.Sprintf("the peer now runs plugin %s", d.pluginVersion()), nil
		}
	}
	return "", fmt.Errorf("the peer did not report plugin %s within 5 minutes (see its events)", d.pluginVersion())
}

var pluginSyncRunning atomic.Bool

// startPluginSync runs pluginSync in the background (one at a time) and reports its result as an event. It returns at once with what
// is about to happen, or why nothing will.
func (d *Daemon) startPluginSync() (string, error) {
	_, _, peer := d.hb.Snapshot()
	if peer.Plug == d.pluginVersion() {
		return fmt.Sprintf("both nodes already run plugin %s", d.pluginVersion()), nil
	}
	if !pluginSyncRunning.CompareAndSwap(false, true) {
		return "", errors.New("a plugin synchronisation is already running")
	}
	go func() {
		defer pluginSyncRunning.Store(false)
		msg, err := d.pluginSync()
		if err != nil {
			d.event("error", "plugin synchronisation failed: %v", err)
			return
		}
		d.event("change", "plugin synchronisation: %s", msg)
	}()
	return fmt.Sprintf("synchronising the plugin (this node %s, peer %s): follow the events", version, peer.Plug), nil
}

// pluginVersion is the version of the plugin running here (the field overrides it in the tests, where two daemons share one binary).
func (d *Daemon) pluginVersion() string {
	if d.pluginVer != "" {
		return d.pluginVer
	}
	return version
}
