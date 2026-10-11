package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionMirror(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	a, b := "sess_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "sess_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	os.WriteFile(filepath.Join(src, a), []byte("user|s:4:\"root\";"), 0o600)
	os.WriteFile(filepath.Join(src, b), []byte("x|i:1;"), 0o600)
	os.WriteFile(filepath.Join(src, "sess_empty00000000000000000000000000"), nil, 0o600) // anonymous, carries nothing
	os.WriteFile(filepath.Join(src, "notasession"), []byte("x"), 0o600)
	local := "sess_cccccccccccccccccccccccccccccccc"
	os.WriteFile(filepath.Join(dst, local), []byte("local login on the standby"), 0o600)

	s := newSessionSync()
	files := collectSessions(src)
	if len(files) != 2 {
		t.Fatalf("collected %d files, want 2", len(files))
	}
	if w, _ := s.applySessions(dst, files); w != 2 {
		t.Fatalf("written %d, want 2", w)
	}
	if _, err := os.Stat(filepath.Join(dst, local)); err != nil {
		t.Fatal("a session created locally on the standby must survive")
	}
	// log-out on the active node: its copy disappears on the standby too, the local one still stays
	os.Remove(filepath.Join(src, a))
	if _, rm := s.applySessions(dst, collectSessions(src)); rm != 1 {
		t.Fatalf("removed %d, want 1", rm)
	}
	if _, err := os.Stat(filepath.Join(dst, a)); err == nil {
		t.Fatal("logged-out session still valid on the standby")
	}
	if _, err := os.Stat(filepath.Join(dst, local)); err != nil {
		t.Fatal("local session was removed")
	}
}

func TestSessionRejectsBadNames(t *testing.T) {
	dst := t.TempDir()
	s := newSessionSync()
	bad := []sessionFile{
		{Name: "../../etc/passwd", Data: []byte("x")},
		{Name: "sess_../../../conf/config.xml", Data: []byte("x")},
		{Name: "sess_short", Data: []byte("x")},
		{Name: "sess_dddddddddddddddddddddddddddddddd", Data: make([]byte, maxSessionSize+1)},
	}
	if w, _ := s.applySessions(dst, bad); w != 0 {
		t.Fatalf("wrote %d files from bad input", w)
	}
	if ents, _ := os.ReadDir(dst); len(ents) != 0 {
		t.Fatalf("directory not empty: %v", ents)
	}
}
