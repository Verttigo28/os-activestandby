package main

import (
	"strings"
	"testing"
)

// An LLDP agent sends its frames from the MAC of the device it listens on, which on a standby is the shared MAC (lldpd on the lab pair:
// a frame every 30 s from the standby with the shared MAC as source, and the hot standby detector shut a device down for it). A standby
// stops it, and the enforcer looks for it.
func TestAStandbyStopsTheLLDPAgent(t *testing.T) {
	found := false
	for _, c := range haStop {
		if strings.Join(c, " ") == "service lldpd onestop" {
			found = true
		}
	}
	if !found {
		t.Fatal("demotion and the standby enforcer must stop lldpd: it transmits with the shared MAC")
	}
}
