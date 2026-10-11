package main

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

type testNode struct {
	d    *Daemon
	srv  *httptest.Server
	host string
	port int
}

func newNode(t *testing.T) *testNode {
	t.Helper()
	dir := t.TempDir()
	st, err := (func() (*Store, error) { t.Setenv("HASYNC_DIR", dir); return openStore() })()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := loadOrCreateCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{store: st, cert: cert, fp: fingerprint(cert.Certificate[0]), pairing: &Pairing{}, ops: &Ops{}, hb: newHeartbeat(), auto: &autoState{}, sync: &syncCtl{wake: make(chan struct{}, 1)}}
	srv := httptest.NewUnstartedServer(d.peerMux())
	srv.TLS = d.peerTLS()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	host, p, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	port, _ := strconv.Atoi(p)
	d.cfg.ListenPort = port
	return &testNode{d: d, srv: srv, host: host, port: port}
}

func TestPairingHappyPath(t *testing.T) {
	master, standby := newNode(t), newNode(t)
	tok, _, err := standby.d.pairing.Start()
	if err != nil {
		t.Fatal(err)
	}
	res, diffs, err := master.d.doPair(standby.host, standby.port, tok)
	if err != nil || len(diffs) != 0 {
		t.Fatalf("pair failed: %v %v", err, diffs)
	}
	if res.FP != standby.d.fp {
		t.Fatal("master pinned wrong fingerprint")
	}
	if m, s := master.d.store.Get(), standby.d.store.Get(); m.Role != RoleMaster || s.Role != RoleStandby ||
		m.PeerFP != standby.d.fp || s.PeerFP != master.d.fp {
		t.Fatalf("bad state: %+v %+v", m, s)
	}
	// token is single use
	if _, _, err := newNode(t).d.doPair(standby.host, standby.port, tok); err == nil {
		t.Fatal("second pairing must fail")
	}
}

func TestPairingWrongToken(t *testing.T) {
	master, standby := newNode(t), newNode(t)
	standby.d.pairing.Start()
	if _, _, err := master.d.doPair(standby.host, standby.port, "AAAAA-AAAAA-AAAAA-AAAAA"); err == nil {
		t.Fatal("wrong token must fail")
	}
	if master.d.store.Get().Role != RoleStandalone || standby.d.store.Get().Role != RoleStandalone {
		t.Fatal("nobody may be paired after a failed attempt")
	}
}

func TestPairingRequiresPrepare(t *testing.T) {
	master, standby := newNode(t), newNode(t)
	if _, _, err := master.d.doPair(standby.host, standby.port, "AAAAA-AAAAA-AAAAA-AAAAA"); err == nil {
		t.Fatal("standby not in pairing mode must refuse")
	}
}

func TestPairingRefusesDifferentNodes(t *testing.T) {
	master, standby := newNode(t), newNode(t)
	tok, _, _ := standby.d.pairing.Start()
	// Compare() is exercised directly; collectInfo() is identical on both test nodes.
	a, b := Info{Version: "26.7", NICs: []NIC{{Name: "ixl0"}}}, Info{Version: "26.7", NICs: []NIC{{Name: "igb0"}}}
	if len(a.Compare(b)) == 0 {
		t.Fatal("different NIC names must be reported")
	}
	if len(a.Compare(a)) != 0 {
		t.Fatal("identical info must pass")
	}
	c := Info{Version: "26.7", Packages: map[string]string{"os-x": "1"}}
	if len(c.Compare(Info{Version: "26.7", Packages: map[string]string{}})) == 0 {
		t.Fatal("package mismatch must be reported")
	}
	_ = master
	_ = tok
}

func TestPeerEndpointsPinned(t *testing.T) {
	master, standby := newNode(t), newNode(t)
	tok, _, _ := standby.d.pairing.Start()
	if _, _, err := master.d.doPair(standby.host, standby.port, tok); err != nil {
		t.Fatal(err)
	}
	if !master.d.pingPeer() {
		t.Fatal("paired master must reach standby")
	}
	// a stranger with its own cert is rejected
	stranger := newNode(t)
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, Certificates: []tls.Certificate{stranger.d.cert}}}}
	resp, err := cl.Get("https://" + net.JoinHostPort(standby.host, strconv.Itoa(standby.port)) + "/v1/ping")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 403 {
		t.Fatalf("stranger got %d, want 403", resp.StatusCode)
	}
}

func TestBreakPair(t *testing.T) {
	master, standby := newNode(t), newNode(t)
	tok, _, _ := standby.d.pairing.Start()
	if _, _, err := master.d.doPair(standby.host, standby.port, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := master.d.breakPair(); err != nil {
		t.Fatal(err)
	}
	if master.d.store.Get().Role != RoleStandalone || standby.d.store.Get().Role != RoleStandalone {
		t.Fatal("both nodes must be standalone after break")
	}
}
