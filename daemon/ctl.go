package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const spoofScript = "/usr/local/opnsense/scripts/OPNsense/ActiveStandby/spoof_macs.php"

func ctlAddr() (network, addr string) {
	v := os.Getenv("HASYNC_SOCK")
	if v == "" {
		v = "/var/run/hasyncd.sock"
	}
	if strings.HasPrefix(v, "tcp:") {
		return "tcp", strings.TrimPrefix(v, "tcp:")
	}
	return "unix", v
}

// ---- server side: local control socket (root only) ----

func (d *Daemon) serveCtl() error {
	network, addr := ctlAddr()
	if network == "unix" {
		os.Remove(addr)
	}
	var l net.Listener
	var err error
	withUmask(0o177, func() { l, err = net.Listen(network, addr) })
	if err != nil {
		return err
	}
	if network == "unix" {
		os.Chmod(addr, 0o600)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", d.ctlStatus)
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, collectInfo()) })
	mux.HandleFunc("/prepare", d.ctlPrepare)
	mux.HandleFunc("/cancel", func(w http.ResponseWriter, r *http.Request) {
		d.pairing.Cancel()
		writeJSON(w, 200, map[string]string{"status": "cancelled"})
	})
	mux.HandleFunc("/pair", d.ctlPair)
	mux.HandleFunc("/promote", d.ctlPromote)
	mux.HandleFunc("/unlock", func(w http.ResponseWriter, r *http.Request) {
		d.unlockedUntil.Set(time.Now().Add(30 * time.Minute))
		unlockConfig()
		d.event("warning", "standby config unlocked for 30 minutes by the operator (e.g. for an update)")
		writeJSON(w, 200, map[string]string{"status": "ok", "message": "config.xml unlocked for 30 minutes"})
	})
	mux.HandleFunc("/lock", func(w http.ResponseWriter, r *http.Request) {
		d.unlockedUntil.Set(time.Time{})
		d.ensureConfigLock()
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"events": d.ev.Recent(50)})
	})
	mux.HandleFunc("/sync-now", func(w http.ResponseWriter, r *http.Request) {
		d.wakeSync()
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/resync", func(w http.ResponseWriter, r *http.Request) {
		d.store.Update(func(s *State) { s.AckedSHA = "" })
		d.wakeSync()
		writeJSON(w, 200, map[string]string{"status": "ok", "message": "full resync queued"})
	})
	mux.HandleFunc("/demote", d.ctlDemote)
	mux.HandleFunc("/failover", d.ctlFailover)
	mux.HandleFunc("/break", func(w http.ResponseWriter, r *http.Request) {
		msg, err := d.breakPair()
		if err != nil {
			writeJSON(w, 200, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok", "message": msg})
	})
	d.registerKit(mux)
	d.registerUpdates(mux)
	d.registerCtl2(mux)
	return http.Serve(l, mux)
}

func (d *Daemon) ctlStatus(w http.ResponseWriter, r *http.Request) {
	st := d.store.Get()
	active, exp := d.pairing.Active()
	out := map[string]any{
		"hostname": hostName(), "mgmt_probe": d.mgmtProbe(), "daemon": "up", "version": version, "role": st.Role, "epoch": st.Epoch,
		"peer_addr": st.PeerAddr, "peer_fp": st.PeerFP, "paired_at": st.PairedAt,
		"local_fp": d.fp, "ha_address": d.haAddr, "listen_port": d.cfg.ListenPort,
		"pairing_open": active, "state": st.State, "state_lost": st.Corrupt, "proxy_port": proxyPort(d.cfg), "auto_failover": d.cfg.autoOn(), "auto_blocked": d.auto.Blocked(),
		"mgmt_address": d.mgmtAddr(), "peer_mgmt": st.PeerMgmt,
		"os_version": d.localVer, "maintenance": d.maintenanceOn(), "maintenance_until": st.MaintenanceUntil,
		"health": d.healthReason(), "degraded": d.degradedReason(), "pfsync_problem": d.pfsyncStatus(), "package_diff": d.packageReport(), "failover_in_progress": d.failing.Load(), "drift": d.driftReason(), "fencing": d.cfg.FenceCommand != "", "preempt": d.cfg.Preempt,
	}
	if st.Kit != nil {
		out["kit"] = map[string]string{"peer": st.Kit.Peer, "expires": st.Kit.Expires, "error": st.Kit.Error}
	}
	if st.Job != nil {
		out["job"] = st.Job
	}
	alive, ago, peer := d.hb.Snapshot()
	if st.PeerFP != "" {
		out["peer_alive"], out["peer_state"], out["peer_epoch"] = alive, peer.State, peer.Epoch
		out["peer_hostname"] = peer.Host
		out["peer_mgmt_probe"] = peer.MgmtProbe
		out["peer_last_hb_ms"] = ago.Milliseconds()
		out["paths"] = map[string]bool{"ha": d.hb.PathAlive("ha"), "mgmt": d.hb.PathAlive("mgmt")}
		pv, mism := d.versionInfo()
		out["peer_os_version"], out["version_mismatch"] = pv, mism
	}
	if active {
		out["pairing_expires"] = exp.UTC().Format(time.RFC3339)
	}
	if st.PeerFP != "" {
		out["sync"] = d.syncStatus(st)
		out["peer_reachable"] = d.pingPeer()
	}
	writeJSON(w, 200, out)
}

func (d *Daemon) pingPeer() bool {
	st := d.store.Get()
	cl := d.peerClient(st.PeerFP, nil)
	cl.Timeout = 3 * time.Second
	resp, err := cl.Get("https://" + net.JoinHostPort(st.PeerAddr, fmt.Sprint(st.PeerPort)) + "/v1/ping")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func (d *Daemon) ctlPrepare(w http.ResponseWriter, r *http.Request) {
	if d.store.Get().Role != RoleStandalone {
		writeJSON(w, 200, map[string]string{"error": "node is already paired; break the pair first"})
		return
	}
	tok, exp, err := d.pairing.Start()
	if err != nil {
		writeJSON(w, 200, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"token": tok, "expires": exp.UTC().Format(time.RFC3339),
		"address": d.haAddr, "port": d.cfg.ListenPort, "fingerprint": d.fp,
	})
}

func (d *Daemon) ctlPair(w http.ResponseWriter, r *http.Request) {
	var req struct{ Peer, Token string }
	if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req) != nil || req.Peer == "" || req.Token == "" {
		writeJSON(w, 200, map[string]string{"error": "peer and token required"})
		return
	}
	res, diffs, err := d.doPair(req.Peer, d.cfg.ListenPort, req.Token)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error(), "diffs": diffs})
		return
	}
	out := map[string]any{"status": "paired", "peer_hostname": res.Peer.Hostname, "peer_fp": res.FP}
	// Master: pin the live MAC of every data interface into config.xml (spoofmac) so the config we
	// replicate makes the standby come up with identical MACs. A no-op for the running master.
	if d.pinMACs() {
		out["spoofmac"] = "a shared MAC address was set on the data interfaces"
	}
	writeJSON(w, 200, out)
}

