package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config sync: the ACTIVE node pushes its full config.xml as numbered revisions to the standby.
// The standby merges it with its node-local parts (merge_config.php), stores the result as active.xml,
// derives the standby view for config.xml, reloads pf only, and acks. Any failure rolls back to the previous
// good files and is reported to the active node (and shown in the GUI). Data interfaces are never touched.

const mergeScript = "/usr/local/opnsense/scripts/OPNsense/ActiveStandby/merge_config.php"

func shaHex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

type syncCtl struct {
	lastPinSHA string
	wake       chan struct{}
	lastTry    time.Time
	lastSHA    string
}

func (d *Daemon) wakeSync() {
	select {
	case d.sync.wake <- struct{}{}:
	default:
	}
}

// syncLoop watches config.xml on the active node and pushes whenever it differs from the last acked content.
// Polling by content hash (every second, plus a wake-up from the config syshook) cannot miss an event.
func (d *Daemon) syncLoop() {
	t := time.NewTicker(time.Second)
	for {
		select {
		case <-t.C:
		case <-d.sync.wake:
		}
		st := d.store.Get()
		if st.PeerFP == "" || st.State != StateActive {
			continue
		}
		alive, _, peer := d.hb.Snapshot()
		if !alive || peer.State != StateStandby {
			continue
		}
		b, err := os.ReadFile(configPath())
		if err != nil {
			continue
		}
		sha := shaHex(b)
		if sha == st.AckedSHA {
			if st.SyncError != "" { // everything we have was acked, any old error is stale
				d.store.Update(func(s *State) { s.SyncError = "" })
			}
			continue
		}
		// interfaces added since pairing (new VLANs, new ports) need their MAC pinned before the standby sees them
		if sha != d.sync.lastPinSHA {
			d.sync.lastPinSHA = sha
			if d.pinMACs() {
				continue // config.xml changed, re-evaluate with the new content
			}
		}
		// after a failure, retry the same content only every 5 s
		if st.SyncError != "" && sha == d.sync.lastSHA && time.Since(d.sync.lastTry) < 5*time.Second {
			continue
		}
		d.sync.lastTry, d.sync.lastSHA = time.Now(), sha
		if err := d.pushConfig(b, sha); err != nil {
			log.Printf("sync: push failed: %v", err)
		}
	}
}

func (d *Daemon) pushConfig(b []byte, sha string) error {
	st := d.store.Get()
	rev := st.SyncRev + 1
	var z bytes.Buffer
	zw := gzip.NewWriter(&z)
	zw.Write(b)
	zw.Close()

	cl := d.peerClient(st.PeerFP, nil)
	cl.Timeout = 12 * time.Minute // the standby may have to install plugins first
	req, _ := http.NewRequest(http.MethodPost, "https://"+net.JoinHostPort(st.PeerAddr, fmt.Sprint(st.PeerPort))+"/v1/config", &z)
	req.Header.Set("X-Rev", strconv.FormatUint(rev, 10))
	req.Header.Set("X-SHA256", sha)
	req.Header.Set("X-Packages", encodePackages(collectInfo().Packages))
	resp, err := cl.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			err = fmt.Errorf("standby rejected revision %d: %s", rev, bytes.TrimSpace(msg))
		}
	}
	if err != nil {
		if d.store.Get().SyncError == "" {
			d.event("error", "config sync failed: %v", err)
		}
		d.store.Update(func(s *State) { s.SyncError = err.Error() })
		return err
	}
	if d.store.Get().SyncError != "" {
		d.event("info", "config sync recovered, revision %d acked", rev)
	}
	log.Printf("sync: revision %d acked by standby (%s)", rev, sha[:12])
	return d.store.Update(func(s *State) {
		s.SyncRev, s.AckedRev, s.AckedSHA, s.SyncError = rev, rev, sha, ""
		s.SyncOK = time.Now().UTC().Format(time.RFC3339)
	})
}

// handleConfig runs on the standby.
func (d *Daemon) handleConfig(w http.ResponseWriter, r *http.Request) {
	st := d.store.Get()
	if st.State != StateStandby {
		http.Error(w, "this node is not standby", 409)
		return
	}
	rev, _ := strconv.ParseUint(r.Header.Get("X-Rev"), 10, 64)
	if rev <= st.SyncRev && r.Header.Get("X-Force") != "1" {
		http.Error(w, fmt.Sprintf("stale revision %d (have %d)", rev, st.SyncRev), 409)
		return
	}
	zr, err := gzip.NewReader(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		http.Error(w, "bad gzip", 400)
		return
	}
	xml, err := io.ReadAll(io.LimitReader(zr, 256<<20))
	if err != nil || shaHex(xml) != r.Header.Get("X-SHA256") {
		http.Error(w, "checksum mismatch", 400)
		return
	}
	if want, err := decodePackages(r.Header.Get("X-Packages")); err != nil {
		http.Error(w, "bad package list", 400)
		return
	} else if err := d.ensurePackages(want); err != nil {
		d.event("error", "package replication: %v", err)
		d.store.Update(func(s *State) { s.SyncError = err.Error() })
		http.Error(w, err.Error(), 500)
		return
	}
	if err := d.applyConfig(xml); err != nil {
		log.Printf("sync: apply of revision %d FAILED, rolled back: %v", rev, err)
		d.store.Update(func(s *State) { s.SyncError = err.Error() })
		http.Error(w, err.Error(), 500)
		return
	}
	log.Printf("sync: revision %d applied (%s)", rev, r.Header.Get("X-SHA256")[:12])
	d.store.Update(func(s *State) {
		s.SyncRev, s.AppliedRev, s.AppliedSHA, s.SyncError = rev, rev, r.Header.Get("X-SHA256"), ""
		s.SyncOK = time.Now().UTC().Format(time.RFC3339)
	})
	w.WriteHeader(200)
}

