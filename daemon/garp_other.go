//go:build !freebsd

package main

import "log"

func announce(dev string) { log.Printf("garp: not supported on this OS (%s)", dev) }