// ---- client side: `hasyncd ctl <cmd>` ----

func runCtl(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: hasyncd ctl status|info|prepare|cancel|pair --peer IP --token T|break")
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	peer := fs.String("peer", "", "")
	token := fs.String("token", "", "")
	tokFile := fs.String("token-file", "", "") // the GUI hands the token over in a root-only file: argv is visible to every local user
	force := fs.Bool("force", false, "")
	hours := fs.Int("hours", defaultMaintenanceHours, "")
	failback := fs.Bool("failback", false, "")
	amsg := fs.String("msg", "", "")
	kha := fs.String("ha", "", "")
	kmgmt := fs.String("mgmt", "", "")
	khost := fs.String("hostname", "", "")
	sub := ""
	if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		sub = args[1]
		fs.Parse(args[2:])
	} else {
		fs.Parse(args[1:])
	}

	network, addr := ctlAddr()
	cl := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}}
	var resp *http.Response
	var err error
	switch cmd {
	case "status", "info", "events":
		resp, err = cl.Get("http://ctl/" + cmd)
	case "prepare", "cancel", "break", "demote", "sync-now", "resync", "unlock", "lock":
		resp, err = cl.Post("http://ctl/"+cmd, "application/json", nil)
	case "failover":
		resp, err = cl.Post("http://ctl/failover?force="+fmt.Sprint(*force), "application/json", nil)
	case "promote":
		resp, err = cl.Post("http://ctl/promote?force="+fmt.Sprint(*force), "application/json", nil)
	case "pair":
		if *tokFile != "" {
			t, ferr := readTokenFile(*tokFile)
			if ferr != nil {
				fmt.Println(`{"error":"cannot read the pairing token"}`)
				return 1
			}
			*token = t
		}
		b, _ := json.Marshal(map[string]string{"peer": *peer, "token": *token})
		resp, err = cl.Post("http://ctl/pair", "application/json", bytes.NewReader(b))
	case "kit":
		b, _ := json.Marshal(map[string]string{"ha": *kha, "mgmt": *kmgmt, "hostname": *khost})
		resp, err = cl.Post("http://ctl/kit", "application/json", bytes.NewReader(b))
	case "audit":
		resp, err = cl.Post("http://ctl/audit?msg="+url.QueryEscape(*amsg), "application/json", nil)
	case "updates":
		resp, err = cl.Get("http://ctl/updates")
	case "updates-check":
		resp, err = cl.Post("http://ctl/updates/check", "application/json", nil)
	case "kit-cancel":
		resp, err = cl.Post("http://ctl/kit/cancel", "application/json", nil)
	case "health", "metrics", "history", "preflight":
		resp, err = cl.Get("http://ctl/" + cmd)
	case "maintenance": // maintenance on|off [--hours N]
		on := "0"
		if sub == "on" {
			on = "1"
		}
		resp, err = cl.Post(fmt.Sprintf("http://ctl/maintenance?on=%s&hours=%d", on, *hours), "application/json", nil)
	case "upgrade": // upgrade start [--failback] | cancel | status
		switch sub {
		case "start":
			resp, err = cl.Post("http://ctl/upgrade/start?failback="+fmt.Sprint(*failback), "application/json", nil)
		case "cancel":
			resp, err = cl.Post("http://ctl/upgrade/cancel", "application/json", nil)
		default:
			resp, err = cl.Get("http://ctl/upgrade/status")
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown command", cmd)
		return 2
	}
	if err != nil {
		// daemon not running: still give a machine-readable answer for the GUI
		st, serr := openStore()
		if cmd == "status" && serr == nil {
			s := st.Get()
			b, _ := json.Marshal(map[string]any{"daemon": "down", "role": s.Role, "peer_addr": s.PeerAddr, "paired_at": s.PairedAt, "epoch": s.Epoch})
			fmt.Println(string(b))
			return 0
		}
		fmt.Println(`{"error":"daemon is not running (enable the service first)"}`)
		return 1
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	fmt.Println(strings.TrimSpace(string(b)))
	if cmd == "health" { // exit status for Monit: 0 ok, 1 warning, 2 critical
		var h struct{ Level int }
		json.Unmarshal(b, &h)
		return h.Level
	}
	return 0
}

func (d *Daemon) ctlPromote(w http.ResponseWriter, r *http.Request) {
	st := d.store.Get()
	if st.Role == RoleStandalone {
		writeJSON(w, 200, map[string]string{"error": "node is not paired"})
		return
	}
	if r.URL.Query().Get("force") != "true" {
		if alive, _, peer := d.hb.Snapshot(); alive && peer.State == StateActive {
			writeJSON(w, 200, map[string]string{"error": "peer is alive and active; use failover, or force to override (risk of split-brain)"})
			return
		}
	}
	t0 := time.Now()
	if err := d.promote("manual"); err != nil {
		writeJSON(w, 200, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"status": "active", "ms": time.Since(t0).Milliseconds()})
}

func (d *Daemon) ctlDemote(w http.ResponseWriter, r *http.Request) {
	if d.store.Get().Role == RoleStandalone {
		writeJSON(w, 200, map[string]string{"error": "node is not paired"})
		return
	}
	t0 := time.Now()
	if err := d.demote("manual"); err != nil {
		writeJSON(w, 200, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"status": "standby", "ms": time.Since(t0).Milliseconds()})
}

func (d *Daemon) ctlFailover(w http.ResponseWriter, r *http.Request) {
	msg, err := d.failover(r.URL.Query().Get("force") == "true")
	if err != nil {
		writeJSON(w, 200, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok", "message": msg})
}

var tokenFileRe = regexp.MustCompile(`^/var/run/aspt[A-Za-z0-9]{4,32}$`)

// readTokenFile reads and removes a token file written by the GUI controller (root-only, fixed place and name).
func readTokenFile(path string) (string, error) {
	if !tokenFileRe.MatchString(path) {
		return "", errors.New("unexpected token file name")
	}
	defer os.Remove(path)
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 256 {
		return "", errors.New("unusable token file")
	}
	b, err := os.ReadFile(path)
	return strings.TrimSpace(string(b)), err
}
