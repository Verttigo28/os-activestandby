package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// The GUI shows times in the time zone of the firewall (System > Settings > General), not in UTC and not in the zone of the browser.
// The times are stored in UTC (RFC 3339, so they sort and compare the same on both nodes); each one is also given converted to the
// local time of this server, with the offset that applied at that moment (summer and winter time).
//
// Go reads the zone of the machine once, at start. A zone changed in the GUI later would only show after a restart of the daemon, so
// /etc/localtime is read again whenever it changes.

const zoneFile = "/etc/localtime"

var zoneCache struct {
	mu       sync.Mutex
	mtime    time.Time
	loc      *time.Location
	override *time.Location // the tests set the zone here (never time.Local, which every goroutine reads)
}

// serverLocation is the zone of this server as /etc/localtime says now (time.Local if it cannot be read, as in the tests).
func serverLocation() *time.Location {
	zoneCache.mu.Lock()
	defer zoneCache.mu.Unlock()
	if zoneCache.override != nil {
		return zoneCache.override
	}
	fi, err := os.Stat(zoneFile)
	if err != nil {
		return time.Local
	}
	if zoneCache.loc != nil && fi.ModTime().Equal(zoneCache.mtime) {
		return zoneCache.loc
	}
	b, err := os.ReadFile(zoneFile)
	if err != nil {
		return time.Local
	}
	loc, err := time.LoadLocationFromTZData("Local", b)
	if err != nil {
		return time.Local
	}
	zoneCache.loc, zoneCache.mtime = loc, fi.ModTime()
	return loc
}

// localStamp converts an RFC 3339 time to "2006-01-02 15:04:05" in the zone of this server ("" if it cannot be read).
func localStamp(rfc string) string {
	t, err := time.Parse(time.RFC3339Nano, rfc)
	if err != nil {
		return ""
	}
	return t.In(serverLocation()).Format("2006-01-02 15:04:05")
}

// serverZone describes the zone of this server for the GUI, e.g. "CEST, UTC+02:00".
func serverZone() string {
	name, off := time.Now().In(serverLocation()).Zone()
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	return fmt.Sprintf("%s, UTC%s%02d:%02d", name, sign, off/3600, off%3600/60)
}

// setZone makes serverLocation return loc (nil: the zone of the machine again). For the tests.
func setZone(loc *time.Location) {
	zoneCache.mu.Lock()
	zoneCache.override = loc
	zoneCache.mu.Unlock()
}
