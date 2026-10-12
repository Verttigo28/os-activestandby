package main

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// reloadFilter loads the ruleset and verifies the result: a reload that "ran" but left pf disabled would let the node route
// traffic without a firewall. The reload is retried once, the check is not skippable.
func reloadFilter() error {
	var last error
	for try := 0; try < 2; try++ {
		out, err := sh("configctl", "filter", "reload")
		if err == nil && !configctlFailed(out) {
			last = nil
			break
		}
		last = fmt.Errorf("filter reload failed: %v: %s", err, out)
		time.Sleep(500 * time.Millisecond)
	}
	if last != nil {
		return last
	}
	out, err := sh("pfctl", "-si")
	if err != nil {
		return fmt.Errorf("cannot verify pf (pfctl -si: %v: %s)", err, out)
	}
	if !pfEnabled(out) {
		return errors.New("pf is not enabled after the filter reload")
	}
	return nil
}

// pfEnabled reads the "Status:" line of `pfctl -si`.
func pfEnabled(info string) bool {
	for _, l := range strings.Split(info, "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "Status:" {
			return f[1] == "Enabled"
		}
	}
	return false
}

// configctlFailed: configctl does not always signal a failed action through its exit status; its answer text does.
func configctlFailed(out string) bool {
	o := strings.ToLower(strings.TrimSpace(out))
	return strings.HasPrefix(o, "error") || strings.Contains(o, "execute error") || strings.Contains(o, "action not found")
}
