package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Role of this node. The role lives in /conf/activestandby/state.json, never in config.xml,
// so it is node-local by construction and cannot be overwritten by config sync.
const (
	RoleStandalone = "standalone"
	RoleMaster     = "master"
	RoleStandby    = "standby"
)

type State struct {
	Role       string `json:"role"`
	PeerAddr   string `json:"peer_addr,omitempty"`
	PeerPort   int    `json:"peer_port,omitempty"`
	PeerFP     string `json:"peer_fp,omitempty"`
	PairedAt   string `json:"paired_at,omitempty"`
	State      string `json:"state,omitempty"` // active|standby (only while paired)
	SyncRev    uint64 `json:"sync_rev"`
	AckedRev   uint64 `json:"acked_rev"`
	AckedSHA   string `json:"acked_sha,omitempty"`
	AppliedRev uint64 `json:"applied_rev"`
	AppliedSHA string `json:"applied_sha,omitempty"`
	SyncError  string `json:"sync_error,omitempty"`
	SyncOK     string `json:"sync_ok,omitempty"`
	PeerMgmt   string `json:"peer_mgmt,omitempty"`
	Epoch      uint64 `json:"epoch"`
	// Corrupt: the state files were unreadable but this node shows signs of having been paired. It then
	// runs as a silent standby with no peer pinned until it is re-paired. Never active.
	Corrupt bool `json:"corrupt,omitempty"`

	// Maintenance pauses automatic failover, preemption and health-driven switching (split-brain protection stays on).
	Maintenance      bool   `json:"maintenance,omitempty"`
	MaintenanceUntil string `json:"maintenance_until,omitempty"` // safety: switches itself off
	MaintAt          string `json:"maint_at,omitempty"`          // time of the last change; the newest change wins between the nodes
	// Integrity hashes of the files this daemon wrote on a standby; a mismatch means someone changed them (drift).
	ActiveSHA string `json:"active_sha,omitempty"`
	ViewSHA   string `json:"view_sha,omitempty"`
	// Job is the guided upgrade in progress, if any (see upgrade.go). Persisted so it survives reboots and failovers.
	Job *Job `json:"job,omitempty"`
}

type Store struct {
	mu   sync.Mutex
	dir  string
	data State
}

func stateDir() string {
	if d := os.Getenv("HASYNC_DIR"); d != "" {
		return d
	}
	return "/conf/activestandby"
}

func openStore() (*Store, error) {
	s := &Store{dir: stateDir()}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, err
	}
	main, bak := filepath.Join(s.dir, "state.json"), filepath.Join(s.dir, "state.json.bak")
	load := func(p string) (State, bool, error) {
		b, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			return State{}, false, nil
		}
		if err != nil {
			return State{}, true, err
		}
		var st State
		if err := json.Unmarshal(b, &st); err != nil {
			return State{}, true, err
		}
		return st, true, nil
	}
	st, exists, err := load(main)
	if err != nil {
		// torn or empty file after a crash: fall back to the second copy; if that is not usable either,
		// keep the original error so the node fails safe
		if st2, ok2, err2 := load(bak); err2 == nil && ok2 {
			st, exists, err = st2, true, nil
		}
	}
	switch {
	case err == nil && exists:
		s.data = st
	case err != nil:
		s.data = State{Role: RoleStandby, State: StateStandby, Corrupt: true}
	default:
		s.data = State{Role: RoleStandalone}
		// no state at all but pairing leftovers exist: do not assume we are standalone
		if _, e := os.Stat(activeXML(s.dir)); e == nil {
			s.data = State{Role: RoleStandby, State: StateStandby, Corrupt: true}
		}
	}
	if s.data.Role == "" {
		s.data.Role = RoleStandalone
	}
	return s, nil
}

func (s *Store) Get() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data
}

func (s *Store) Update(f func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.data)
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileDurable(filepath.Join(s.dir, "state.json"), b, 0o600); err != nil {
		return err
	}
	return writeFileDurable(filepath.Join(s.dir, "state.json.bak"), b, 0o600)
}

func (s *Store) PairNow(role, addr string, port int, fp string) error {
	return s.Update(func(st *State) {
		st.Role, st.PeerAddr, st.PeerPort, st.PeerFP = role, addr, port, fp
		st.State = StateStandby
		if role == RoleMaster {
			st.State = StateActive
		}
		st.PairedAt = time.Now().UTC().Format(time.RFC3339)
		if st.Epoch == 0 {
			st.Epoch = 1
		}
	})
}

func (s *Store) Reset() error {
	return s.Update(func(st *State) { *st = State{Role: RoleStandalone, Epoch: st.Epoch} })
}
