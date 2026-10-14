package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Reported by the audit of ba21e4d: a damaged identity was replaced. The identity is what the peer pins: it is kept twice, a
// damaged copy is restored from the other, damaged files are never overwritten, and a replacement is reported to the caller.
func TestIdentityIsPreserved(t *testing.T) {
	fixedAside(t)
	dir := t.TempDir()
	first, replaced, err := loadIdentity(dir)
	if err != nil || !replaced {
		t.Fatalf("first start: a new identity is reported as such (the caller ignores it on a node that is not paired): %v replaced=%v", err, replaced)
	}
	fp := fingerprint(first.Certificate[0])
	read := func() string {
		c, replaced, err := loadIdentity(dir)
		if err != nil || replaced {
			t.Fatalf("err %v replaced %v", err, replaced)
		}
		return fingerprint(c.Certificate[0])
	}
	if read() != fp {
		t.Fatal("not stable across starts")
	}
	// the key is torn (power loss during a write)
	os.WriteFile(filepath.Join(dir, "key.pem"), []byte("-----BEGIN EC PRI"), 0o600)
	if read() != fp {
		t.Fatal("a damaged key must be restored from the backup copy, the identity kept")
	}
	if !kept(dir, "key.pem") {
		t.Fatal("the damaged files must be kept aside")
	}
	// the main copy is gone
	os.Remove(filepath.Join(dir, "cert.pem"))
	os.Remove(filepath.Join(dir, "key.pem"))
	if read() != fp {
		t.Fatal("restored from the backup copy")
	}
	// the backup copy is damaged, the main copy is fine: the backup is rewritten
	os.WriteFile(filepath.Join(dir, "cert.pem.bak"), []byte("x"), 0o600)
	if read() != fp {
		t.Fatal("changed")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "cert.pem.bak")); !strings.Contains(string(b), "BEGIN CERTIFICATE") {
		t.Fatal("the backup copy must be repaired")
	}
}

func TestIdentityReplacedOnlyWhenBothCopiesAreLost(t *testing.T) {
	fixedAside(t)
	dir := t.TempDir()
	first, _, _ := loadIdentity(dir)
	for _, f := range []string{"cert.pem", "key.pem", "cert.pem.bak", "key.pem.bak"} {
		os.WriteFile(filepath.Join(dir, f), []byte("garbage"), 0o600)
	}
	c, replaced, err := loadIdentity(dir)
	if err != nil || !replaced {
		t.Fatalf("both copies damaged: a new identity, reported as replaced (err %v replaced %v)", err, replaced)
	}
	if fingerprint(c.Certificate[0]) == fingerprint(first.Certificate[0]) {
		t.Fatal("impossible: same identity")
	}
	if !kept(dir, "cert.pem") {
		t.Fatal("the damaged identity must be kept aside, not overwritten")
	}
}

// kept tells whether the damaged copy of name was kept aside (the stamp is fixed by fixedAside)
func kept(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name+".damaged-T"))
	return err == nil
}

func fixedAside(t *testing.T) {
	old := asideStamp
	asideStamp = func() string { return "T" }
	t.Cleanup(func() { asideStamp = old })
}

// every single failure of the identity files is repaired from the other copy, and the identity (what the peer pins) never changes
func TestIdentityRepairedFromEverySingleFailure(t *testing.T) {
	fixedAside(t)
	for name, damage := range map[string]func(dir string){
		"key missing":           func(d string) { os.Remove(filepath.Join(d, "key.pem")) },
		"certificate missing":   func(d string) { os.Remove(filepath.Join(d, "cert.pem")) },
		"key empty":             func(d string) { os.WriteFile(filepath.Join(d, "key.pem"), nil, 0o600) },
		"certificate truncated": func(d string) { os.WriteFile(filepath.Join(d, "cert.pem"), []byte("-----BEGIN CERTIF"), 0o600) },
		"key of another node": func(d string) {
			other := t.TempDir()
			loadIdentity(other)
			b, _ := os.ReadFile(filepath.Join(other, "key.pem"))
			os.WriteFile(filepath.Join(d, "key.pem"), b, 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			first, _, _ := loadIdentity(dir)
			want := fingerprint(first.Certificate[0])
			damage(dir)
			c, replaced, err := loadIdentity(dir)
			if err != nil || replaced {
				t.Fatalf("err %v replaced %v: one damaged copy must be repaired from the other", err, replaced)
			}
			if fingerprint(c.Certificate[0]) != want {
				t.Fatal("the identity changed: the peer would refuse this node")
			}
			// and the repair holds: the next start loads it without help
			c2, replaced, err := loadIdentity(dir)
			if err != nil || replaced || fingerprint(c2.Certificate[0]) != want {
				t.Fatal("not repaired for good")
			}
			if fi, err := os.Stat(filepath.Join(dir, "key.pem")); err != nil || (runtime.GOOS != "js" && runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0) { // permission bits mean nothing under the wasm runner
				t.Fatalf("the restored key must stay private: %v %v", fi, err)
			}
		})
	}
}

// a node whose identity files AND backups are gone starts a new identity and says so (a paired node then goes to standby)
func TestIdentityLostEntirelyIsReported(t *testing.T) {
	dir := t.TempDir()
	loadIdentity(dir)
	for _, f := range []string{"cert.pem", "key.pem", "cert.pem.bak", "key.pem.bak"} {
		os.Remove(filepath.Join(dir, f))
	}
	if _, replaced, err := loadIdentity(dir); err != nil || !replaced {
		t.Fatalf("err %v replaced %v", err, replaced)
	}
}
