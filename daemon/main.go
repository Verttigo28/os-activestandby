package main

import (
	"fmt"
	"log"
	"os"
)

func main() {
	// rc, ssh and configd start us with different search paths; the tools we call live in /usr/local/{s,}bin
	os.Setenv("PATH", "/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin:/usr/local/bin")
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: hasyncd daemon | ctl <cmd> | version")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "daemon":
		log.SetFlags(log.LstdFlags | log.Lmsgprefix)
		log.Fatal(runDaemon())
	case "ctl":
		os.Exit(runCtl(os.Args[2:]))
	case "boot-guard":
		if err := bootGuard(); err != nil {
			fmt.Fprintln(os.Stderr, "boot-guard:", err)
			os.Exit(1)
		}
	case "boot-standby":
		if err := bootStandby(); err != nil {
			fmt.Fprintln(os.Stderr, "boot-standby:", err)
			os.Exit(1)
		}
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand", os.Args[1])
		os.Exit(2)
	}
}
