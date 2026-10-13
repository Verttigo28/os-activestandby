package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Fault injection for the persistence of the role: the disk refuses (full, EIO, fsync or rename failing), the process dies between
// two steps, the files are damaged. The invariant: what the memory says is what the disk says, or the failure is returned; and what a
// restart reads is never an "active" that nobody confirmed.

// failDisk makes the durable write fail for the files whose name ends with one of the suffixes; the others are written for real.
func failDisk(t *testing.T, err error, suffixes ...string) *int {
	t.Helper()
	calls := new(int)
	old := fsWrite
	fsWrite = func(path string, data []byte, perm os.FileMode) error {
		*calls++
		for _, s := range suffixes {
			if strings.HasSuffix(path, s) {
				return err
			}
		}
		return old(path, data, perm)
	}
	t.Cleanup(func() { fsWrite = old })
	return calls
}

func diskState(t *testing.T, s *Store, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		return ""
	}
	return string(b)
}

func pairedStore(t *testing.T) *Store {
	t.Helper()
	s := storeIn(t, nil)
	if err := s.PairNow(RoleMaster, "10.0.0.2", 7690, "fp"); err != nil {
		t.Fatal(err)
	}
	return s
}

// every failure of the durable write (no space left, I/O error, fsync, rename, directory sync all end up as an error of fsWrite)
func TestUpdateThatCannotBeSavedChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"disk full", errors.New("write state.json.tmp: no space left on device")},
		{"EIO", errors.New("write state.json.tmp: input/output error")},
		{"fsync", errors.New("sync state.json.tmp: input/output error")},
		{"rename", errors.New("rename state.json.tmp state.json: input/output error")},
		{"directory sync", errors.New("sync /conf/activestandby: input/output error")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := pairedStore(t)
			before := s.Get()
			onDisk := diskState(t, s, "state.json")
			failDisk(t, tc.err, "state.json")
			err := s.Update(func(x *State) { x.State = StateStandby; x.Epoch = 99 })
			if err == nil || !strings.Contains(err.Error(), "not saved") {
				t.Fatalf("the failure must be returned, got %v", err)
			}
			if got := s.Get(); got.State != before.State || got.Epoch != before.Epoch {
				t.Fatalf("memory changed although the save failed: %+v", got)
			}
			if diskState(t, s, "state.json") != onDisk {
				t.Fatal("the file on disk changed")
			}
		})
	}
}

// UpdateSafe is for what already happened to the machine: believed in memory, the failure returned, the disk untouched
func TestUpdateSafePublishesWhenTheDiskRefuses(t *testing.T) {
	s := pairedStore(t)
	onDisk := diskState(t, s, "state.json")
	failDisk(t, errors.New("no space left on device"), "state.json")
	err := s.UpdateSafe(func(x *State) { x.State = StateStandby })
	if err == nil {
		t.Fatal("the failure to save must be returned")
	}
	if s.Get().State != StateStandby {
		t.Fatal("a node that went down must be believed to be standby")
	}
	if diskState(t, s, "state.json") != onDisk {
		t.Fatal("the file on disk changed")
	}
}

// the commit point is state.json: when only the backup copy fails, the new state is saved and published
func TestFailureOfTheBackupCopyDoesNotUndoTheCommit(t *testing.T) {
	s := pairedStore(t)
	failDisk(t, errors.New("input/output error"), "state.json.bak")
	if err := s.Update(func(x *State) { x.SyncRev = 5 }); err != nil {
		t.Fatalf("committed state reported as failed: %v", err)
	}
	if s.Get().SyncRev != 5 || !strings.Contains(diskState(t, s, "state.json"), `"sync_rev": 5`) {
		t.Fatal("the new state must be in memory and in state.json")
	}
}

func TestUpdateRefusesAnInvalidState(t *testing.T) {
	s := pairedStore(t)
	for _, f := range []func(*State){
		func(x *State) { x.Role = "primary" },
		func(x *State) { x.State = "promoting" },
	} {
		if err := s.Update(f); err == nil {
			t.Fatal("an unknown role or state must be refused")
		}
	}
	if st := s.Get(); st.Role != RoleMaster || st.State != StateActive {
		t.Fatalf("memory changed: %+v", st)
	}
}

