package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// a plugin installation that runs when this node has to take over is stopped first (its scripts restart configd and reconfigure
// plugins, which would collide with the takeover)
func TestTakeoverStopsAPluginInstallation(t *testing.T) {
	d := newHANode(t, standbyState(RoleStandby, 5))
	started := make(chan struct{})
	old := pkgInstall
	pkgInstall = func(ctx context.Context, _, _ []string) ([]byte, error) {
		close(started)
		<-ctx.Done() // a long installation
		return nil, ctx.Err()
	}
	t.Cleanup(func() { pkgInstall = old })
	res := make(chan error, 1)
	go func() { res <- d.ensurePackages(map[string]string{"os-example": "1.0"}) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the installation did not start")
	}
	t0 := time.Now()
	d.stopPackageInstall()
	if time.Since(t0) > 5*time.Second {
		t.Fatal("the takeover waited too long")
	}
	select {
	case err := <-res:
		if err == nil || !strings.Contains(err.Error(), "interrupted by a takeover") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the installation was not stopped")
	}
	d.stopPackageInstall() // nothing running: returns at once
}

func TestNoPluginInstallationOnANodeThatIsNotStandby(t *testing.T) {
	d := newHANode(t, activeState(RoleMaster, 5))
	called := false
	old := pkgInstall
	pkgInstall = func(context.Context, []string, []string) ([]byte, error) { called = true; return nil, nil }
	t.Cleanup(func() { pkgInstall = old })
	if err := d.ensurePackages(map[string]string{"os-example": "1.0"}); err == nil || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}
