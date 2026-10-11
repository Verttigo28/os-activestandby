package main

import (
	"os"
	"path/filepath"
	"runtime"
)

// writeFileDurable writes data to path so that after a power loss the file is either the old or the new
// content, never empty or half-written: write temp, fsync it, rename, fsync the directory.
func writeFileDurable(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	// never write through a pre-planted symlink: remove whatever is there, then create exclusively (O_EXCL also fails on a link)
	os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	if runtime.GOOS == "windows" { // directories cannot be fsynced there; dev/test only
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// syncFile flushes a file written by someone else (e.g. the PHP helpers) to disk.
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// writeExcl creates path exclusively (it fails if anything, including a symlink, is there already).
func writeExcl(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
