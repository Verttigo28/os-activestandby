package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// version is set at build time (-ldflags "-X main.version=...") by CI and build/build.ps1.
var version = "0.5.3"

type Config struct {
	ListenPort   int    `json:"listen_port"`
	HADevice     string `json:"ha_device"`
	MgmtDevice   string `json:"mgmt_device"`
	AutoFailover *bool  `json:"auto_failover"`
	AllowHAOnly  bool   `json:"allow_ha_only_promote"`
	ForcePromisc bool   `json:"force_promisc"`

	NotifyLevel      string `json:"notify_level"`
	NotifyWebhook    string `json:"notify_webhook"`
	FenceCommand     string `json:"fence_command"`
	Preempt          bool   `json:"preempt"`
	PreemptDelay     int    `json:"preempt_delay"`
	MonitorIfaces    string `json:"monitor_interfaces"`
	HealthServices   bool   `json:"health_services"`
	HealthHold       int    `json:"health_hold"`
	PairToken        string `json:"pair_token"`         // secondary kit: pairing token prepared on the primary
	DetectTime       int    `json:"detect_time"`        // seconds of heartbeat silence before the peer counts as dead
	ProbeTargets     string `json:"probe_targets"`      // comma separated IPv4 addresses the active node must be able to ping
	MgmtProbeTargets string `json:"mgmt_probe_targets"` // pinged from the management address on both nodes (e.g. the management switch)
	SyncSessions     *bool  `json:"sync_sessions"`      // replicate GUI logins to the standby (default on)
	PairExpires      string `json:"pair_expires"`       // ... and its deadline
}

func (c Config) autoOn() bool       { return c.AutoFailover == nil || *c.AutoFailover }
func (c Config) syncSessions() bool { return c.SyncSessions == nil || *c.SyncSessions }

type Daemon struct {
	cfg           Config
	store         *Store
	cert          tls.Certificate
	fp            string
	pairing       *Pairing
	haAddr        string
	ops           *Ops
	sync          *syncCtl
	ev            *Events
	lastClockSet  atomicTime
	unlockedUntil atomicTime
	localVer      string
	verMu         sync.Mutex
	failMu        sync.Mutex // one switchover at a time
	sess          *sessionSync
	peerVer       string
	verMismatch   bool
	auto          *autoState
	mgmtIP        atomic.Value
	hb            *Heartbeat
	hist          *History
	health        *healthState
	drift         *driftState
	leases        *leaseState
	upd           *updateRun
}

func loadConfig() Config {
	c := Config{ListenPort: 7690}
	path := os.Getenv("HASYNC_CONF")
	if path == "" {
		path = "/usr/local/etc/hasyncd.json"
	}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &c)
	}
	if c.ListenPort == 0 {
		c.ListenPort = 7690
	}
	return c
}