func (d *Daemon) applyConfig(xml []byte) error {
	d.ops.mu.Lock()
	defer d.ops.mu.Unlock()
	if d.store.Get().State != StateStandby {
		return errors.New("node is no longer standby")
	}
	dir := d.store.dir
	full := activeXML(dir)
	incoming := filepath.Join(dir, "incoming.xml")
	prevFull, prevCfg := full+".prev", configPath()+".prev"

	if err := writeFileDurable(incoming, xml, 0o600); err != nil {
		return err
	}
	// snapshots for rollback
	hadFull := copyFile(full, prevFull, 0o600) == nil
	if err := copyFile(configPath(), prevCfg, 0o640); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	rollback := func(cause error) error {
		if hadFull {
			copyFile(prevFull, full, 0o600)
		}
		unlockConfig()
		copyFile(prevCfg, configPath(), 0o640)
		lockConfig()
		d.ops.cache, d.ops.mtime = IfaceSet{}, time.Time{}
		sh("configctl", "filter", "reload")
		d.enforceOnce()
		return fmt.Errorf("%w (rolled back to the previous good configuration)", cause)
	}

	// node-local parts come from the LIVE config.xml, so settings edited on the standby itself survive
	base := configPath()
	out := full + ".new"
	if o, err := sh(mergeScript, incoming, base, out); err != nil {
		os.Remove(out)
		return fmt.Errorf("merge: %v: %s", err, o)
	}
	if _, err := loadIfaces(out); err != nil {
		os.Remove(out)
		return fmt.Errorf("merged config failed validation: %w", err)
	}
	syncFile(out) // the PHP helper wrote it: flush before it replaces the good copy
	if err := os.Rename(out, full); err != nil {
		return rollback(err)
	}
	syncDir(dir)
	if err := d.writeStandbyView(full); err != nil {
		return rollback(err)
	}
	d.ops.cache, d.ops.mtime = IfaceSet{}, time.Time{}
	if o, err := sh("configctl", "filter", "reload"); err != nil {
		return rollback(fmt.Errorf("filter reload: %v: %s", err, o))
	}
	d.enforceOnce() // belt and braces: nothing may have come up
	os.Rename(incoming, filepath.Join(dir, "last-incoming.xml"))
	return nil
}

// syncStatus summarises replication for the GUI: in_sync | behind | failed.
func (d *Daemon) syncStatus(st State) map[string]any {
	out := map[string]any{"rev": st.SyncRev, "last_ok": st.SyncOK, "error": st.SyncError}
	switch {
	case st.State == StateStandby:
		out["direction"] = "receiving"
		out["applied_rev"] = st.AppliedRev
		out["state"] = map[bool]string{true: "failed", false: "in_sync"}[st.SyncError != ""]
	case st.State == StateActive:
		out["direction"] = "sending"
		out["acked_rev"] = st.AckedRev
		b, err := os.ReadFile(configPath())
		switch {
		case st.SyncError != "":
			out["state"] = "failed"
		case err == nil && shaHex(b) == st.AckedSHA:
			out["state"] = "in_sync"
			out["behind"] = 0
		default:
			out["state"] = "behind"
			out["behind"] = d.revisionsSince(st.SyncOK)
		}
	}
	return out
}

// revisionsSince counts OPNsense config backups newer than the last ack: one per saved change (at least 1).
func (d *Daemon) revisionsSince(since string) int {
	t, _ := time.Parse(time.RFC3339, since)
	files, _ := filepath.Glob("/conf/backup/config-*.xml")
	n := 0
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil && fi.ModTime().After(t) {
			n++
		}
	}
	if n == 0 {
		n = 1
	}
	return n
}

// pinMACs runs the helper that writes the live MAC of every enabled data interface into <spoofmac> if missing.
// Returns true when it changed config.xml.
func (d *Daemon) pinMACs() bool {
	out, err := sh(spoofScript)
	if err != nil {
		log.Printf("sync: pinning MACs failed: %v: %s", err, out)
		return false
	}
	if strings.HasPrefix(out, "pinned") {
		d.event("change", "pinned shared MAC for new interface(s): %s", strings.TrimPrefix(out, "pinned "))
		return true
	}
	return false
}
