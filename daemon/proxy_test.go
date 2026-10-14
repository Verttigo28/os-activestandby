package main

import (
	"net"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestProxyRefusesLocalAndLinkLocalAddresses(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "::1", "169.254.1.1", "fe80::1", "0.0.0.0", "224.0.0.1", "ff02::1"} {
		if !proxyForbiddenIP(net.ParseIP(ip)) {
			t.Errorf("%s must be refused", ip)
		}
	}
	for _, ip := range []string{"10.1.2.3", "192.168.1.1", "172.16.5.5", "203.0.113.7", "2001:db8::1"} {
		if proxyForbiddenIP(net.ParseIP(ip)) {
			t.Errorf("%s is a possible mirror (an internal one included)", ip)
		}
	}
	// the check is made at connect time, on the resolved address: a name that resolves to loopback gets no connection
	if runtime.GOOS == "js" {
		t.Skip("no sockets under the wasm runner (runs on FreeBSD and in the CI)")
	}
	if c, err := proxyDialer.Dial("tcp", "127.0.0.1:9"); err == nil || !strings.Contains(err.Error(), "does not connect") {
		if c != nil {
			c.Close()
		}
		t.Fatalf("a connection to loopback through the proxy dialer: %v", err)
	}
}

func TestProxyPorts(t *testing.T) {
	for p, want := range map[string]bool{"80": true, "443": true, "6379": false, "22": false, "7690": false, "": false} {
		if proxyPortOK(p) != want {
			t.Errorf("port %q: want %v", p, want)
		}
	}
	for raw, want := range map[string]string{"http://a.example/x": "80", "https://a.example/x": "443", "http://a.example:6379/x": "6379"} {
		u, _ := url.Parse(raw)
		if urlPort(u) != want {
			t.Errorf("%s -> %s, want %s", raw, urlPort(u), want)
		}
	}
}

func TestCommentedRepositoryLineIsNotARepository(t *testing.T) {
	conf := "OPNsense: {\n  url: \"pkg+https://pkg.opnsense.org/x\",\n  # url: \"https://evil.example/y\"\n  #url: \"https://evil2.example/y\"\n}\n  url: \"https://real.example/z\"\n"
	var hosts []string
	for _, m := range repoURL.FindAllStringSubmatch(conf, -1) {
		hosts = append(hosts, m[1])
	}
	if strings.Join(hosts, ",") != "pkg.opnsense.org,real.example" {
		t.Fatalf("hosts %v", hosts)
	}
}

// the proxy serves the paired peer only, and only so many connections at a time: any host of the HA segment could otherwise hold
// tens of thousands of connections open
func TestProxyListenerServesOnlyThePeerAndCapsConnections(t *testing.T) {
	if runtime.GOOS == "js" {
		t.Skip("no sockets under the wasm runner (runs on FreeBSD and in the CI)")
	}
	d := newHANode(t, standbyState(RoleStandby, 5))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback")
	}
	defer ln.Close()
	g := &proxyGuard{Listener: ln, d: d, slot: make(chan struct{}, 2)}
	accepted := make(chan net.Conn, 8)
	go func() {
		for {
			c, err := g.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	dial := func() net.Conn {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	// not paired with this address: closed at once
	d.store.Update(func(s *State) { s.PeerAddr = "10.99.0.2" })
	c := dial()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("a source that is not the peer must be closed")
	}
	c.Close()
	// the peer (here 127.0.0.1): two connections served, the third refused
	d.store.Update(func(s *State) { s.PeerAddr = "127.0.0.1" })
	a, b := dial(), dial()
	defer a.Close()
	defer b.Close()
	for i := 0; i < 2; i++ {
		select {
		case <-accepted:
		case <-time.After(2 * time.Second):
			t.Fatal("the peer's connection was not accepted")
		}
	}
	third := dial()
	third.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := third.Read(make([]byte, 1)); err == nil {
		t.Fatal("the connection over the cap must be closed")
	}
	third.Close()
}
