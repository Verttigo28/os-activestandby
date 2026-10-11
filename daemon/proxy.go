package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Update proxy: the standby has no WAN, so package installs go through the ACTIVE node over the HA link.
// A minimal forward proxy listens on the HA address only (listen_port+1), accepts connections from the paired
// peer's address only, and relays only to the hosts of the configured OPNsense package repositories.

var repoURL = regexp.MustCompile(`url:\s*"(?:pkg\+)?https?://([^/:"]+)`)

func proxyPort(c Config) int { return c.ListenPort + 1 }

func allowedRepoHosts() map[string]bool {
	hosts := map[string]bool{"pkg.opnsense.org": true}
	files, _ := filepath.Glob("/usr/local/etc/pkg/repos/*.conf")
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, m := range repoURL.FindAllStringSubmatch(string(b), -1) {
			hosts[strings.ToLower(m[1])] = true
		}
	}
	return hosts
}

func (d *Daemon) hostAllowed(h string) bool {
	h = strings.ToLower(h)
	if allowedRepoHosts()[h] || strings.HasSuffix(h, ".opnsense.org") {
		return true
	}
	return false
}

func (d *Daemon) proxyPeerOnly(r *http.Request) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	st := d.store.Get()
	return st.PeerAddr != "" && host == st.PeerAddr
}

func (d *Daemon) proxyHandler(w http.ResponseWriter, r *http.Request) {
	if !d.proxyPeerOnly(r) {
		http.Error(w, "only the paired peer may use this proxy", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodConnect {
		host, port, err := net.SplitHostPort(r.Host)
		if err != nil || (port != "443" && port != "80") || !d.hostAllowed(host) {
			http.Error(w, "destination not allowed", http.StatusForbidden)
			log.Printf("proxy: refused CONNECT %s", r.Host)
			return
		}
		dst, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			dst.Close()
			http.Error(w, "hijack unsupported", 500)
			return
		}
		src, _, err := hj.Hijack()
		if err != nil {
			dst.Close()
			return
		}
		src.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		log.Printf("proxy: CONNECT %s", r.Host)
		go func() { io.Copy(dst, src); dst.Close() }()
		go func() { io.Copy(src, dst); src.Close() }()
		return
	}
	// plain http absolute-URI request
	if !r.URL.IsAbs() || !d.hostAllowed(r.URL.Hostname()) {
		http.Error(w, "destination not allowed", http.StatusForbidden)
		log.Printf("proxy: refused %s %s", r.Method, r.URL)
		return
	}
	r.RequestURI = ""
	r.Header.Del("Proxy-Connection")
	resp, err := (&http.Transport{ResponseHeaderTimeout: 30 * time.Second}).RoundTrip(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	log.Printf("proxy: %s %s -> %d", r.Method, r.URL.Host, resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (d *Daemon) proxyListener(haIP string) {
	addr := net.JoinHostPort(haIP, fmt.Sprint(proxyPort(d.cfg)))
	srv := &http.Server{Addr: addr, Handler: http.HandlerFunc(d.proxyHandler), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("update proxy for the standby listening on %s", addr)
	log.Printf("update proxy ended: %v", srv.ListenAndServe())
}
