package main

import (
	"testing"
	"time"
	_ "time/tzdata" // the zone database inside the test binary: the test does not depend on the machine that runs it
)

// times are shown in the zone of the firewall, with the offset of their own date (summer and winter time)
func TestTimesInTheZoneOfTheFirewall(t *testing.T) {
	ams, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Local
	time.Local = ams
	t.Cleanup(func() { time.Local = old })
	for utc, want := range map[string]string{
		"2026-10-14T05:58:23Z":           "2026-10-14 07:58:23", // summer time, UTC+2
		"2026-12-01T05:58:23Z":           "2026-12-01 06:58:23", // winter time, UTC+1
		"2026-10-14T05:58:23.123456789Z": "2026-10-14 07:58:23",
		"not a time":                     "",
	} {
		if got := localStamp(utc); got != want {
			t.Errorf("%s: got %q, want %q", utc, got, want)
		}
	}
	if z := serverZone(); z != "CEST, UTC+02:00" && z != "CET, UTC+01:00" {
		t.Errorf("zone %q", z)
	}
	ev := &Events{list: []Event{{Time: "2026-10-14T05:58:23Z", Kind: "info", Msg: "x"}}}
	if r := ev.Recent(1); r[0].Local != "2026-10-14 07:58:23" || ev.list[0].Local != "" {
		t.Fatalf("the local time is added when read, never stored: %+v / %+v", r[0], ev.list[0])
	}
}
