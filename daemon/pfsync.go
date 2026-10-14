package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// pfsync: firewall state replication over the HA link (unicast to the peer, independent of CARP).
// Configured here, not through config.xml: the sync peer address differs per node, so it must be node-local,
// and it must follow pairing state exactly. pf on the standby imports the states even though its data
// interfaces are down, so established connections survive a failover.
//
// The setup is supervised: pfsyncLoop re-reads the interface every few seconds and repairs it when it is gone or points
// somewhere else (a reconfigure of the HA interface, a late HA address, a failed first attempt). While it is not
// in place the status says so, because then the session continuity after a failover is NOT guaranteed.
// What is verified is the configuration and the link flags, not that the state tables are identical.

var pfsyncMu sync.Mutex

const pfsyncCheckEvery = 10 * time.Second

func (d *Daemon) pfsyncSetup() {
	pfsyncMu.Lock()
	defer pfsyncMu.Unlock()
	d.pfsyncApplyLocked()
}

func (d *Daemon) pfsyncApplyLocked() {
	st := d.store.Get()
	if st.PeerFP == "" || st.PeerAddr == "" || d.cfg.HADevice == "" {
		d.pfsyncTeardownLocked()
		d.pfsyncProblem.Store("")
		return
	}
	out, err := boundedCmd(true, "ifconfig", "pfsync0", "syncdev", d.cfg.HADevice, "syncpeer", st.PeerAddr, "maxupd", "128", "up")
	if err != nil {
		log.Printf("pfsync: setup failed: %v: %s", err, out)
		d.setPfsyncProblem(fmt.Sprintf("pfsync setup failed: %v: %s", err, strings.TrimSpace(string(out))))
		return
	}
	log.Printf("pfsync: syncing states with %s over %s", st.PeerAddr, d.cfg.HADevice)
	d.setPfsyncProblem("")
}

func (d *Daemon) pfsyncTeardownLocked() {
	if out, err := boundedCmd(true, "ifconfig", "pfsync0", "-syncdev", "-syncpeer", "down"); err != nil {
		log.Printf("pfsync: teardown: %v: %s", err, out)
	}
}

func (d *Daemon) pfsyncTeardown() {
	pfsyncMu.Lock()
	defer pfsyncMu.Unlock()
	d.pfsyncTeardownLocked()
	d.pfsyncProblem.Store("")
}

func (d *Daemon) setPfsyncProblem(msg string) {
	if prev, _ := d.pfsyncProblem.Load().(string); prev != msg {
		if msg != "" {
			d.event("warning", "state synchronisation (pfsync) is not working: %s. Established sessions may not survive a failover", msg)
		} else if prev != "" {
			d.event("info", "state synchronisation (pfsync) is working again")
		}
	}
	d.pfsyncProblem.Store(msg)
}

// pfsyncStatus is empty when pfsync is configured as expected, otherwise the reason it is not.
func (d *Daemon) pfsyncStatus() string {
	s, _ := d.pfsyncProblem.Load().(string)
	return s
}

// pfsyncLoop verifies the pfsync interface and repairs it.
func (d *Daemon) pfsyncLoop() {
	for range time.Tick(pfsyncCheckEvery) {
		st := d.store.Get()
		if st.PeerFP == "" || st.PeerAddr == "" || d.cfg.HADevice == "" {
			continue
		}
		out, _ := sh("ifconfig", "pfsync0")
		if reason := pfsyncProblem(out, d.cfg.HADevice, st.PeerAddr); reason != "" {
			d.setPfsyncProblem(reason)
			pfsyncMu.Lock()
			d.pfsyncApplyLocked() // clears the problem when it works
			pfsyncMu.Unlock()
		} else {
			d.setPfsyncProblem("")
		}
	}
}

// pfsyncProblem checks the output of `ifconfig pfsync0` against what this node should have configured.
func pfsyncProblem(ifconfigOut, dev, peer string) string {
	lines := strings.Split(strings.TrimSpace(ifconfigOut), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return "pfsync0 does not exist"
	}
	up := false
	if i, j := strings.Index(lines[0], "<"), strings.Index(lines[0], ">"); i >= 0 && j > i {
		for _, f := range strings.Split(lines[0][i+1:j], ",") {
			up = up || f == "UP"
		}
	}
	if !up {
		return "pfsync0 is down"
	}
	var gotDev, gotPeer string
	// FreeBSD 15 prints several settings on one line ("syncdev: em1 syncpeer: 10.0.0.2 maxupd: 128 ..."), older releases one per
	// line: look at every field
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		for i := 0; i+1 < len(f); i++ {
			switch f[i] {
			case "syncdev:":
				gotDev = f[i+1]
			case "syncpeer:":
				gotPeer = f[i+1]
			}
		}
	}
	if gotDev != dev {
		return "pfsync0 does not use " + dev + " as sync device"
	}
	if gotPeer != peer {
		return "pfsync0 does not sync with " + peer
	}
	return ""
}
