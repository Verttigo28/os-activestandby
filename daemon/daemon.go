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
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// version is set at build time (-ldflags "-X main.version=...") by CI and build/build.ps1.
var version = "1.0.4"

type Config struct {
	ListenPort   int    `json:"listen_port"`
	HADevice     string `json:"ha_device"`
	MgmtDevice   string `json:"mgmt_device"`
	AutoFailover *bool  `json:"auto_failover"`
	AllowHAOnly  bool   `json:"allow_ha_only_promote"`
	// FenceRequired: a standby never takes over by itself because the peer went silent unless a fencing command is configured and succeeds.
	FenceRequired bool  `json:"fence_required"`
	ForcePromisc  bool  `json:"force_promisc"`
	FastIfaces    *bool `json:"fast_ifaces"` // bring static VLANs up natively at a takeover (default on, see fastpath.go)

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
	HAMode           string `json:"ha_mode"`            // "classic" (default), "warm" (warm.go) or "hot" (hot.go)
	HotLACP          string `json:"hot_lacp"`           // hot standby with a LACP LAGG: "shared" (the standby LAGG carries the shared MAC) or "off" (default)
}

func (c Config) autoOn() bool       { return c.AutoFailover == nil || *c.AutoFailover }
func (c Config) syncSessions() bool { return c.SyncSessions == nil || *c.SyncSessions }
func (c Config) fastOn() bool       { return c.FastIfaces == nil || *c.FastIfaces }

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
	lastClockWarn atomicTime
	unlockedUntil atomicTime
	localVer      string
	verMu         sync.Mutex
	failMu        sync.Mutex // one switchover at a time
	pkgMu         sync.Mutex // one package installation at a time
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
	act           *actions     // test seams (nil: the real system)
	tx            txLog        // transition ids handled by /v1/promote and /v1/abort
	failing       atomic.Bool  // a graceful failover started here is in progress
	degraded      atomic.Value // string: why the last promotion left this node degraded (empty: healthy)
	pfsyncProblem atomic.Value // string: why pfsync is not in place (empty: as configured)
	pkgReport     atomic.Value // string: how the plugin set of this standby differs from the active node (empty: conform)
	wst           warmStatus   // readiness of the warm standby (see warm.go)
	hwOnce        sync.Once
	hwv           *hwMacs       // permanent MAC addresses of the warm devices
	rs            rulesetMirror // the ruleset of the active node as a hot standby holds it (see ruleset.go)
	runIntent     string        // second copy of the transition record (intent.go); "" = runIntentPath
	cfgProblem    string        // the settings file could not be read in full (loadConfigChecked): what is used instead
	pkgr          pkgRun        // the plugin installation in progress (packages.go)
	maintExpiring atomic.Bool   // the end of the maintenance mode is being written (maintenance.go)
}

func loadConfig() Config {
	c, _ := loadConfigChecked()
	return c
}

// goodConfigPath keeps a copy of the last settings file that could be read in full.
func goodConfigPath() string { return filepath.Join(stateDir(), "hasyncd.json.good") }

