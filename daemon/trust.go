package main

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// The environment of the daemon can move the helper scripts (HASYNC_SCRIPTS) and the search path of the external commands
// (HASYNC_PATH). Both exist for the sandbox in lab/sandbox only. The daemon runs as root and executes what they point to, so
// they are honoured only for directories that root owns and nobody else can write to; anything else is ignored with an error in
// the log and the built-in locations are used.

const defaultPATH = "/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin:/usr/local/bin"

// trustedDir: an absolute path to a directory owned by root that group and others cannot write to.
func trustedDir(p string) error {
	if !filepath.IsAbs(p) {
		return errors.New("not an absolute path")
	}
	fi, err := os.Lstat(p) // a symlink is not accepted: its target could be swapped
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errors.New("not a directory")
	}
	if fi.Mode()&0o022 != 0 {
		return errors.New("writable by group or others")
	}
	return checkOwnerRoot(fi)
}

// envDir returns the directory named by the environment variable when it is trusted, the default otherwise.
func envDir(name, def string) string {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	if err := trustedDir(v); err != nil {
		log.Printf("ERROR: ignoring %s=%q: %v", name, v, err)
		return def
	}
	return v
}

// searchPath returns HASYNC_PATH when every directory in it is trusted, the built-in path otherwise.
func searchPath() string {
	v := os.Getenv("HASYNC_PATH")
	if v == "" {
		return defaultPATH
	}
	for _, d := range strings.Split(v, ":") {
		if err := trustedDir(d); err != nil {
			log.Printf("ERROR: ignoring HASYNC_PATH: %q: %v", d, err)
			return defaultPATH
		}
	}
	return v
}