// the real durable write: a failure leaves the old file and no half-written one
func TestDurableWriteFailsCleanly(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := writeFileDurable(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	// a directory in the place of the temporary file cannot be replaced by a file: the write fails before the rename
	os.Remove(p + ".tmp")
	if err := os.Mkdir(p+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p+".tmp", "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileDurable(p, []byte("new"), 0o600); err == nil {
		t.Fatal("the write must fail")
	}
	if b, _ := os.ReadFile(p); string(b) != "old" {
		t.Fatalf("the old content must be intact, got %q", b)
	}
	// into a directory that does not exist (the disk went away)
	if err := writeFileDurable(filepath.Join(dir, "gone", "state.json"), []byte("x"), 0o600); err == nil {
		t.Fatal("writing into a missing directory must fail")
	}
}

// ---- what a restart reads after a crash at each step ----

func TestCrashLeftoversAreIgnoredAtStart(t *testing.T) {
	good := `{"role":"master","state":"standby","peer_fp":"abc","epoch":4}`
	// crash after the temporary file was written, before the rename: the old state.json stands, the .tmp is ignored
	st := storeIn(t, map[string]string{"state.json": good, "state.json.tmp": `{"role":"master","state":"active","epoch":5}`}).Get()
	if st.State != StateStandby || st.Epoch != 4 || st.Corrupt {
		t.Fatalf("a leftover temporary file must not be read: %+v", st)
	}
}

func TestMainFileCorruptBackupValid(t *testing.T) {
	bak := `{"role":"standby","state":"standby","peer_fp":"abc","epoch":9}`
	for name, main := range map[string]string{"empty": "", "torn": `{"role":"master","stat`, "empty object": "{}", "null": "null", "unknown role": `{"role":"x"}`, "unknown state": `{"role":"master","state":"promoting"}`} {
		t.Run(name, func(t *testing.T) {
			st := storeIn(t, map[string]string{"state.json": main, "state.json.bak": bak}).Get()
			if st.Role != RoleStandby || st.Epoch != 9 || st.Corrupt {
				t.Fatalf("the backup must be used: %+v", st)
			}
		})
	}
}

func TestBothFilesCorruptFailSafe(t *testing.T) {
	for _, main := range []string{"{}", "null", "{broken"} {
		st := storeIn(t, map[string]string{"state.json": main, "state.json.bak": main}).Get()
		if st.State != StateStandby || !st.Corrupt || st.PeerFP != "" {
			t.Fatalf("%q: must be a silent unpaired standby, got %+v", main, st)
		}
	}
}

// the main file is committed, the backup is older (crash between the two writes): the main file wins
func TestStaleBackupDoesNotOverrideTheMainFile(t *testing.T) {
	st := storeIn(t, map[string]string{
		"state.json":     `{"role":"master","state":"standby","peer_fp":"abc","epoch":8}`,
		"state.json.bak": `{"role":"master","state":"active","peer_fp":"abc","epoch":7}`,
	}).Get()
	if st.State != StateStandby || st.Epoch != 8 {
		t.Fatalf("%+v", st)
	}
}

// ---- concurrency and aliasing ----

func TestConcurrentUpdatesAreNotLost(t *testing.T) {
	s := pairedStore(t)
	const n = 40
	var wg sync.WaitGroup
	var ok atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// (the wasm runner on Windows sometimes refuses a rename over a file that a scanner holds: that update is then reported as
			// failed and must not be counted; on FreeBSD every update must succeed)
			if err := s.Update(func(x *State) { x.SyncRev++ }); err == nil {
				ok.Add(1)
			} else if runtime.GOOS != "js" {
				t.Error(err)
			}
		}()
	}
	// readers never wait for the disk and never see a half-applied update
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.Get() }()
	}
	wg.Wait()
	if got := s.Get().SyncRev; got != uint64(ok.Load()) {
		t.Fatalf("lost or invented updates: memory %d, successful %d", got, ok.Load())
	}
	reopened, err := openStore()
	if err != nil || reopened.Get().SyncRev != uint64(ok.Load()) {
		t.Fatalf("what is on the disk differs: %v %+v", err, reopened.Get())
	}
}

func TestGetReturnsACopyOfThePointers(t *testing.T) {
	s := pairedStore(t)
	s.Update(func(x *State) { x.Job = &Job{ID: "1", Phase: "r1-update", Log: []string{"a"}}; x.Kit = &Kit{Peer: "p"} })
	g := s.Get()
	g.Job.Phase, g.Job.Log[0], g.Kit.Peer = "done", "changed", "other"
	again := s.Get()
	if again.Job.Phase != "r1-update" || again.Job.Log[0] != "a" || again.Kit.Peer != "p" {
		t.Fatalf("a caller changed the live state: %+v %+v", again.Job, again.Kit)
	}
}

func TestPanicInAnUpdateDoesNotBlockTheStore(t *testing.T) {
	s := pairedStore(t)
	func() {
		defer func() { recover() }()
		s.Update(func(x *State) { x.Epoch = 1000; panic("boom") })
	}()
	if s.Get().Epoch == 1000 {
		t.Fatal("a half-done update was published")
	}
	if err := s.Update(func(x *State) { x.SyncRev = 1 }); err != nil {
		t.Fatalf("the store is blocked: %v", err)
	}
}

// ---- the takeover when the disk refuses ----

func TestTakeoverStaysActiveAndLoudWhenTheStateCannotBeSaved(t *testing.T) {
	d := newHANode(t, standbyState(RoleStandby, 6))
	failDisk(t, fmt.Errorf("no space left on device"), "state.json")
	d.commitActive()
	st := d.store.Get()
	if st.State != StateActive || st.Epoch != 7 {
		t.Fatalf("the node serves traffic: it must be believed active at epoch 7, got %+v", st)
	}
	if !strings.Contains(d.degradedReason(), "could not be saved") {
		t.Fatalf("the failure must be visible in the status, got %q", d.degradedReason())
	}
}

func TestTakeoverCommitSavesWhenTheDiskWorks(t *testing.T) {
	d := newHANode(t, standbyState(RoleStandby, 6))
	d.commitActive()
	if st := d.store.Get(); st.State != StateActive || st.Epoch != 7 {
		t.Fatalf("%+v", st)
	}
	if d.degradedReason() != "" {
		t.Fatalf("unexpected degraded: %s", d.degradedReason())
	}
	if !strings.Contains(diskState(t, d.store, "state.json"), `"epoch": 7`) {
		t.Fatal("not on disk")
	}
}
