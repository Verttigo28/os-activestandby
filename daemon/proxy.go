package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Update proxy: the standby has no WAN, so package installs go through the ACTIVE node over the HA link.
// A minimal forward proxy listens on the HA address only (listen_port+1), accepts connections from the paired
// peer's address only, and relays only to the hosts of the configured OPNsense package repositories.

// only a real `url:` line of a repository file: a commented-out line (`# url: ...`) is not a repository
var repoURL = regexp.MustCompile(`(?m)^[ \t]*url:\s*"(?:pkg\+)?https?://([^/:"]+)`)

// proxyDialer refuses to connect to an address that has no business being a package mirror: loopback (the GUI and the daemon of this
// node), link-local, unspecified and multicast. The check is made on the address that is about to be connected to, after the name was
// resolved, so a name that resolves to such an address (a mirror whose DNS is changed, DNS rebinding) is refused too. Private
// (RFC 1918) addresses stay allowed: an internal mirror is a legitimate setup.
var proxyDialer = &net.Dialer{
	Timeout: 10 * time.Second,
	Control: func(network, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip == nil || proxyForbiddenIP(ip) {
			return fmt.Errorf("the update proxy does not connect to %s", host)
		}
		return nil
	},
}

func proxyForbiddenIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast()
}

// proxyPortOK: a repository is read over HTTP or HTTPS; no other port of an allowed host is reachable through the proxy.
func proxyPortOK(port string) bool { return port == "80" || port == "443" }

const (
	proxyMaxConns   = 32               // simultaneous connections, tunnels included (an update uses a handful)
	proxyMaxTunnel  = 2 * time.Hour    // no tunnel lives longer
	proxyTunnelIdle = 10 * time.Minute // ... nor sits idle longer
)

// proxyGuard serves only the paired peer, closes everybody else right after accept (before a request is parsed), and caps the number
// of connections: any host of the HA segment could otherwise hold tens of thousands of connections open and exhaust the daemon.
type proxyGuard struct {
	net.Listener
	d    *Daemon
	slot chan struct{}
}

func (g *proxyGuard) Accept() (net.Conn, error) {
	for {
		c, err := g.Listener.Accept()
		if err != nil {
			return nil, err
		}
		host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
		if st := g.d.store.Get(); st.PeerAddr == "" || host != st.PeerAddr {
			c.Close()
			continue
		}
		select {
		case g.slot <- struct{}{}:
		default:
			c.Close()
			continue
		}
		return &guardConn{Conn: c, release: func() { <-g.slot }}, nil
	}
}

func proxyPort(c Config) int { return c.ListenPort + 1 }

func allowedRepoHosts() map[string]bool {
	hosts := map[string]bool{"pkg.opnsense.org": true}
	var files []string
	for _, pat := range []string{"/usr/local/etc/pkg/repos/*.conf", "/etc/pkg/*.conf", "/usr/local/etc/pkg.conf"} {
		f, _ := filepath.Glob(pat)
		files = append(files, f...)
	}
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
		if err != nil || !proxyPortOK(port) || !d.hostAllowed(host) {
			http.Error(w, "destination not allowed", http.StatusForbidden)
			log.Printf("proxy: refused CONNECT %s", r.Host)
			return
		}
		dst, err := proxyDialer.Dial("tcp", r.Host)
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
		// a tunnel that carries nothing for 10 minutes is closed: half-open connections must not pile up (each holds two goroutines)
		time.AfterFunc(proxyMaxTunnel, func() { src.Close(); dst.Close() }) // no tunnel lives for ever
		go func() { copyIdle(dst, src, proxyTunnelIdle); dst.Close(); src.Close() }()
		go func() { copyIdle(src, dst, proxyTunnelIdle); src.Close(); dst.Close() }()
		return
	}
	// plain http absolute-URI request
	if !r.URL.IsAbs() || !d.hostAllowed(r.URL.Hostname()) || !proxyPortOK(urlPort(r.URL)) {
		http.Error(w, "destination not allowed", http.StatusForbidden)
		log.Printf("proxy: refused %s %s", r.Method, r.URL)
		return
	}
	r.RequestURI = ""
	r.Header.Del("Proxy-Connection")
	resp, err := proxyTransport.RoundTrip(r)
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
	srv := &http.Server{Addr: addr, Handler: http.HandlerFunc(d.proxyHandler), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("update proxy: %v", err)
		return
	}
	log.Printf("update proxy for the standby listening on %s", addr)
	log.Printf("update proxy ended: %v", srv.Serve(&proxyGuard{Listener: ln, d: d, slot: make(chan struct{}, proxyMaxConns)}))
}

// one transport for every plain HTTP request of the proxy, with idle connections closed: a transport per request kept every idle
// connection (and its goroutines) for ever
var proxyTransport = &http.Transport{DialContext: proxyDialer.DialContext, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 90 * time.Second, MaxIdleConns: 16}

// copyIdle copies src to dst until EOF, an error, or idle without any byte read for that long.
func copyIdle(dst io.Writer, src net.Conn, idle time.Duration) {
	buf := make([]byte, 32<<10)
	for {
		src.SetReadDeadline(time.Now().Add(idle))
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// urlPort is the port of a request URL ("80" for plain http, "443" for https without an explicit port).
func urlPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}
