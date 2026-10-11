//go:build windows

package main

import "os"

// ownership cannot be checked on Windows (development builds only)
func checkOwnerRoot(os.FileInfo) error { return nil }
