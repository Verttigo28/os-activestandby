package main

import (
	"net"
	"sync"
	"time"
)

// guardListener protects the peer API port against hosts that have nothing to do with the cluster:
//   - once paired, only the peer's addresses (HA, and management if known) are served at all; any other source is
//     closed straight after accept, before a TLS handshake costs anything;
//   - the number of simultaneous connections is capped, so slow connections cannot starve the heartbeat;
//   - an accepted connection must finish its TLS handshake and first request quickly (deadline) or it is dropped.
// While unpaired every source is allowed: that is when pairing happens, and the token protects it.

const (
	maxPeerConns    = 128
	firstByteWindow = 8 * time.Second
)

type guardListener struct {
	net.Listener
	d    *Daemon
	slot chan struct{}
}

func (d *Daemon) guard(l net.Listener) net.Listener {
	return &guardListener{Listener: l, d: d, slot: make(chan struct{}, maxPeerConns)}
}

func (g *guardListener) Accept() (net.Conn, error) {
	for {
		c, err := g.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if !g.d.allowedSource(c.RemoteAddr()) {
			c.Close()
			continue
		}
		select {
		case g.slot <- struct{}{}:
		default:
			c.Close() // saturated: refuse new work rather than starve the existing connections (the heartbeat among them)
			continue
		}
		c.SetDeadline(time.Now().Add(firstByteWindow)) // net/http replaces this with its own deadlines once the request is read
		return &guardConn{Conn: c, release: func() { <-g.slot }}, nil
	}
}

type guardConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *guardConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}

// allowedSource: paired nodes talk to their peer only.
func (d *Daemon) allowedSource(a net.Addr) bool {
	st := d.store.Get()
	if st.PeerFP == "" {
		return true
	}
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return false
	}
	return host == st.PeerAddr || (st.PeerMgmt != "" && host == st.PeerMgmt)
}
