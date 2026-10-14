package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Every external command of the daemon is bounded in time. A command that hangs (a configd action stuck behind a lock, a PHP helper
// waiting on a file, a driver that does not answer) would otherwise hold whatever the caller holds: during a promotion or a demotion
// that is the operations lock, so every later role change would wait for ever. The bound is generous (the slowest legitimate
// commands, a filter reload with a large ruleset or the creation of many VLANs, take seconds); the long operations (package
// installation, firmware update, the fencing command) have their own limits and do not go through here.
var cmdTimeout = 2 * time.Minute

var errCmdTimeout = errors.New("did not finish in time and was stopped")

// boundedCmd runs name with args; combined: stdout and stderr together (like CombinedOutput), otherwise stdout only.
func boundedCmd(combined bool, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...)
	c.WaitDelay = 5 * time.Second // a child that keeps the output open must not hold the caller either
	var out []byte
	var err error
	if combined {
		out, err = c.CombinedOutput()
	} else {
		out, err = c.Output()
	}
	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("%s %s: %w (after %s)", name, strings.Join(args, " "), errCmdTimeout, cmdTimeout)
	}
	return out, err
}
