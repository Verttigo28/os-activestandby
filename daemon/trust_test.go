package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestTrustedDirRefusesWhatRootShouldNotRun(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "js" {
		t.Skip("permission bits and ownership are meaningless here")
	}
	if trustedDir("relative/dir") == nil {
		t.Error("a relative path must be refused")
	}
	if trustedDir(filepath.Join(t.TempDir(), "missing")) == nil {
		t.Error("a missing directory must be refused")
	}
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, nil, 0o600)
	if trustedDir(f) == nil {
		t.Error("a file is not a directory")
	}
	open := t.TempDir()
	os.Chmod(open, 0o777)
	if trustedDir(open) == nil {
		t.Error("a world-writable directory must be refused")
	}
	grp := t.TempDir()
	os.Chmod(grp, 0o775)
	if trustedDir(grp) == nil {
		t.Error("a group-writable directory must be refused")
	}
	link := filepath.Join(t.TempDir(), "link")
	good := t.TempDir()
	os.Chmod(good, 0o755)
	if os.Symlink(good, link) == nil && trustedDir(link) == nil {
		t.Error("a symlink must be refused: its target could be swapped")
	}
}

func TestEnvOverridesAreIgnoredWhenUntrusted(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "js" {
		t.Skip("permission bits and ownership are meaningless here")
	}
	open := t.TempDir()
	os.Chmod(open, 0o777)
	t.Setenv("HASYNC_SCRIPTS", open)
	if got := envDir("HASYNC_SCRIPTS", "/default"); got != "/default" {
		t.Fatalf("an untrusted scripts directory was honoured: %q", got)
	}
	t.Setenv("HASYNC_PATH", open+":/usr/bin")
	if got := searchPath(); got != defaultPATH {
		t.Fatalf("an untrusted search path was honoured: %q", got)
	}
	t.Setenv("HASYNC_PATH", "")
	if searchPath() != defaultPATH {
		t.Fatal("no override: the built-in path")
	}
}
