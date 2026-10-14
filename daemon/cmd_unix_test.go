//go:build unix

package main

import (
	"errors"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// a command stopped on timeout takes its children with it (a shell that started opnsense-update or pkg must not leave them running)
func TestTimeoutKillsTheWholeProcessGroup(t *testing.T) {
	t.Cleanup(setCmdTimeout(300 * time.Millisecond))
	out, err := sh("/bin/sh", "-c", "sleep 30 & echo $!; wait")
	if !errors.Is(err, errCmdTimeout) {
		t.Fatalf("err = %v", err)
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(strings.Split(out, "\n")[0]))
	if perr != nil {
		t.Fatalf("no child pid in %q", out)
	}
	for i := 0; i < 50; i++ {
		if syscall.Kill(pid, 0) != nil {
			return // gone
		}
		time.Sleep(20 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("the child %d of the stopped command is still running", pid)
}
