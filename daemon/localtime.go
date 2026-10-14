package main

import (
	"fmt"
	"time"
)

// The GUI shows times in the time zone of the firewall (System > Settings > General), not in UTC and not in the zone of the browser.
// The times are stored in UTC (RFC 3339, so they sort and compare the same on both nodes); each one is also given converted to the
// local time of this server, with the offset that applied at that moment (summer and winter time).

// localStamp converts an RFC 3339 time to "2006-01-02 15:04:05" in the local zone of this server ("" if it cannot be read).
func localStamp(rfc string) string {
	t, err := time.Parse(time.RFC3339Nano, rfc)
	if err != nil {
		return ""
	}
	return t.In(time.Local).Format("2006-01-02 15:04:05")
}

// serverZone describes the zone of this server for the GUI, e.g. "CEST, UTC+02:00".
func serverZone() string {
	name, off := time.Now().Zone()
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	return fmt.Sprintf("%s, UTC%s%02d:%02d", name, sign, off/3600, off%3600/60)
}
