package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Fencing: before an automatic promotion because the peer went silent, run an operator supplied command (IPMI power
// off, smart PDU, ...). "Peer silent" does not prove "peer dead"; with fencing, a peer that is still alive is switched off
// before this node takes over its addresses, so a real split-brain cannot happen. A failing command cancels the promotion.

const fenceTimeout = 30 * time.Second

// validateFenceCommand accepts only an absolute path to a regular, executable file owned by root and not writable
// by group or others (the daemon runs as root and executes it).
func validateFenceCommand(path string) error {
	if path == "" {
		return nil
	}
	if !filepath.IsAbs(path) || strings.ContainsAny(path, " \t\n;|&$`") {
		return errors.New("fence command must be an absolute path without spaces or shell characters")
	}
	fi, err := os.Lstat(path) // a symlink is not accepted: what it points to could be swapped
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0 {
		return errors.New("fence command is not an executable regular file")
	}
	if fi.Mode()&0o022 != 0 {
		return errors.New("fence command is writable by group or others")
	}
	if err := checkOwnerRoot(fi); err != nil {
		return err
	}
	// and nobody but root may rename or replace it: every directory above must be root-owned and not group/world-writable
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		di, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if di.Mode()&0o022 != 0 {
			return fmt.Errorf("directory %s is writable by group or others", dir)
		}
		if err := checkOwnerRoot(di); err != nil {
			return fmt.Errorf("directory %s: %w", dir, err)
		}
		if dir == "/" || dir == "." || dir == filepath.Dir(dir) {
			break
		}
	}
	return nil
}

func (d *Daemon) runFence() error {
	if d.act != nil && d.act.fence != nil {
		return d.act.fence()
	}
	cmdPath := d.cfg.FenceCommand
	if cmdPath == "" {
		return nil
	}
	if err := validateFenceCommand(cmdPath); err != nil {
		return fmt.Errorf("fence command rejected: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), fenceTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cmdPath, "promote", d.store.Get().PeerAddr)
	ownGroup(cmd)                   // a child of the script (ipmitool, ssh) goes with it on timeout
	cmd.WaitDelay = 5 * time.Second // and does not hold the output open: the election loop waits for this command
	out, err := cmd.CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if len(msg) > 300 {
		msg = msg[len(msg)-300:]
	}
	if ctx.Err() != nil {
		return fmt.Errorf("fence command timed out after %s", fenceTimeout)
	}
	if err != nil {
		return fmt.Errorf("fence command failed: %v: %s", err, msg)
	}
	d.event("info", "fence command succeeded: %s", msg)
	return nil
}
