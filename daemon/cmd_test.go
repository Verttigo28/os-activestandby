package main

import (
	"errors"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// a command that hangs is stopped and reported, instead of holding its caller (and the operations lock) for ever
func TestHangingCommandIsStopped(t *testing.T) {
	if runtime.GOOS == "js" || runtime.GOOS == "windows" {
		t.Skip("no external commands here (runs in the CI)")
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep command")
	}
	t.Cleanup(setCmdTimeout(200 * time.Millisecond))
	t0 := time.Now()
	_, err := sh("sleep", "30")
	if !errors.Is(err, errCmdTimeout) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(t0) > 10*time.Second {
		t.Fatalf("the caller waited %s", time.Since(t0))
	}
	if out, err := sh("sleep", "0"); err != nil {
		t.Fatalf("a short command must still work: %v %s", err, out)
	}
}
