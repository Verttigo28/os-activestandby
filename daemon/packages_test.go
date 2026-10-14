package main

import (
	"context"
	"strings"
	"sync/atomic"
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
	d.blockPackageInstall()
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
	d.unblockPackageInstall()
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

// Reported by the audit of 11372fc: a promotion that started between the role check of ensurePackages and the registration of the
// installation found nothing to stop, and the installation then ran next to the takeover (reproduced by this test on 1.0.4).
// Now the check and the registration happen under the lock the promotion takes: both orders are safe.
func TestPromotionAndInstallationCannotOverlap(t *testing.T) {
	var started atomic.Int32
	old := pkgInstall
	pkgInstall = func(ctx context.Context, _, _ []string) ([]byte, error) {
		started.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	t.Cleanup(func() { pkgInstall = old })

	t.Run("the promotion comes first: the installation does not start", func(t *testing.T) {
		started.Store(0)
		d := newHANode(t, standbyState(RoleStandby, 5))
		d.blockPackageInstall()
		if err := d.ensurePackages(map[string]string{"os-example": "1.0"}); err == nil {
			t.Fatal("an installation started during a promotion")
		}
		if started.Load() != 0 {
			t.Fatal("pkg was started")
		}
		d.unblockPackageInstall()
	})

	t.Run("the promotion arrives while the installation is being registered: it is cancelled", func(t *testing.T) {
		started.Store(0)
		d := newHANode(t, standbyState(RoleStandby, 5))
		atCheck, release := make(chan struct{}), make(chan struct{})
		pkgAfterRoleCheck = func() { close(atCheck); <-release }
		t.Cleanup(func() { pkgAfterRoleCheck = nil })
		res := make(chan error, 1)
		go func() { res <- d.ensurePackages(map[string]string{"os-example": "1.0"}) }()
		<-atCheck // the installation passed its role check and holds the lock
		blocked := make(chan struct{})
		go func() { d.blockPackageInstall(); close(blocked) }()
		time.Sleep(50 * time.Millisecond) // the promotion is waiting for the lock
		close(release)
		select {
		case err := <-res:
			if err == nil || !strings.Contains(err.Error(), "interrupted") {
				t.Fatalf("err = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the installation still runs next to the promotion")
		}
		select {
		case <-blocked:
		case <-time.After(5 * time.Second):
			t.Fatal("the promotion waited too long")
		}
		d.unblockPackageInstall()
	})
}

// a promotion goes on 15 s after it killed an installation that does not end (what can still run then cannot be stopped from here)
func TestPromotionDoesNotWaitForeverForAnInstallation(t *testing.T) {
	d := newHANode(t, standbyState(RoleStandby, 5))
	old, oldWait := pkgInstall, pkgStopWait
	pkgStopWait = 100 * time.Millisecond
	hang := make(chan struct{})
	pkgInstall = func(context.Context, []string, []string) ([]byte, error) { <-hang; return nil, nil } // ignores the cancellation
	finished := make(chan struct{})
	t.Cleanup(func() {
		close(hang) // the installation ends, its goroutine returns, and only then the replaced functions are restored
		<-finished
		pkgInstall, pkgStopWait = old, oldWait
	})
	go func() { d.ensurePackages(map[string]string{"os-example": "1.0"}); close(finished) }()
	for i := 0; i < 100; i++ {
		d.pkgr.mu.Lock()
		running := d.pkgr.cancel != nil
		d.pkgr.mu.Unlock()
		if running {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	t0 := time.Now()
	d.blockPackageInstall()
	if time.Since(t0) > 2*time.Second {
		t.Fatalf("waited %s", time.Since(t0))
	}
	if err := d.ensurePackages(map[string]string{"os-other": "1.0"}); err == nil {
		t.Fatal("no new installation while the promotion runs")
	}
	d.unblockPackageInstall()
}

// an automatic takeover does not start next to a software update that replaces the base system and the packages on the same node
func TestNoAutomaticTakeoverWhileASoftwareUpdateRuns(t *testing.T) {
	d := newHANode(t, standbyState(RoleStandby, 5))
	d.upd = &updateRun{}
	var promoted atomic.Int32
	d.act.promote = func(string) error { promoted.Add(1); return nil }
	d.upd.mu.Lock()
	d.upd.running = true
	d.upd.mu.Unlock()
	d.autoPromote("test", false)
	if promoted.Load() != 0 || !strings.Contains(d.auto.Blocked(), "software update") {
		t.Fatalf("promoted %d, reason %q", promoted.Load(), d.auto.Blocked())
	}
	d.upd.mu.Lock()
	d.upd.running = false
	d.upd.mu.Unlock()
	d.autoPromote("test", false)
	if promoted.Load() != 1 {
		t.Fatal("once the update ended the takeover goes ahead")
	}
}
