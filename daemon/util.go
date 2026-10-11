package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"
)

func readFile(p string) ([]byte, error) { return os.ReadFile(p) }
func removeFile(p string)               { os.Remove(p) }

func osStat(path string) (time.Time, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

func peerHostPort(st State, port int) string { return net.JoinHostPort(st.PeerAddr, fmt.Sprint(port)) }

func peerURL(st State, path string) string { return "https://" + peerHostPort(st, st.PeerPort) + path }

// execShell runs a fixed script through /bin/sh (never with operator input).
func execShell(script string) (string, error) {
	out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput()
	return string(out), err
}
