//go:build !windows

package main

import "syscall"

// withUmask runs f with the given umask (the socket must never exist with a permissive mode, not even for a moment).
func withUmask(mask int, f func()) {
	old := syscall.Umask(mask)
	defer syscall.Umask(old)
	f()
}

// setProcessUmask fixes the umask of the whole daemon (and of every helper it starts) for good.
func setProcessUmask(mask int) { syscall.Umask(mask) }
