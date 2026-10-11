package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Secondary kit: the primary generates the configuration file for the second firewall. Restoring it on a fresh node
// (with the plugin installed) gives that node its own hostname and HA/management addresses, the shared interface
// settings in their silent standby form, and a one-time pairing token. The secondary opens its pairing window with
// that token on start; the primary keeps trying to pair with it (Kit in the state file) until it answers.

const (
	kitScript = "/usr/local/opnsense/scripts/OPNsense/ActiveStandby/kit_build.php"
	kitTTL    = 48 * time.Hour
)

var (
	hostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
)

// Kit is what the primary remembers about a secondary it is waiting for.
type Kit struct {
	Peer    string `json:"peer"`
	Token   string `json:"token"`
	Expires string `json:"expires"`
	Error   string `json:"error,omitempty"`
}

// StartWith opens the pairing window with a token that was prepared in advance (secondary kit).
func (p *Pairing) StartWith(token string, expires time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.s = &pairSession{token: normToken(token), expires: expires, hellos: map[string]helloState{}, helloFails: map[string]int{}, blocked: map[string]time.Time{}}
}

// startKitPairing runs on a secondary that was configured from a kit.
func (d *Daemon) startKitPairing() {
	if d.cfg.PairToken == "" || d.store.Get().Role != RoleStandalone {
		return
	}
	exp, err := time.Parse(time.RFC3339, d.cfg.PairExpires)
	if err != nil || time.Now().After(exp) {
		return
	}
	d.pairing.StartWith(d.cfg.PairToken, exp)
	d.event("info", "pairing window opened from the secondary kit (valid until %s); waiting for the primary", exp.UTC().Format(time.RFC3339))
}

func (d *Daemon) registerKit(mux *http.ServeMux) {
	mux.HandleFunc("/audit", func(w http.ResponseWriter, r *http.Request) {
		msg := regexp.MustCompile(`[^A-Za-z0-9._:@-]`).ReplaceAllString(r.URL.Query().Get("msg"), "")
		d.event("info", "AUDIT %s", msg)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/kit", d.ctlKit)
	mux.HandleFunc("/kit/cancel", func(w http.ResponseWriter, r *http.Request) {
		d.store.Update(func(s *State) { s.Kit = nil })
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
}

func (d *Daemon) ctlKit(w http.ResponseWriter, r *http.Request) {
	fail := func(msg string) { writeJSON(w, 200, map[string]string{"error": msg}) }
	var req struct{ HA, Mgmt, Hostname string }
	if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req) != nil {
		fail("bad request")
		return
	}
	st := d.store.Get()
	if st.Role != RoleStandalone {
		fail("this node is already paired")
		return
	}
	if d.cfg.HADevice == "" || d.haAddr == "" {
		fail("configure the HA link interface first (setup wizard, step 1)")
		return
	}
	ip := net.ParseIP(req.HA)
	if ip == nil || ip.To4() == nil || req.HA == d.haAddr {
		fail("enter the HA link IPv4 address of the second firewall (it must differ from this node's " + d.haAddr + ")")
		return
	}
	if req.Mgmt == "-" {
		req.Mgmt = ""
	}
	if req.Mgmt != "" && req.Mgmt != "dhcp" && (net.ParseIP(req.Mgmt) == nil || net.ParseIP(req.Mgmt).To4() == nil) {
		fail("management address must be an IPv4 address or dhcp")
		return
	}
	if !hostRe.MatchString(req.Hostname) {
		fail("invalid hostname")
		return
	}
	// pin the live MACs first: the secondary has to come up with exactly these
	d.pinMACs()
	token, err := newToken()
	if err != nil {
		fail(err.Error())
		return
	}
	exp := time.Now().Add(kitTTL).UTC().Format(time.RFC3339)
	mg := req.Mgmt
	if mg == "" {
		mg = "-"
	}
	out, err := exec.Command(kitScript, configPath(), req.HA, mg, req.Hostname, token, exp, d.haAddr).Output()
	if err != nil {
		msg := err.Error()
		if ee, ok := err.(*exec.ExitError); ok {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		fail("building the kit failed: " + msg)
		return
	}
	d.store.Update(func(s *State) { s.Kit = &Kit{Peer: req.HA, Token: token, Expires: exp} })
	d.event("change", "secondary kit created for %s (%s); waiting for it to come up", req.Hostname, req.HA)
	writeJSON(w, 200, map[string]any{"xml": string(out), "filename": "config-" + req.Hostname + "-secondary.xml", "peer": req.HA, "expires": exp})
}

// kitLoop makes the primary pair with the secondary as soon as it answers.
func (d *Daemon) kitLoop() {
	for range time.Tick(4 * time.Second) {
		st := d.store.Get()
		if st.Kit == nil {
			continue
		}
		if st.Role != RoleStandalone {
			d.store.Update(func(s *State) { s.Kit = nil })
			continue
		}
		if exp, err := time.Parse(time.RFC3339, st.Kit.Expires); err != nil || time.Now().After(exp) {
			d.event("warning", "the secondary kit expired before the second firewall came up")
			d.store.Update(func(s *State) { s.Kit = nil })
			continue
		}
		// cheap reachability check first: pairing gathers node info, which is expensive to do every few seconds for hours
		if c, derr := net.DialTimeout("tcp", net.JoinHostPort(st.Kit.Peer, fmt.Sprint(d.cfg.ListenPort)), 1500*time.Millisecond); derr != nil {
			continue
		} else {
			c.Close()
		}
		_, diffs, err := d.doPair(st.Kit.Peer, d.cfg.ListenPort, st.Kit.Token)
		if err != nil {
			msg := err.Error()
			if len(diffs) > 0 {
				msg += ": " + strings.Join(diffs, "; ")
			}
			if msg != st.Kit.Error {
				d.store.Update(func(s *State) {
					if s.Kit != nil {
						s.Kit.Error = msg
					}
				})
			}
			continue
		}
		d.pinMACs()
		d.store.Update(func(s *State) { s.Kit = nil })
		d.event("change", "the second firewall came up and was paired automatically")
	}
}
