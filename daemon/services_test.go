package main

import (
	"reflect"
	"testing"
)

func TestPluginTemplates(t *testing.T) {
	got := pluginTemplates([]string{"ActiveStandby", "Filter", "Lldpd", "Macros", "Netsnmp", "NodeExporter", "IPsec", "bad name", "-x"})
	want := []string{"OPNsense/Lldpd", "OPNsense/Netsnmp", "OPNsense/NodeExporter"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestStoppedServices(t *testing.T) {
	// shape of `pluginctl -S`: status is a string, an array for a few services, absent for others
	in := `[
	 {"name":"configd","locked":true,"status":"configd is not running."},
	 {"name":"cron","status":"cron is running as pid 25766."},
	 {"name":"lldpd","status":"lldpd is not running."},
	 {"name":"node_exporter","status":"node_exporter is not running."},
	 {"name":"snmpd","status":"snmpd is not running."},
	 {"name":"strongswan","status":"strongswan is not running."},
	 {"name":"ntpd","status":"ntpd is not running."},
	 {"name":"sysctl","status":["a","b"]},
	 {"name":"bad name; rm","status":"x is not running."},
	 {"name":"nostatus"}
	]`
	got := stoppedServices(in)
	want := []string{"lldpd", "node_exporter", "snmpd"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if s := stoppedServices("not json"); s != nil {
		t.Fatalf("garbage must give nothing, got %v", s)
	}
}
