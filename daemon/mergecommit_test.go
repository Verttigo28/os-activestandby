package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func cfgXML(host, marker string) string {
	return "<?xml version=\"1.0\"?>\n<opnsense><system><hostname>" + host + "</hostname></system><m>" + marker + "</m>" +
		strings.Repeat("<x>filler</x>", 30) + "</opnsense>\n"
}

// A promotion carries the settings of this node (HA and management interface, host name) from config.xml into the saved
// configuration of the active node. Asked by the review of promote.go: every step is checked, and on any failure the promotion stops
// BEFORE anything is changed, with the saved configuration exactly as it was.
type mergeEnv struct {
	d      *Daemon
	full   string
	before string
}

func newMergeEnv(t *testing.T, merge func(args []string) (string, error)) *mergeEnv {
	t.Helper()
	d := newHANode(t, standbyState(RoleStandby, 6))
	d.health, d.drift, d.leases = newHealthState(), &driftState{}, &leaseState{}
	cfg := filepath.Join(d.store.dir, "config.xml")
	t.Setenv("HASYNC_CONFIG", cfg)
	os.WriteFile(cfg, []byte(cfgXML("standby-b", "standby view")), 0o600)
	e := &mergeEnv{d: d, full: activeXML(d.store.dir), before: cfgXML("active-a", "the active node's configuration")}
	os.WriteFile(e.full, []byte(e.before), 0o600)
	d.act.sh = func(name string, args ...string) (string, error) {
		if name == mergeScript {
			return merge(args)
		}
		return "", nil
	}
	return e
}

func (e *mergeEnv) untouched(t *testing.T) {
	t.Helper()
	if b, _ := os.ReadFile(e.full); string(b) != e.before {
		t.Fatal("the saved configuration of the active node changed although the commit failed")
	}
	if _, err := os.Stat(e.full + ".new"); err == nil {
		t.Fatal("a temporary file was left behind")
	}
	if recordExists(e.d) {
		t.Fatal("a transition was recorded although nothing was changed")
	}
	if e.d.store.Get().State != StateStandby {
		t.Fatal("the node must stay standby")
	}
}

func okMerge(args []string) (string, error) {
	return "", os.WriteFile(args[2], []byte(cfgXML("standby-b", "merged")), 0o644)
}

func TestMergedConfigurationReplacesTheSavedOneAtomically(t *testing.T) {
	e := newMergeEnv(t, okMerge)
	if err := e.d.commitMergedConfig(e.full); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(e.full)
	if !strings.Contains(string(b), "merged") || !strings.Contains(string(b), "standby-b") {
		t.Fatal("the merged configuration is not in place")
	}
	if _, err := os.Stat(e.full + ".new"); err == nil {
		t.Fatal("temporary file left")
	}
	if runtime.GOOS != "js" && runtime.GOOS != "windows" {
		if fi, _ := os.Stat(e.full); fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("the saved configuration (password hashes, keys) must be private, mode %v", fi.Mode().Perm())
		}
	}
}

func TestPromotionStopsWhenTheLocalSettingsCannotBeCarriedOver(t *testing.T) {
	for name, tc := range map[string]struct {
		merge func(args []string) (string, error)
		setup func(d *Daemon)
		want  string
	}{
		"the merge helper fails": {
			merge: func([]string) (string, error) { return "php: Parse error", errors.New("exit status 255") },
			want:  "merge of the local settings failed",
		},
		"the helper succeeds but writes nothing": {
			merge: func([]string) (string, error) { return "", nil },
			want:  "not usable",
		},
		"the helper writes a torn file": {
			merge: func(a []string) (string, error) {
				return "", os.WriteFile(a[2], []byte("<?xml version=\"1.0\"?><opnsense><system>"), 0o600)
			},
			want: "incomplete",
		},
		"the result lost the host name of this node": {
			merge: func(a []string) (string, error) {
				return "", os.WriteFile(a[2], []byte(cfgXML("active-a", "unmerged: the active node's own settings")), 0o600)
			},
			want: "host name",
		},
		"the file cannot be flushed": {
			merge: okMerge,
			setup: func(d *Daemon) { d.act.syncFile = func(string) error { return errors.New("input/output error") } },
			want:  "flushed",
		},
		"the rename fails": {
			merge: okMerge,
			setup: func(d *Daemon) {
				d.act.rename = func(string, string) error { return errors.New("read-only file system") }
			},
			want: "cannot replace",
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newMergeEnv(t, tc.merge)
			if tc.setup != nil {
				tc.setup(e.d)
			}
			err := e.d.promote("test")
			if err == nil || !strings.Contains(err.Error(), "promotion aborted") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want an abort mentioning %q", err, tc.want)
			}
			e.untouched(t)
		})
	}
}

// the stale temporary file of a crashed attempt does not matter
func TestStaleTemporaryFileOfACrashedAttemptIsIgnored(t *testing.T) {
	e := newMergeEnv(t, okMerge)
	os.WriteFile(e.full+".new", []byte("torn"), 0o600)
	if err := e.d.commitMergedConfig(e.full); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(e.full); !strings.Contains(string(b), "merged") {
		t.Fatal("not committed")
	}
}
