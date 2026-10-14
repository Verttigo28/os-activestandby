package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Every external command of the daemon is bounded in time. A command that hangs (a configd action stuck behind a lock, a PHP helper
// waiting on a file, a driver that does not answer) would otherwise hold whatever the caller holds: during a promotion or a demotion
// that is the operations lock, so every later role change would wait for ever. The bound is generous for the ordinary commands (the
// slowest legitimate ones, a filter reload with a large ruleset or the creation of many VLANs, take seconds). The long operations
// say so explicitly: the firmware update (updateTimeout), the package installation and the fencing command have their own limits.
//
// A command runs in its own process group, and on timeout the whole group is killed: a shell or a helper that started children
// (php, pkg, opnsense-update) must not leave them running on their own after its caller gave up (cmd_unix.go).
var cmdTimeoutV = struct {
	mu sync.RWMutex
	d  time.Duration
}{d: 2 * time.Minute}

func cmdTimeout() time.Duration {
	cmdTimeoutV.mu.RLock()
	defer cmdTimeoutV.mu.RUnlock()
	return cmdTimeoutV.d
}

// setCmdTimeout changes the bound (the tests shorten it) and returns the function that restores it.
func setCmdTimeout(d time.Duration) (restore func()) {
	cmdTimeoutV.mu.Lock()
	old := cmdTimeoutV.d
	cmdTimeoutV.d = d
	cmdTimeoutV.mu.Unlock()
	return func() {
		cmdTimeoutV.mu.Lock()
		cmdTimeoutV.d = old
		cmdTimeoutV.mu.Unlock()
	}
}

// updateTimeout bounds the software update of a standby during the guided upgrade (opnsense-update and pkg upgrade through the
// proxy of the active node can take long on a slow link).
var updateTimeout = 90 * time.Minute

var errCmdTimeout = errors.New("did not finish in time and was stopped")

// boundedCmd runs name with args within cmdTimeout; combined: stdout and stderr together (like CombinedOutput), otherwise stdout only.
func boundedCmd(combined bool, name string, args ...string) ([]byte, error) {
	return boundedCmdFor(cmdTimeout(), combined, name, args...)
}

func boundedCmdFor(limit time.Duration, combined bool, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...)
	ownGroup(c)                   // the children go with it on timeout
	c.WaitDelay = 5 * time.Second // a child that keeps the output open must not hold the caller either
	var out []byte
	var err error
	if combined {
		out, err = c.CombinedOutput()
	} else {
		out, err = c.Output()
	}
	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("%s %s: %w (after %s)", name, strings.Join(args, " "), errCmdTimeout, limit)
	}
	return out, err
}
