package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
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
	// LeaderEpoch is the highest leadership epoch of the configuration pushes accepted from the peer (see checkConfigPush); 0 after pairing.
	LeaderEpoch uint64 `json:"leader_epoch,omitempty"`
	// Corrupt: the state files were unreadable but this node shows signs of having been paired. It then
	// runs as a silent standby with no peer pinned until it is re-paired. Never active.
	Corrupt bool `json:"corrupt,omitempty"`

	// Maintenance pauses automatic failover, preemption and health-driven switching (split-brain protection stays on).
	Maintenance      bool   `json:"maintenance,omitempty"`
	MaintenanceUntil string `json:"maintenance_until,omitempty"` // safety: switches itself off
	MaintAt          string `json:"maint_at,omitempty"`          // time of the last change; the newest change wins between the nodes
	// UnlockedUntil: the configuration of this standby may be written until then (an update, see setUnlocked)
	UnlockedUntil string `json:"unlocked_until,omitempty"`
	// Integrity hashes of the files this daemon wrote on a standby; a mismatch means someone changed them (drift).
	ActiveSHA string `json:"active_sha,omitempty"`
	// SettingsSHA fingerprints the shared plugin settings last applied here; a change re-renders the daemon config and restarts it.
	SettingsSHA string `json:"settings_sha,omitempty"`
	ViewSHA     string `json:"view_sha,omitempty"`
	// Job is the guided upgrade in progress, if any (see upgrade.go). Persisted so it survives reboots and failovers.
	Job *Job `json:"job,omitempty"`
	// Kit: a secondary kit was handed out and this (standalone) node pairs with that peer as soon as it answers.
	Kit *Kit `json:"kit,omitempty"`
}

type Store struct {
	mu   sync.Mutex   // serialises writers (it is held across the fsync of a save)
	rd   sync.RWMutex // protects data only: readers never wait for the disk, so a slow disk cannot silence the heartbeat
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
		// valid JSON is not a valid state: "{}" or "null" must not turn a paired node into a standalone one
		if err := st.validate(); err != nil || st.Role == "" {
			return State{}, true, fmt.Errorf("%s: not a valid state (%v)", p, err)
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

// clone copies the pointers (Job, Kit) and the slice inside them, so that a copy of the state never shares memory with the live one.
func (s State) clone() State {
	if s.Job != nil {
		j := *s.Job
		j.Log = append([]string(nil), s.Job.Log...)
		s.Job = &j
	}
	if s.Kit != nil {
		k := *s.Kit
		s.Kit = &k
	}
	return s
}

// validate is the check of a state before it is saved and after it was loaded: a role and a state that this software knows.
func (s State) validate() error {
	switch s.Role {
	case RoleStandalone, RoleMaster, RoleStandby:
	default:
		return fmt.Errorf("unknown role %q", s.Role)
	}
	switch s.State {
	case "", StateActive, StateStandby:
	default:
		return fmt.Errorf("unknown state %q", s.State)
	}
	// coherence: combinations that no version of this software writes (an edited or damaged file)
	if s.Role == RoleStandalone && s.State != "" {
		return fmt.Errorf("a standalone node cannot be %s", s.State)
	}
	if s.PeerFP != "" {
		if ip := net.ParseIP(s.PeerAddr); ip == nil {
			return fmt.Errorf("paired with a peer whose address %q is not an IP address", s.PeerAddr)
		}
		if s.PeerPort < 1 || s.PeerPort > 65535 {
			return fmt.Errorf("paired with a peer whose port is %d", s.PeerPort)
		}
		if s.State == "" {
			return errors.New("paired but neither active nor standby")
		}
	}
	return nil
}

// Get returns a copy: the caller can change it, or keep it, without touching or racing with the live state.
func (s *Store) Get() State {
	s.rd.RLock()
	defer s.rd.RUnlock()
	return s.data.clone()
}

// fsWrite is the durable write of the state files (replaceable by the tests, see setFsWrite).
var fsHook struct {
	mu sync.RWMutex
	f  func(string, []byte, os.FileMode) error // nil: writeFileDurable
}

func fsWrite(path string, data []byte, perm os.FileMode) error {
	fsHook.mu.RLock()
	f := fsHook.f
	fsHook.mu.RUnlock()
	if f != nil {
		return f(path, data, perm)
	}
	return writeFileDurable(path, data, perm)
}

// setFsWrite replaces the durable write (the tests make the disk fail) and returns the function that restores it.
func setFsWrite(f func(string, []byte, os.FileMode) error) (restore func()) {
	fsHook.mu.Lock()
	old := fsHook.f
	fsHook.f = f
	fsHook.mu.Unlock()
	return func() {
		fsHook.mu.Lock()
		fsHook.f = old
		fsHook.mu.Unlock()
	}
}

// Update applies f to a copy of the state, validates and saves it, and only then publishes it to the readers.
// The commit point is the rename of state.json (written and flushed first, then the .bak copy). If anything before it fails, nothing
// changed: the memory still shows the previous state, which is the one on the disk, and the error is returned.
func (s *Store) Update(f func(*State)) error { return s.commit(f, false) }

// UpdateSafe is for the changes that follow something that already happened to the machine and that must be believed even if the
// disk refuses them (a node that has gone down to standby, a node that has taken over): the new state is published, the failure to
// save it is returned and logged. After a restart the node reads the old file, which boots as a silent standby.
func (s *Store) UpdateSafe(f func(*State)) error { return s.commit(f, true) }

func (s *Store) commit(f func(*State), publishAnyway bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.Get()
	f(&next)
	if next.Role == "" {
		next.Role = RoleStandalone
	}
	if err := next.validate(); err != nil {
		return fmt.Errorf("refused: %w", err)
	}
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("state not saved: %w", err)
	}
	publish := func() {
		s.rd.Lock()
		s.data = next
		s.rd.Unlock()
	}
	if err := fsWrite(filepath.Join(s.dir, "state.json"), b, 0o600); err != nil {
		log.Printf("STATE NOT SAVED: %v", err)
		if publishAnyway {
			publish()
		}
		return fmt.Errorf("state not saved: %w", err)
	}
	publish() // committed: the main file holds the new state
	if err := fsWrite(filepath.Join(s.dir, "state.json.bak"), b, 0o600); err != nil {
		log.Printf("state backup copy not written (the state itself is saved): %v", err)
	}
	return nil
}

func (s *Store) PairNow(role, addr string, port int, fp string) error {
	return s.Update(func(st *State) {
		st.Role, st.PeerAddr, st.PeerPort, st.PeerFP = role, addr, port, fp
		st.State = StateStandby
		if role == RoleMaster {
			st.State = StateActive
		}
		st.PairedAt = time.Now().UTC().Format(time.RFC3339)
		st.LeaderEpoch = 0 // a new pair: what was accepted from a former peer does not count
		if st.Epoch == 0 {
			st.Epoch = 1
		}
	})
}

func (s *Store) Reset() error {
	return s.Update(func(st *State) { *st = State{Role: RoleStandalone, Epoch: st.Epoch} })
}
