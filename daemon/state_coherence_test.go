package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A state file that is valid JSON with known role and state values, but that no version of this software writes (an edited or damaged
// file): it must not be believed. A node that reads one starts as a silent standby, like for any other unreadable state.
func TestIncoherentStatesAreRefused(t *testing.T) {
	bad := map[string]State{
		"standalone but active":     {Role: RoleStandalone, State: StateActive},
		"standalone but standby":    {Role: RoleStandalone, State: StateStandby},
		"paired without an address": {Role: RoleMaster, State: StateActive, PeerFP: "fp", PeerPort: 7690},
		"paired, address not an IP": {Role: RoleMaster, State: StateActive, PeerFP: "fp", PeerAddr: "peer.example", PeerPort: 7690},
		"paired, port zero":         {Role: RoleStandby, State: StateStandby, PeerFP: "fp", PeerAddr: "10.0.0.2"},
		"paired, port too large":    {Role: RoleStandby, State: StateStandby, PeerFP: "fp", PeerAddr: "10.0.0.2", PeerPort: 70000},
		"paired but no state":       {Role: RoleMaster, PeerFP: "fp", PeerAddr: "10.0.0.2", PeerPort: 7690},
	}
	for name, st := range bad {
		if err := st.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	good := map[string]State{
		"standalone":             {Role: RoleStandalone},
		"paired active":          {Role: RoleMaster, State: StateActive, PeerFP: "fp", PeerAddr: "10.0.0.2", PeerPort: 7690},
		"paired standby":         {Role: RoleStandby, State: StateStandby, PeerFP: "fp", PeerAddr: "10.0.0.1", PeerPort: 7690},
		"unreadable-state start": {Role: RoleStandby, State: StateStandby, Corrupt: true},
	}
	for name, st := range good {
		if err := st.validate(); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

func TestAnIncoherentStateFileStartsAsASilentStandby(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HASYNC_DIR", dir)
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"role":"master","state":"active","peer_fp":"fp","peer_addr":"not-an-ip","peer_port":7690}`), 0o600)
	st, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	if g := st.Get(); g.State != StateStandby || !g.Corrupt {
		t.Fatalf("an edited state file made this node %s (corrupt %v): it must start as a silent standby", g.State, g.Corrupt)
	}
}