// haIPv4 returns the first IPv4 address of the HA device; the peer listener binds to it only,
// so the sync/heartbeat port is never exposed on data interfaces.
func haIPv4(dev string) (string, error) {
	ifc, err := net.InterfaceByName(dev)
	if err != nil {
		return "", err
	}
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if ip, _, err := net.ParseCIDR(a.String()); err == nil && ip.To4() != nil {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("no IPv4 address on %s", dev)
}

func runDaemon() error {
	st, err := openStore()
	if err != nil {
		return err
	}
	cert, err := loadOrCreateCert(st.dir)
	if err != nil {
		return err
	}
	d := &Daemon{cfg: loadConfig(), store: st, cert: cert, fp: fingerprint(cert.Certificate[0]), pairing: &Pairing{}, ops: &Ops{}, hb: newHeartbeat(), auto: &autoState{}, sync: &syncCtl{wake: make(chan struct{}, 1)}, sess: newSessionSync()}
	setDetection(d.cfg.DetectTime)
	d.health, d.drift, d.leases, d.upd = newHealthState(), &driftState{}, &leaseState{}, &updateRun{}
	d.ev = openEvents(st.dir)
	d.ev.sink = d.notify
	d.hist = openHistory(st.dir)
	d.localVer = run("opnsense-version")
	if err := validateFenceCommand(d.cfg.FenceCommand); err != nil {
		// fail safe: the operator asked for fencing, so an unusable command must BLOCK automatic promotion (runFence
		// re-validates and refuses), never silently turn fencing off
		d.event("error", "the configured fencing command is not usable: automatic promotion is blocked until it is fixed: %v", err)
	}
	d.event("info", "hasyncd %s started (role %s, state %s%s)", version, st.Get().Role, st.Get().State, map[bool]string{true: ", STATE FILES WERE UNREADABLE", false: ""}[st.Get().Corrupt])

	listen := os.Getenv("HASYNC_LISTEN") // dev/test override, e.g. 127.0.0.1:7690
	if listen == "" {
		if d.cfg.HADevice == "" {
			return errors.New("no HA interface configured (ha_device empty)")
		}
		for {
			ip, err := haIPv4(d.cfg.HADevice)
			if err == nil {
				d.haAddr = ip
				go d.proxyListener(ip)
				listen = net.JoinHostPort(ip, fmt.Sprint(d.cfg.ListenPort))
				break
			}
			log.Printf("waiting for HA address: %v", err)
			time.Sleep(5 * time.Second)
		}
	} else {
		d.haAddr = strings.Split(listen, ":")[0]
	}

	srv := &http.Server{
		Addr:              listen,
		Handler:           d.peerMux(),
		TLSConfig:         d.peerTLS(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second,
	}
	go func() { log.Fatal(d.serveCtl()) }()
	go d.hbLoop()
	go d.enforceLoop()
	d.ensureConfigLock()
	go d.syncLoop()
	go d.pfsyncSetup()
	go d.autoLoop()
	go d.healthLoop()
	go d.driftLoop()
	go d.leaseLoop()
	go d.jobLoop()
	go d.kitLoop()
	go d.sessionLoop()
	d.startKitPairing()
	go d.mgmtListener()
	if s := d.store.Get(); s.State == StateStandby && s.ActiveSHA == "" && s.PeerFP != "" && activeValid(st.dir) {
		d.recordIntegrity()
	}
	log.Printf("hasyncd %s listening on %s (fp %s, role %s)", version, listen, d.fp[:16], st.Get().Role)
	return srv.ListenAndServeTLS("", "")
}

func (d *Daemon) peerMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/pair/hello", d.post(d.handleHello))
	mux.HandleFunc("/pair/confirm", d.post(d.handleConfirm))
	mux.HandleFunc("/v1/ping", d.paired(d.handlePing))
	mux.HandleFunc("/v1/config", d.post(d.paired(d.handleConfig)))
	mux.HandleFunc("/v1/hb", d.post(d.paired(d.handleHB)))
	mux.HandleFunc("/v1/promote", d.post(d.paired(d.handlePromote)))
	mux.HandleFunc("/v1/handover", d.post(d.paired(d.handleHandover)))
	mux.HandleFunc("/v1/break", d.post(d.paired(d.handlePeerBreak)))
	mux.HandleFunc("/v1/maintenance", d.post(d.paired(d.handleMaintenance)))
	mux.HandleFunc("/v1/preflight", d.post(d.paired(d.handlePreflight)))
	mux.HandleFunc("/v1/dryrun", d.post(d.paired(d.handleDryRun)))
	mux.HandleFunc("/v1/file", d.post(d.paired(d.handleFile)))
	mux.HandleFunc("/v1/update/start", d.post(d.paired(d.handleUpdateStart)))
	mux.HandleFunc("/v1/update/status", d.post(d.paired(d.handleUpdateStatus)))
	mux.HandleFunc("/v1/reboot", d.post(d.paired(d.handleReboot)))
	mux.HandleFunc("/v1/job", d.post(d.paired(d.handleJobAdopt)))
	mux.HandleFunc("/v1/sessions", d.post(d.paired(d.handleSessions)))
	return mux
}

func (d *Daemon) peerTLS() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{d.cert},
		ClientAuth:   tls.RequireAnyClientCert, // identity is checked by fingerprint in the handlers
		MinVersion:   tls.VersionTLS13,
	}
}

func (d *Daemon) post(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, 405, map[string]string{"error": "POST only"})
			return
		}
		h(w, r)
	}
}

// paired restricts an endpoint to the pinned peer certificate.
func (d *Daemon) paired(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fp, err := peerFP(r)
		st := d.store.Get()
		if err != nil || st.PeerFP == "" || fp != st.PeerFP {
			writeJSON(w, 403, map[string]string{"error": "not the paired peer"})
			return
		}
		h(w, r)
	}
}

func (d *Daemon) handlePing(w http.ResponseWriter, r *http.Request) {
	st := d.store.Get()
	writeJSON(w, 200, map[string]any{"role": st.Role, "epoch": st.Epoch, "version": version, "time": time.Now().UTC()})
}

func (d *Daemon) handlePeerBreak(w http.ResponseWriter, r *http.Request) {
	if err := d.store.Reset(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	d.pfsyncTeardown()
	log.Printf("pair broken by peer")
	writeJSON(w, 200, map[string]string{"status": "broken"})
}

// breakPair tells the peer (best effort) and resets the local state.
func (d *Daemon) breakPair() (string, error) {
	st := d.store.Get()
	msg := "local state reset"
	if st.PeerFP != "" && st.PeerAddr != "" {
		cl := d.peerClient(st.PeerFP, nil)
		cl.Timeout = 5 * time.Second
		resp, err := cl.Post("https://"+net.JoinHostPort(st.PeerAddr, fmt.Sprint(st.PeerPort))+"/v1/break", "application/json", nil)
		if err != nil {
			msg += "; peer unreachable, run break on it as well"
		} else {
			resp.Body.Close()
			msg += "; peer reset"
		}
	}
	err := d.store.Reset()
	d.pfsyncTeardown()
	return msg, err
}

func (d *Daemon) mgmtAddr() string {
	if v, ok := d.mgmtIP.Load().(string); ok {
		return v
	}
	return ""
}

// mgmtListener serves the same peer API on the management network (secondary heartbeat path).
func (d *Daemon) mgmtListener() {
	if d.cfg.MgmtDevice == "" {
		return
	}
	for {
		ip, err := haIPv4(d.cfg.MgmtDevice)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		d.mgmtIP.Store(ip)
		srv := &http.Server{Addr: net.JoinHostPort(ip, fmt.Sprint(d.cfg.ListenPort)), Handler: d.peerMux(),
			TLSConfig: d.peerTLS(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second}
		log.Printf("secondary heartbeat path listening on %s", srv.Addr)
		log.Printf("mgmt listener ended: %v", srv.ListenAndServeTLS("", ""))
		d.mgmtIP.Store("")
		time.Sleep(5 * time.Second)
	}
}
