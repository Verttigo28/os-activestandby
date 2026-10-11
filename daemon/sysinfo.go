package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

type NIC struct {
	Name string `json:"name"`
	MAC  string `json:"mac"`    // current (possibly spoofed)
	HW   string `json:"hw_mac"` // burned-in
}

// Info is what both nodes compare before pairing. Hostname and MACs are informational only.
type Info struct {
	Hostname string            `json:"hostname"`
	Version  string            `json:"version"`
	Kernel   string            `json:"kernel"`
	Arch     string            `json:"arch"`
	NICs     []NIC             `json:"nics"`
	Packages map[string]string `json:"packages"`
}

var virtualIf = regexp.MustCompile(`^(lo|pflog|pfsync|enc|bridge|vlan|lagg|gif|gre|tun|tap|vxlan|wg|ovpn|epair|ue|ipsec)[0-9]*$`)

func run(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func collectInfo() Info {
	h, _ := os.Hostname()
	in := Info{
		Hostname: h,
		Version:  run("opnsense-version"),
		Kernel:   run("uname", "-r"),
		Arch:     run("uname", "-m"),
		Packages: map[string]string{},
	}
	for _, name := range strings.Fields(run("ifconfig", "-l")) {
		if virtualIf.MatchString(name) || strings.ContainsAny(name, "._") {
			continue
		}
		n := NIC{Name: name}
		for _, l := range strings.Split(run("ifconfig", name), "\n") {
			f := strings.Fields(l)
			if len(f) == 2 && f[0] == "ether" {
				n.MAC = f[1]
			}
			if len(f) == 2 && f[0] == "hwaddr" {
				n.HW = f[1]
			}
		}
		if n.HW == "" {
			n.HW = n.MAC
		}
		in.NICs = append(in.NICs, n)
	}
	sort.Slice(in.NICs, func(i, j int) bool { return in.NICs[i].Name < in.NICs[j].Name })
	for _, l := range strings.Split(run("pkg", "query", "%n %v"), "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && (f[0] == "opnsense" || strings.HasPrefix(f[0], "os-")) {
			in.Packages[f[0]] = f[1]
		}
	}
	return in
}

// Compare returns human-readable differences; empty means the nodes are pairable.
func (a Info) Compare(b Info) []string {
	var d []string
	if a.Version != b.Version {
		d = append(d, fmt.Sprintf("OPNsense version differs: %q vs %q", a.Version, b.Version))
	}
	if a.Kernel != b.Kernel || a.Arch != b.Arch {
		d = append(d, fmt.Sprintf("kernel/arch differs: %s/%s vs %s/%s", a.Kernel, a.Arch, b.Kernel, b.Arch))
	}
	an, bn := nicNames(a), nicNames(b)
	if an != bn {
		d = append(d, fmt.Sprintf("NIC names differ: [%s] vs [%s]", an, bn))
	}
	for k, v := range a.Packages {
		if bv, ok := b.Packages[k]; !ok {
			d = append(d, fmt.Sprintf("package %s only installed on one node", k))
		} else if bv != v {
			d = append(d, fmt.Sprintf("package %s version differs: %s vs %s", k, v, bv))
		}
	}
	for k := range b.Packages {
		if _, ok := a.Packages[k]; !ok {
			d = append(d, fmt.Sprintf("package %s only installed on one node", k))
		}
	}
	sort.Strings(d)
	return d
}

func nicNames(i Info) string {
	var n []string
	for _, x := range i.NICs {
		n = append(n, x.Name)
	}
	return strings.Join(n, ",")
}
