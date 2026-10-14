//go:build !unix

package main

import "os/exec"

// ownGroup: no process groups here (development and test platforms only); the default cancellation kills the process itself.
func ownGroup(*exec.Cmd) {}
