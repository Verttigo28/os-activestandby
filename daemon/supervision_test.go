package main

import (
	"reflect"
	"testing"
)

func TestPackageDiffReportsMissingDifferentAndExtra(t *testing.T) {
	want := map[string]string{
		"opnsense": "25.1", "os-activestandby": "0.7.0", // checked elsewhere
		"os-haproxy": "4.1", "os-wireguard-kmod": "2.0", "os-acme-client": "4.8",
		"-bad-name": "1", // from the peer: never trusted
	}
	have := map[string]string{
		"opnsense": "25.1", "os-activestandby": "0.6.0",
		"os-haproxy": "4.0", "os-acme-client": "4.8", "os-theme-x": "1.0",
	}
	diff, extra := packageDiff(want, have)
	if !reflect.DeepEqual(diff, []string{"os-haproxy (active 4.1, here 4.0)", "os-wireguard-kmod (missing here, active 2.0)"}) {
		t.Fatalf("diff = %q", diff)
	}
	if !reflect.DeepEqual(extra, []string{"os-theme-x"}) {
		t.Fatalf("extra = %q", extra)
	}
}

func TestPackageDiffConformIsEmpty(t *testing.T) {
	m := map[string]string{"opnsense": "25.1", "os-haproxy": "4.1"}
	if d, e := packageDiff(m, m); len(d) != 0 || len(e) != 0 {
		t.Fatalf("%q %q", d, e)
	}
}

func TestPackageReportIsNotAFalseAllClear(t *testing.T) {
	d := newHANode(t, standbyState(RoleStandby, 1))
	d.setPkgReport(map[string]string{"os-haproxy": "4.1"}, map[string]string{"os-haproxy": "4.0"})
	if d.packageReport() == "" {
		t.Fatal("a version difference must be reported even though the sync itself succeeded")
	}
	d.setPkgReport(map[string]string{"os-haproxy": "4.1"}, map[string]string{"os-haproxy": "4.1"})
	if d.packageReport() != "" {
		t.Fatal("report not cleared after the nodes converged")
	}
}

const pfsyncGood = "pfsync0: flags=41<UP,RUNNING> metric 0 mtu 1500\n\tsyncdev: em1\n\tsyncpeer: 10.0.0.2\n\tmaxupd: 128 defer: off\n\tgroups: pfsync\n"

func TestPfsyncProblemDetection(t *testing.T) {
	cases := []struct {
		name, out, want string
	}{
		{"configured as expected", pfsyncGood, ""},
		{"interface missing", "", "pfsync0 does not exist"},
		{"down", "pfsync0: flags=0<> metric 0 mtu 1500\n\tsyncdev: em1\n\tsyncpeer: 10.0.0.2\n", "pfsync0 is down"},
		{"no sync device", "pfsync0: flags=41<UP,RUNNING> metric 0 mtu 1500\n\tsyncpeer: 10.0.0.2\n", "pfsync0 does not use em1 as sync device"},
		{"other device with the same prefix", "pfsync0: flags=41<UP,RUNNING> metric 0 mtu 1500\n\tsyncdev: em10\n\tsyncpeer: 10.0.0.2\n", "pfsync0 does not use em1 as sync device"},
		{"multicast (no peer)", "pfsync0: flags=41<UP,RUNNING> metric 0 mtu 1500\n\tsyncdev: em1\n", "pfsync0 does not sync with 10.0.0.2"},
		{"wrong peer", "pfsync0: flags=41<UP,RUNNING> metric 0 mtu 1500\n\tsyncdev: em1\n\tsyncpeer: 10.0.0.22\n", "pfsync0 does not sync with 10.0.0.2"},
	}
	for _, c := range cases {
		if got := pfsyncProblem(c.out, "em1", "10.0.0.2"); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPfsyncProblemIsReportedOnceAndCleared(t *testing.T) {
	d := newHANode(t, standbyState(RoleStandby, 1))
	d.setPfsyncProblem("pfsync0 is down")
	d.setPfsyncProblem("pfsync0 is down")
	if d.pfsyncStatus() != "pfsync0 is down" {
		t.Fatalf("%q", d.pfsyncStatus())
	}
	d.setPfsyncProblem("")
	if d.pfsyncStatus() != "" {
		t.Fatal("not cleared")
	}
}
