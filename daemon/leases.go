package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DHCP lease replication. The configuration of the DHCP servers replicates with config.xml, but the leases (who got which
// address) live in files: without them a new active node would hand out addresses that are already in use. The active node pushes
// the lease files of dnsmasq and Kea to the standby whenever they change; the standby writes them to the same paths so the
// service reads them when it is (re)started on promote.

// only these names are accepted from the peer, mapped to fixed paths
var leaseFiles = map[string]string{
	"dnsmasq.leases":  "/var/db/dnsmasq.leases",
	"kea-leases4.csv": "/var/db/kea/kea-leases4.csv",
	"kea-leases6.csv": "/var/db/kea/kea-leases6.csv",
}

type leaseState struct {
	mu   sync.Mutex
	sent map[string]string
}

func (l *leaseState) reset() {
	l.mu.Lock()
	l.sent = nil
	l.mu.Unlock()
}

func (d *Daemon) leaseLoop() {
	for range time.Tick(10 * time.Second) {
		d.leaseStep()
	}
}

func (d *Daemon) leaseStep() {
	st := d.store.Get()
	if st.PeerFP == "" || st.State != StateActive {
		return
	}
	alive, _, peer := d.hb.Snapshot()
	if !alive || peer.State != StateStandby {
		return
	}
	d.leases.mu.Lock()
	if d.leases.sent == nil {
		d.leases.sent = map[string]string{}
	}
	d.leases.mu.Unlock()
	for name, path := range leaseFiles {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		sha := shaHex(b)
		d.leases.mu.Lock()
		same := d.leases.sent[name] == sha
		d.leases.mu.Unlock()
		if same {
			continue
		}
		var z bytes.Buffer
		zw := gzip.NewWriter(&z)
		zw.Write(b)
		zw.Close()
		cl := d.peerClient(st.PeerFP, nil)
		cl.Timeout = 15 * time.Second
		resp, err := cl.Post("https://"+net.JoinHostPort(st.PeerAddr, fmt.Sprint(st.PeerPort))+"/v1/file?name="+url.QueryEscape(name), "application/octet-stream", &z)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == 200 {
			d.leases.mu.Lock()
			d.leases.sent[name] = sha
			d.leases.mu.Unlock()
		}
	}
}

// handleFile (standby): accept a lease file from the active peer.
func (d *Daemon) handleFile(w http.ResponseWriter, r *http.Request) {
	if d.store.Get().State != StateStandby {
		http.Error(w, "not standby", http.StatusConflict)
		return
	}
	path, ok := leaseFiles[r.URL.Query().Get("name")]
	if !ok {
		http.Error(w, "file not allowed", http.StatusForbidden)
		return
	}
	zr, err := gzip.NewReader(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		http.Error(w, "bad gzip", 400)
		return
	}
	b, err := io.ReadAll(io.LimitReader(zr, 64<<20))
	if err != nil {
		http.Error(w, "bad body", 400)
		return
	}
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := writeFileDurable(path, b, 0o644); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(200)
}
