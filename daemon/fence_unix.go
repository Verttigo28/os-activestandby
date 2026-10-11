//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

func checkOwnerRoot(fi os.FileInfo) error {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != 0 {
		return errors.New("fence command is not owned by root")
	}
	return nil
}
