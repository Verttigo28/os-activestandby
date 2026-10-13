package main

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
)

// Fast classic takeover. The classic takeover runs OPNsense's PHP interface code once per interface; with 20 VLANs that is
// 2.3 s of CPU, whatever the parallelism (measured on the target hardware, docs/validation.md section J). For a VLAN with a
// static IPv4 address and IP aliases that code does exactly this, and nothing else:
//
//	flush the addresses, set the MAC (when it differs), inet <address>, inet6 -accept_rtadv -no_dad description <name> up,
//	inet <alias> alias ...
//
// It skips the hardware offload settings for VLAN devices (_interfaces_hardware returns early for them), so a VLAN has no
// offload state to reproduce. Everything else it does for other kinds of interface (MTU, IPv6, DHCP, bridges, GRE/GIF,
// static ARP entries, IP aliases with a gateway or a vhid, ...) makes the plan say "fast": false, and the stock code runs.
// The physical NICs and the LAGG always use the stock code (they carry the offload settings and the link negotiation).
//
// One ifconfig per VLAN, all at once. A VLAN that is not in the expected state before, or does not have the planned
// state afterwards, is left to the stock code, which flushes and reconfigures it: the fast path can only make a takeover
// faster, never different.

var (
	fastDescrRe = regexp.MustCompile(`^[A-Za-z0-9 _().:/+-]{1,64}$`)
	fastDevRe   = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,30}$`)
)

// fastOK re-checks in Go what ifaces.php decided: a plan from another version or a bug there must not reach ifconfig.
func fastOK(i WarmIface) bool {
	return i.Fast && i.Parent != "" && fastDevRe.MatchString(i.Dev) && validCIDR(i.FastV4, 4) && allCIDR(i.VIPs, 4) &&
		(i.FastMAC == "" || validMAC(i.FastMAC)) && fastDescrRe.MatchString(i.Descr)
}

// fastCmds are the ifconfig invocations that bring the VLAN up, in the order of OPNsense's own code: the address, then the
// interface options and "up", then one invocation per alias. They do not combine: after "inet <address>" ifconfig reads the
// next word as the peer address of a point-to-point link (also after "alias"), and "inet" after the inet6 options is
// refused ("inet6: bad value", "inet: bad value": both met on the hardware).
func fastCmds(i WarmIface) [][]string {
	second := []string{i.Dev, "inet6", "-accept_rtadv", "-no_dad", "description", i.Descr, "up"}
	if i.Promisc {
		second = append(second, "promisc")
	}
	cmds := [][]string{{i.Dev, "inet", i.FastV4}, second}
	for _, v := range i.VIPs {
		cmds = append(cmds, []string{i.Dev, "inet", v, "alias"})
	}
	return cmds
}

// fastOne brings one VLAN up and verifies the result.
func (d *Daemon) fastOne(i WarmIface) error {
	st := d.readIf(i.Dev)
	if !st.Exists {
		return fmt.Errorf("%s does not exist", i.Dev)
	}
	if len(st.Inet) > 0 {
		return fmt.Errorf("%s still has the addresses %v", i.Dev, st.Inet)
	}
	if i.FastMAC != "" && !macEq(st.Ether, i.FastMAC) {
		if out, err := d.run("ifconfig", i.Dev, "link", i.FastMAC); err != nil {
			return fmt.Errorf("link %s: %v: %s", i.FastMAC, err, out)
		}
	}
	for _, c := range fastCmds(i) {
		if out, err := d.run("ifconfig", c...); err != nil {
			return fmt.Errorf("ifconfig %s: %v: %s", strings.Join(c, " "), err, out)
		}
	}
	after := d.readIf(i.Dev)
	if !after.Up || !hasAddr(after.Inet, i.FastV4) || (i.FastMAC != "" && !macEq(after.Ether, i.FastMAC)) {
		return fmt.Errorf("%s did not come up as planned (up %v, mac %s, addresses %v)", i.Dev, after.Up, after.Ether, after.Inet)
	}
	for _, v := range i.VIPs {
		if !hasAddr(after.Inet, v) {
			return fmt.Errorf("%s lacks the alias %s", i.Dev, v)
		}
	}
	return nil
}

// fastTier configures what it can of group natively, all VLANs at once, and returns the interfaces that remain for the stock
// code (everything that is not in the plan as fast, and everything that failed).
func (d *Daemon) fastTier(planPath string, group []Iface) []Iface {
	if !d.cfg.fastOn() || len(group) == 0 {
		return group
	}
	p, err := d.planFor(planPath)
	if err != nil || p == nil {
		if err != nil {
			log.Printf("promote: plan for the fast takeover: %v", err)
		}
		return group
	}
	byKey := map[string]WarmIface{}
	for _, e := range p.Ifaces {
		if fastOK(e) {
			byKey[e.Key] = e
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	ok := map[string]bool{}
	for _, i := range group {
		e, fast := byKey[i.Key]
		if !fast || e.Dev != i.Dev {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.fastOne(e); err != nil {
				log.Printf("promote: fast takeover of %s failed, the stock code will do it: %v", e.Key, err)
				return
			}
			mu.Lock()
			ok[e.Key] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	var rest []Iface
	for _, i := range group {
		if !ok[i.Key] {
			rest = append(rest, i)
		}
	}
	return rest
}
