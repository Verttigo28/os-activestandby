package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPluginVersionsAreOrdered(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.0.5", "1.0.4", 1}, {"1.0.4", "1.0.5", -1}, {"1.0.5", "1.0.5", 0}, {"1.0.10", "1.0.9", 1}, {"1.1.0", "1.0.99", 1}, {"2.0.0", "1.9.9", 1},
		{"1.0.5", "1.0.5-dev", 1}, // a release is newer than its pre-release
		{"1.0.5-dev", "1.0.4", 1}, // a pre-release of 1.0.5 is newer than 1.0.4
		{"1.0.5.d1", "1.0.5", -1}, // the package name of a development build
		{"1.0.5.d1", "1.0.4", 1},
		{"1.0", "1.0.0", 0},
	} {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// two daemons over real mutual TLS: the older fetches the package of the newer, checks it, and installs it; and everything that must
// stop it, stops it
func TestOlderNodeFetchesAndInstallsThePluginOfTheNewerOne(t *testing.T) {
	if runtime.GOOS == "js" {
		t.Skip("no sockets under the wasm runner (runs on FreeBSD and in the CI)")
	}
	src := newHANode(t, activeState(RoleMaster, 5))
	dst := newHANode(t, standbyState(RoleStandby, 5))
	for _, d := range []*Daemon{src, dst} {
		c, err := loadOrCreateCert(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		d.cert, d.fp = c, fingerprint(c.Certificate[0])
	}
	src.pluginVer, dst.pluginVer = "1.0.5", "1.0.4"
	srv := httptest.NewUnstartedServer(src.peerMux())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{src.cert}, ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	src.store.Update(func(s *State) { s.PeerFP = dst.fp })
	dst.store.Update(func(s *State) { s.PeerFP, s.PeerAddr, s.PeerPort = src.fp, "127.0.0.1", p })
	dst.health = newHealthState()

	pkgBytes := []byte("this stands for the bytes of os-activestandby-1.0.5.pkg")
	oldBuild, oldVerify, oldAdd := buildPluginPackage, verifyPluginPackage, addPluginPackage
	t.Cleanup(func() { buildPluginPackage, verifyPluginPackage, addPluginPackage = oldBuild, oldVerify, oldAdd })
	buildPluginPackage = func(dir string) (string, error) {
		os.MkdirAll(dir, 0o700)
		f := filepath.Join(dir, "os-activestandby-1.0.5.pkg")
		return f, os.WriteFile(f, pkgBytes, 0o600)
	}
	verified := ""
	verifyPluginPackage = func(path string) (string, error) {
		b, _ := os.ReadFile(path)
		verified = string(b)
		return "1.0.5", nil
	}
	var installed []byte
	addPluginPackage = func(_ context.Context, path string) ([]byte, error) {
		installed, _ = os.ReadFile(path)
		return nil, nil
	}

	msg, err := dst.pullPlugin()
	if err != nil || !strings.Contains(msg, "installed") {
		t.Fatalf("%q %v", msg, err)
	}
	if string(installed) != string(pkgBytes) || verified != string(pkgBytes) {
		t.Fatal("the installed package is not the one the peer sent, or it was not verified before")
	}
	if _, err := os.Stat(filepath.Join(dst.store.dir, "plugin-in.pkg")); err == nil {
		t.Fatal("the package file must be removed afterwards")
	}

	// never a downgrade: the peer is OLDER
	installed = nil
	src.pluginVer = "1.0.3"
	time.Sleep(11 * time.Second) // the source builds at most one package every 10 s
	if msg, err := dst.pullPlugin(); err != nil || installed != nil || !strings.Contains(msg, "nothing to install") {
		t.Fatalf("a package older than the installed one must not be installed: %q %v installed=%v", msg, err, installed != nil)
	}

	// a package that is not this plugin is refused before it is installed
	src.pluginVer = "1.0.9"
	verifyPluginPackage = func(string) (string, error) { return "", os.ErrInvalid }
	time.Sleep(11 * time.Second)
	if _, err := dst.pullPlugin(); err == nil || installed != nil {
		t.Fatal("a package that fails verification must not be installed")
	}
}

// the node that is asked to fetch checks that the peer really has something newer
func TestPluginPullIsRefusedWithoutANewerPeer(t *testing.T) {
	d := newHANode(t, standbyState(RoleStandby, 5))
	d.pluginVer = "1.0.5"
	for _, tc := range []struct {
		name, peer string
		heard      bool
		want       int
	}{
		{"peer not heard", "", false, 409},
		{"same version", "1.0.5", true, 409},
		{"peer older", "1.0.4", true, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.heard {
				seePeer(d, hbMsg{State: StateActive, Plug: tc.peer})
			}
			rec := httptest.NewRecorder()
			d.handlePluginPull(rec, httptest.NewRequest("POST", "/v1/plugin/pull", nil))
			if rec.Code != tc.want {
				t.Fatalf("%d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}
