package main

import (
	"os"
	"path/filepath"
	"testing"
)

func storeIn(t *testing.T, files map[string]string) *Store {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HASYNC_DIR", dir)
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFreshNodeIsStandalone(t *testing.T) {
	if st := storeIn(t, nil).Get(); st.Role != RoleStandalone || st.Corrupt {
		t.Fatalf("fresh node: %+v", st)
	}
}

// the exact crash seen in the lab: state.json truncated to 0 bytes by a power loss
func TestEmptyStateWithPairingLeftoversFailsSafe(t *testing.T) {
	s := storeIn(t, map[string]string{"state.json": "", "active.xml": "<opnsense/>"})
	st := s.Get()
	if st.Role == RoleStandalone || st.State != StateStandby || !st.Corrupt || st.PeerFP != "" {
		t.Fatalf("must be a silent, unpaired standby, got %+v", st)
	}
}

func TestEmptyStateWithoutLeftoversStillFailsSafe(t *testing.T) {
	st := storeIn(t, map[string]string{"state.json": "{not json"}).Get()
	if st.State != StateStandby || !st.Corrupt {
		t.Fatalf("garbage state must fail safe, got %+v", st)
	}
}

func TestBackupCopyRescuesTornState(t *testing.T) {
	good := `{"role":"master","state":"active","peer_fp":"abc","peer_addr":"10.0.0.2","peer_port":7690,"epoch":7}`
	st := storeIn(t, map[string]string{"state.json": "", "state.json.bak": good}).Get()
	if st.Role != RoleMaster || st.PeerFP != "abc" || st.Epoch != 7 || st.Corrupt {
		t.Fatalf("backup not used: %+v", st)
	}
}

func TestUpdateWritesBothCopies(t *testing.T) {
	s := storeIn(t, nil)
	if err := s.Update(func(x *State) { x.Role, x.Epoch = RoleMaster, 3 }); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"state.json", "state.json.bak"} {
		if b, err := os.ReadFile(filepath.Join(s.dir, n)); err != nil || len(b) == 0 {
			t.Fatalf("%s missing or empty: %v", n, err)
		}
	}
}
