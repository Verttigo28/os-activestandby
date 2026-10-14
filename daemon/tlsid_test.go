package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Reported by the audit of ba21e4d: a damaged identity was replaced. The identity is what the peer pins: it is kept twice, a
// damaged copy is restored from the other, damaged files are never overwritten, and a replacement is reported to the caller.
func TestIdentityIsPreserved(t *testing.T) {
	fixedAside(t)
	dir := t.TempDir()
	first, replaced, err := loadIdentity(dir)
	if err != nil || replaced {
		t.Fatalf("first start: %v replaced=%v", err, replaced)
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
