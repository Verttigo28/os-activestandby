//go:build windows

package main

func withUmask(mask int, f func()) { f() }

func setProcessUmask(mask int) {}
