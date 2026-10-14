package main

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func identity(t *testing.T) (*x509.Certificate, string) {
	t.Helper()
	c, err := loadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	x, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return x, fingerprint(c.Certificate[0])
}

func requestFrom(method, path string, cert *x509.Certificate) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	if cert != nil {
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	} else {
		r.TLS = nil
	}
	return r
}

// Every endpoint of the peer API is reachable by the pinned peer only, and the ones that change something by POST only. Checked on
// the real routing table, so that an endpoint added later without the wrappers is caught here.
func TestEveryPeerEndpointIsRestrictedToThePinnedPeer(t *testing.T) {
	peer, peerFP := identity(t)
	stranger, _ := identity(t)
	d := newHANode(t, standbyState(RoleStandby, 5))
	d.store.Update(func(s *State) { s.PeerFP = peerFP })
	mux := d.peerMux()
	var endpoints []string
	for _, r := range d.peerRoutes() {
		if !r.readOnly {
			endpoints = append(endpoints, r.path)
		}
	}
	if len(endpoints) < 16 {
		t.Fatalf("only %d routes that change something", len(endpoints))
	}
	for _, p := range endpoints {
		for name, cert := range map[string]*x509.Certificate{"another certificate": stranger, "no certificate": nil} {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, requestFrom(http.MethodPost, p, cert))
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s with %s: %d, want 403", p, name, rec.Code)
			}
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, requestFrom(http.MethodGet, p, peer))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s from the peer: %d, want 405", p, rec.Code)
		}
	}
	// /v1/ping reads only: the pinned peer, any method
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, requestFrom(http.MethodGet, "/v1/ping", stranger))
	if rec.Code != http.StatusForbidden {
		t.Errorf("/v1/ping with another certificate: %d", rec.Code)
	}
	// and the pinned peer gets through (here: a heartbeat)
	rec = httptest.NewRecorder()
	d.health = newHealthState()
	mux.ServeHTTP(rec, requestFrom(http.MethodPost, "/v1/hb", peer))
	if rec.Code != http.StatusOK {
		t.Errorf("heartbeat of the pinned peer: %d", rec.Code)
	}
}

// a node that is not paired accepts nothing on the peer API, whatever certificate is shown
func TestUnpairedNodeAcceptsNoPeerRequest(t *testing.T) {
	cert, _ := identity(t)
	d := newHANode(t, State{Role: RoleStandalone})
	rec := httptest.NewRecorder()
	d.peerMux().ServeHTTP(rec, requestFrom(http.MethodPost, "/v1/promote", cert))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("%d", rec.Code)
	}
}
