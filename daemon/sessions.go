package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// GUI session replication: the web GUI keeps its logins in PHP session files. The active node pushes them to the
// standby every few seconds, so after a failover the browser's cookie is still valid on the new active node (same
// shared IP and same certificate, replicated with the configuration) and nobody has to log in again.
//
// A session file is a bearer credential: it only travels over the pinned peer connection and is written 0600 root.
// The standby mirrors deletions too (a log-out on the active node must not leave a valid copy behind): it removes the
// files it received earlier that the active node no longer has. Sessions created locally on a standby are never
// touched.

var (
	sessionDir  = "/var/lib/php/sessions"
	sessionName = regexp.MustCompile(`^sess_[A-Za-z0-9,-]{16,128}$`)
)

const (
	maxSessions    = 500
	maxSessionSize = 64 << 10
	sessionPushGap = 2 * time.Second
)

type sessionFile struct {
	Name  string `json:"name"`
	Data  []byte `json:"data"` // base64 in JSON
	MTime int64  `json:"mtime"`
}

type sessionPush struct {
	Files []sessionFile `json:"files"`
}

type sessionSync struct {
	mu       sync.Mutex
	lastHash string
	tracked  map[string]bool // files on this node that came from the peer (or existed when this node became standby)
}

func newSessionSync() *sessionSync { return &sessionSync{tracked: map[string]bool{}} }

// collectSessions reads the live sessions (empty files are fresh anonymous sessions that carry nothing).
func collectSessions(dir string) []sessionFile {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []sessionFile
	for _, e := range ents {
		if !sessionName.MatchString(e.Name()) || e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil || fi.Size() == 0 || fi.Size() > maxSessionSize {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil || len(b) == 0 {
			continue
		}
		out = append(out, sessionFile{Name: e.Name(), Data: b, MTime: fi.ModTime().Unix()})
		if len(out) >= maxSessions {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func sessionsHash(fs []sessionFile) string {
	h := sha256.New()
	for _, f := range fs {
		h.Write([]byte(f.Name))
		h.Write(f.Data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// applySessions installs the pushed set and removes what the active node no longer has.
func (s *sessionSync) applySessions(dir string, files []sessionFile) (written, removed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := map[string]bool{}
	for _, f := range files {
		if !sessionName.MatchString(f.Name) || len(f.Data) == 0 || len(f.Data) > maxSessionSize {
			continue
		}
		keep[f.Name] = true
		p := filepath.Join(dir, f.Name)
		if cur, err := os.ReadFile(p); err == nil && string(cur) == string(f.Data) {
			s.tracked[f.Name] = true
			continue
		}
		tmp := p + ".tmp"
		if os.WriteFile(tmp, f.Data, 0o600) != nil {
			continue
		}
		t := time.Unix(f.MTime, 0)
		os.Chtimes(tmp, t, t)
		if os.Rename(tmp, p) == nil {
			s.tracked[f.Name] = true
			written++
		}
	}
	for name := range s.tracked {
		if !keep[name] {
			os.Remove(filepath.Join(dir, name))
			delete(s.tracked, name)
			removed++
		}
	}
	return
}

// trackExisting: when a node becomes standby, everything in the session directory was (or may have been) copied to
// the peer while it was active, so the peer's later deletions must reach these files as well.
func (s *sessionSync) trackExisting(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if sessionName.MatchString(e.Name()) {
			s.tracked[e.Name()] = true
		}
	}
}

func (d *Daemon) handleSessions(w http.ResponseWriter, r *http.Request) {
	if d.store.Get().State != StateStandby || !d.cfg.syncSessions() {
		writeJSON(w, 200, map[string]string{"status": "ignored"})
		return
	}
	var p sessionPush
	if json.NewDecoder(io.LimitReader(r.Body, int64(maxSessions)*(maxSessionSize*4/3+256))).Decode(&p) != nil || len(p.Files) > maxSessions {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	wr, rm := d.sess.applySessions(sessionDir, p.Files)
	writeJSON(w, 200, map[string]int{"written": wr, "removed": rm})
}

// sessionLoop runs on the active node.
func (d *Daemon) sessionLoop() {
	for range time.Tick(sessionPushGap) {
		st := d.store.Get()
		if st.State != StateActive || st.PeerFP == "" || !d.cfg.syncSessions() {
			continue
		}
		if alive, _, peer := d.hb.Snapshot(); !alive || peer.State != StateStandby {
			d.sess.mu.Lock()
			d.sess.lastHash = "" // push everything again when the standby comes back
			d.sess.mu.Unlock()
			continue
		}
		files := collectSessions(sessionDir)
		h := sessionsHash(files)
		d.sess.mu.Lock()
		same := h == d.sess.lastHash
		d.sess.mu.Unlock()
		if same {
			continue
		}
		if _, err := d.peerPostJSON("/v1/sessions", sessionPush{Files: files}); err != nil {
			log.Printf("session sync: %v", err)
			continue
		}
		d.sess.mu.Lock()
		d.sess.lastHash = h
		d.sess.mu.Unlock()
	}
}
