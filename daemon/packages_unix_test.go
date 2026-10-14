//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Asked by the audit of 09281e5: the cancellation or the timeout of a plugin installation must not leave a process that can still
// change the system during a promotion. A fake "pkg" starts a child and a grandchild (a package script that runs service restarts):
// after the cancellation none of them may be alive.
func fakePkg(t *testing.T) (pidFile string) {
	t.Helper()
	dir := t.TempDir()
	pidFile = filepath.Join(dir, "pids")
	script := "#!/bin/sh\n" +
		"( sleep 300 & echo $! >> " + pidFile + "; wait ) &\n" + // a child with its own child
		"sleep 300 & echo $! >> " + pidFile + "\n" +
		"echo $$ >> " + pidFile + "\n" +
		"wait\n"
	p := filepath.Join(dir, "pkg")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := pkgBinary
	pkgBinary = p
	t.Cleanup(func() { pkgBinary = old })
	return pidFile
}

func alivePIDs(t *testing.T, pidFile string) []int {
	t.Helper()
	b, _ := os.ReadFile(pidFile)
	var alive []int
	for _, f := range strings.Fields(string(b)) {
		pid, err := strconv.Atoi(f)
		if err == nil && syscall.Kill(pid, 0) == nil {
			alive = append(alive, pid)
		}
	}
	return alive
}

func TestCancelledInstallationLeavesNoProcessBehind(t *testing.T) {
	for name, stop := range map[string]func(cancel context.CancelFunc){
		"cancelled by a promotion": func(cancel context.CancelFunc) { cancel() },
		"timeout":                  func(cancel context.CancelFunc) {},
	} {
		t.Run(name, func(t *testing.T) {
			pidFile := fakePkg(t)
			ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
			defer cancel()
			done := make(chan struct{})
			go func() { pkgInstall(ctx, os.Environ(), []string{"os-example"}); close(done) }()
			time.Sleep(300 * time.Millisecond)
			if len(alivePIDs(t, pidFile)) < 3 {
				t.Skip("the fake pkg did not start its processes")
			}
			stop(cancel)
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("the installation did not end")
			}
			time.Sleep(200 * time.Millisecond)
			if left := alivePIDs(t, pidFile); len(left) > 0 {
				for _, p := range left {
					syscall.Kill(p, syscall.SIGKILL)
				}
				t.Fatalf("processes of the installation are still running: %v", left)
			}
		})
	}
}