// loadConfigChecked reads the daemon settings. A file that is not valid JSON is never half used: decoding stops at the error, and the
// fields after it (fence_required, auto_failover, the detection time, ...) would silently take their default values. Instead the last
// settings that could be read in full are used; if there are none, the daemon runs with automatic failover off and fencing required
// (it serves as it is, but takes no decision of its own) until the file is fixed. problem says which, for the events and the
// pre-flight ("" when the file was fine).
func loadConfigChecked() (Config, string) {
	path := os.Getenv("HASYNC_CONF")
	if path == "" {
		path = "/usr/local/etc/hasyncd.json"
	}
	c := Config{ListenPort: 7690}
	problem := ""
	if b, err := os.ReadFile(path); err == nil {
		os.Chmod(path, 0o600) // webhook URL and kit token live here
		var parsed Config
		if err := json.Unmarshal(b, &parsed); err == nil {
			c = parsed
			if c.ListenPort == 0 {
				c.ListenPort = 7690
			}
			if old, _ := os.ReadFile(goodConfigPath()); string(old) != string(b) {
				if werr := fsWrite(goodConfigPath(), b, 0o600); werr != nil {
					log.Printf("settings: the copy of the last valid settings was not saved: %v", werr)
				}
			}
		} else {
			problem = fmt.Sprintf("%s is not valid (%v)", path, err)
			var good Config
			if gb, gerr := os.ReadFile(goodConfigPath()); gerr == nil && json.Unmarshal(gb, &good) == nil {
				c = good
				problem += ": the last valid settings are used until it is fixed"
			} else {
				off := false
				c.AutoFailover, c.FenceRequired = &off, true
				problem += " and no earlier valid settings exist: automatic failover is OFF until the settings are saved again"
			}
			log.Printf("ERROR: %s", problem)
		}
	}
	if c.ListenPort < 1024 || c.ListenPort > 65535 {
		c.ListenPort = 7690
	}
	if c.HotLACP != "shared" {
		c.HotLACP = "off"
	}
	if c.HAMode != "warm" && c.HAMode != "hot" { // anything else is the classic mode
		c.HAMode = "classic"
	}
	// device names end up as arguments of ifconfig and friends: only plain names
	for _, dev := range []*string{&c.HADevice, &c.MgmtDevice} {
		if *dev != "" && !devNameRe.MatchString(*dev) {
			log.Printf("ERROR: ignoring invalid device name %q in the daemon configuration", *dev)
			*dev = ""
		}
	}
	return c, problem
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
	cert, identityReplaced, err := loadIdentity(st.dir)
	if err != nil {
		return err
	}
	cfg, cfgProblem := loadConfigChecked()
	d := &Daemon{cfg: cfg, cfgProblem: cfgProblem, store: st, cert: cert, fp: fingerprint(cert.Certificate[0]), pairing: &Pairing{}, ops: &Ops{}, hb: newHeartbeat(), auto: &autoState{}, sync: &syncCtl{wake: make(chan struct{}, 1)}, sess: newSessionSync()}
	setDetection(d.cfg.DetectTime)
	d.health, d.drift, d.leases, d.upd = newHealthState(), &driftState{}, &leaseState{}, &updateRun{}
	d.hb.inc = nextIncarnation(st.dir) // increasing across restarts, whatever the clock does
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

	if d.cfgProblem != "" {
		d.event("error", "plugin settings: %s", d.cfgProblem)
	}
	// a promotion or demotion that the last run of the daemon did not finish (intent.go): back to standby before anything acts on the role
	d.recoverTransition()
	if identityReplaced && st.Get().PeerFP != "" {
		// the identity the peer pinned is lost: the peer can no longer hear this node, and would take this node for dead. A node that
		// cannot prove who it is must not keep the shared identity: standby until the pair is formed again
		d.event("error", "the TLS identity of this node was damaged and replaced: the peer will refuse it. This node goes to STANDBY; break the pair and pair again")
		d.toSafeStandby("replaced TLS identity", true)
	}

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
	go d.pfsyncLoop()
	go d.autoLoop()
	go d.healthLoop()
	go d.rulesetLoop()
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
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	return srv.ServeTLS(d.guard(ln), "", "")
}

// peerRoute is one endpoint of the peer API. Every route goes through paired (the pinned peer certificate only), and every route that
// changes something through post as well: the table is the only place routes are declared, so none can be added without them.
type peerRoute struct {
	path     string
	h        http.HandlerFunc
	readOnly bool // any method (it reads only); otherwise POST only
}

func (d *Daemon) peerRoutes() []peerRoute {
	return []peerRoute{
		{"/v1/ping", d.handlePing, true},
		{"/v1/config", d.handleConfig, false},
		{"/v1/hb", d.handleHB, false},
		{"/v1/promote", d.handlePromote, false},
		{"/v1/abort", d.handleAbort, false},
		{"/v1/handover", d.handleHandover, false},
		{"/v1/break", d.handlePeerBreak, false},
		{"/v1/maintenance", d.handleMaintenance, false},
		{"/v1/preflight", d.handlePreflight, false},
		{"/v1/dryrun", d.handleDryRun, false},
		{"/v1/file", d.handleFile, false},
		{"/v1/update/start", d.handleUpdateStart, false},
		{"/v1/update/status", d.handleUpdateStatus, false},
		{"/v1/reboot", d.handleReboot, false},
		{"/v1/job", d.handleJobAdopt, false},
		{"/v1/sessions", d.handleSessions, false},
		{"/v1/ruleset", d.handleRuleset, false},
	}
}

func (d *Daemon) peerMux() *http.ServeMux {
	mux := http.NewServeMux()
	// pairing: before a peer is pinned, protected by the one-time token and its HMAC (pairing.go)
	mux.HandleFunc("/pair/hello", d.post(d.handleHello))
	mux.HandleFunc("/pair/confirm", d.post(d.handleConfirm))
	for _, r := range d.peerRoutes() {
		h := d.paired(r.h)
		if !r.readOnly {
			h = d.post(h)
		}
		mux.HandleFunc(r.path, h)
	}
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
		if ln, lerr := net.Listen("tcp", srv.Addr); lerr != nil {
			log.Printf("mgmt listener: %v", lerr)
		} else {
			log.Printf("mgmt listener ended: %v", srv.ServeTLS(d.guard(ln), "", ""))
		}
		d.mgmtIP.Store("")
		time.Sleep(5 * time.Second)
	}
}

var devNameRe = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,30}$`)
