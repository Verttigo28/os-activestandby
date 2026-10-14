package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func pushPackages(t *testing.T, d *Daemon, rev string) *httptest.ResponseRecorder {
	t.Helper()
	d.drift = &driftState{}
	body := []byte("<opnsense/>")
	req := httptest.NewRequest(http.MethodPost, "/v1/config", gz(body))
	req.Header = withPackages(pushHeaders(rev, shaHex(body)), encodePackages(map[string]string{"os-cpu-microcode-amd": "1.0", "opnsense": "26.7"}))
	rec := httptest.NewRecorder()
	d.handleConfig(rec, req)
	return rec
}

// Reported from the field: a plugin that the standby cannot install (a third-party repository that refuses this node: Forbidden)
// made the standby reject EVERY configuration revision ("config sync failed"), so the standby ran with an old configuration while the
// active node kept changing it. The plugin set is replicated on a best-effort basis: the configuration is applied, the difference is
// reported.
func TestFailedPluginInstallDoesNotBlockTheConfigurationSync(t *testing.T) {
	holdPackages := currentPackages
	currentPackages = func() map[string]string { return map[string]string{"opnsense": "26.7"} }
	t.Cleanup(func() { currentPackages = holdPackages })
	d := newHANode(t, standbyState(RoleStandby, 5))
	var applied atomic.Int32
	d.act.apply = func([]byte) error { applied.Add(1); return nil }
	var installs atomic.Int32
	old := pkgInstall
	pkgInstall = func(context.Context, []string, []string) ([]byte, error) {
		installs.Add(1)
		return []byte("pkg: https://updates.zenarmor.net/...: Forbidden\nUnable to update repository SunnyValley"), errors.New("exit status 3")
	}
	t.Cleanup(func() { pkgInstall = old })
	rec := pushPackages(t, d, "50")
	if rec.Code != http.StatusOK {
		t.Fatalf("the push was rejected (%d): %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if applied.Load() != 1 {
		t.Fatalf("the configuration was applied %d times, want once", applied.Load())
	}
	st := d.store.Get()
	if st.SyncError != "" || st.AppliedRev != 50 {
		t.Fatalf("the standby must be in sync: error %q, applied revision %d", st.SyncError, st.AppliedRev)
	}
	if !strings.Contains(d.packageReport(), "os-cpu-microcode-amd") {
		t.Fatalf("the difference must be reported: %q", d.packageReport())
	}
	// the next push, a moment later: no new attempt (it would fail the same way and keep pkg busy)
	pushPackages(t, d, "51")
	if installs.Load() != 1 {
		t.Fatalf("pkg was run %d times: a failed installation is not retried at every push", installs.Load())
	}
	// after the retry delay it is tried again
	d.pkgRetry.mu.Lock()
	d.pkgRetry.at = time.Now().Add(-pkgRetryAfter - time.Second)
	d.pkgRetry.mu.Unlock()
	pushPackages(t, d, "52")
	if installs.Load() != 2 {
		t.Fatalf("pkg was run %d times after the delay, want 2", installs.Load())
	}
}

// a difference of the OPNsense core version still stops the synchronisation: a configuration of another version may not load
func TestCoreVersionDifferenceStillBlocksTheSync(t *testing.T) {
	old := currentPackages
	currentPackages = func() map[string]string { return map[string]string{"opnsense": "26.7"} }
	t.Cleanup(func() { currentPackages = old })
	d := newHANode(t, standbyState(RoleStandby, 5))
	d.drift = &driftState{}
	d.act.apply = func([]byte) error { t.Fatal("applied"); return nil }
	body := []byte("<opnsense/>")
	req := httptest.NewRequest(http.MethodPost, "/v1/config", gz(body))
	have := map[string]string{"opnsense": "99.9.9"}
	req.Header = withPackages(pushHeaders("5", shaHex(body)), encodePackages(have))
	rec := httptest.NewRecorder()
	d.handleConfig(rec, req)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "core version differs") {
		t.Fatalf("a configuration of another core version must be refused: %d %s", rec.Code, rec.Body.String())
	}
}
