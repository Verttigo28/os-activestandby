package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func settingsIn(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HASYNC_DIR", dir)
	p := filepath.Join(dir, "hasyncd.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HASYNC_CONF", p)
	return p
}

// Reported by the audit of ba21e4d: a settings file that breaks in the middle must not be half used. Here fence_required comes after
// the error: decoded "as far as possible" it would silently be false.
func TestBrokenSettingsAreNeverHalfUsed(t *testing.T) {
	p := settingsIn(t, `{"detect_time": 3, "fence_required": true, "auto_failover": true, "fence_command": "/root/fence.sh"}`)
	good, problem := loadConfigChecked()
	if problem != "" || !good.FenceRequired {
		t.Fatalf("a valid file: %q %+v", problem, good)
	}
	// the file is broken (a truncated write, an edit by hand)
	os.WriteFile(p, []byte(`{"detect_time": 1, "auto_failover": true, "fence_requ`), 0o600)
	c, problem := loadConfigChecked()
	if problem == "" || !strings.Contains(problem, "last valid settings") {
		t.Fatalf("the problem must be reported: %q", problem)
	}
	if !c.FenceRequired || c.FenceCommand != "/root/fence.sh" || c.DetectTime != 3 {
		t.Fatalf("the last valid settings must be used, not a mix: %+v", c)
	}
}

func TestBrokenSettingsWithoutAnyValidCopyAreFailSafe(t *testing.T) {
	settingsIn(t, `{"auto_failover": true, "fence_requ`)
	c, problem := loadConfigChecked()
	if problem == "" || c.autoOn() || !c.FenceRequired {
		t.Fatalf("no earlier settings: automatic failover off and fencing required, got auto=%v fence=%v problem=%q", c.autoOn(), c.FenceRequired, problem)
	}
}

func TestMissingSettingsFileIsAFreshInstall(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HASYNC_DIR", dir)
	t.Setenv("HASYNC_CONF", filepath.Join(dir, "none.json"))
	c, problem := loadConfigChecked()
	if problem != "" || c.ListenPort != 7690 || !c.autoOn() {
		t.Fatalf("%q %+v", problem, c)
	}
}

// a field of a wrong type is an error of the whole file too (decoding stops there)
func TestSettingsWithAWrongTypeAreRefused(t *testing.T) {
	settingsIn(t, `{"detect_time": "three", "fence_required": true}`)
	if _, problem := loadConfigChecked(); problem == "" {
		t.Fatal("must be reported")
	}
}
