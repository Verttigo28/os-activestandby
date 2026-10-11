package main

import (
	"log"
	"os/exec"
	"sync"
)

// pfsync: firewall state replication over the HA link (unicast to the peer, independent of CARP).
// Configured here, not through config.xml: the sync peer address differs per node, so it must be node-local,
// and it must follow pairing state exactly. pf on the standby imports the states even though its data
// interfaces are down, so established connections survive a failover.

var pfsyncMu sync.Mutex

func (d *Daemon) pfsyncSetup() {
	pfsyncMu.Lock()
	defer pfsyncMu.Unlock()
	st := d.store.Get()
	if st.PeerFP == "" || st.PeerAddr == "" || d.cfg.HADevice == "" {
		d.pfsyncTeardownLocked()
		return
	}
	out, err := exec.Command("ifconfig", "pfsync0", "syncdev", d.cfg.HADevice, "syncpeer", st.PeerAddr, "maxupd", "128", "up").CombinedOutput()
	if err != nil {
		log.Printf("pfsync: setup failed: %v: %s", err, out)
		return
	}
	log.Printf("pfsync: syncing states with %s over %s", st.PeerAddr, d.cfg.HADevice)
}

func (d *Daemon) pfsyncTeardownLocked() {
	if out, err := exec.Command("ifconfig", "pfsync0", "-syncdev", "-syncpeer", "down").CombinedOutput(); err != nil {
		log.Printf("pfsync: teardown: %v: %s", err, out)
	}
}

func (d *Daemon) pfsyncTeardown() {
	pfsyncMu.Lock()
	defer pfsyncMu.Unlock()
	d.pfsyncTeardownLocked()
}
