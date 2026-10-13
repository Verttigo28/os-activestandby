package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Ruleset mirror (hot standby only).
//
// A hot standby has its links up and takes over in 0.2 s, but the packet filter it holds is the one generated from its own
// standby view of the configuration (no rule for the data interfaces): established connections pass on the states pfsync
// replicated, a NEW connection waits until the takeover has generated and loaded the ruleset (about 1 s, measured).
//
// The mirror closes that gap. The active node sends the ruleset it has loaded (/tmp/rules.debug) to the standby, which loads
// it as its own packet filter ruleset; the standby reports the hash of what it holds in its heartbeat, so the active node
// sends again whenever they differ (standby restarted, standby reloaded its own filter after a configuration sync, new rules
// on the active node). A new connection at the takeover is then judged by the ruleset the active node was using.
//
// It cannot make a node less safe than it is without it:
//   - the peer is the pinned peer of the configuration sync (mutual TLS), a ruleset is as trusted as a configuration;
//   - the text must contain the default deny rule, must have no NUL, must be at most 8 MiB, and must pass `pfctl -n` here;
//   - `set hostid` and the `set limit` lines are replaced by the ones of this node's own ruleset (two nodes with the same
//     pf hostid would make the pfsync state ids collide), and without them nothing is loaded;
//   - after the load the node must still hear its peer; if not, the ruleset of the standby view is loaded again and the
//     mirror stays off for ten minutes (a ruleset that locked the nodes out of each other must not stay);
//   - nothing is loaded while a promotion or a demotion runs (they hold the operations lock).

const (
	rulesetMax     = 8 << 20
	rulesetEvery   = 3 * time.Second  // between two sends
	rulesetResend  = 30 * time.Second // a send nobody confirmed in the heartbeat is repeated
	rulesetBackoff = 10 * time.Minute // mirror off after a ruleset that cut the nodes off each other
)

var localRulesPath = "/tmp/rules.debug"

// rulesetSettle: how long a loaded mirror is given before the node checks that it still hears its peer (replaced in the tests)
var rulesetSettle = func() time.Duration { return 2 * hbInterval }

type rulesetMirror struct {
	mu      sync.Mutex
	applied string    // hash of the active node's ruleset as it is loaded here ("" = none)
	blocked time.Time // no mirror before this time
	sentSHA string    // active side: what was sent last ...
	sentAt  time.Time // ... and when
	nextTry time.Time // active side: no send before this time
	fails   int       // active side: consecutive sends that failed or were refused
}

func (m *rulesetMirror) get() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applied
}

func (m *rulesetMirror) set(sha string) {
	m.mu.Lock()
	m.applied = sha
	m.mu.Unlock()
}

// hbValue is what the heartbeat of a hot standby carries: the hash of the mirrored ruleset it holds.
func (d *Daemon) rulesetHB(st State) string {
	if !d.hotOn() || st.State != StateStandby {
		return ""
	}
	return d.rs.get()
}

// filterReloadLocal reloads the packet filter from this node's own configuration. That replaces a mirrored ruleset, so the
// mirror is declared gone and the active node sends it again.
func (d *Daemon) filterReloadLocal() (string, error) {
	d.rs.set("")
	return sh("configctl", "filter", "reload")
}

func (d *Daemon) rulesetLoop() {
	for range time.Tick(time.Second) {
		d.rulesetStep()
	}
}

// readActiveRules returns the ruleset this (active) node has loaded; ok is false when it cannot be trusted to be what pf holds
// (a load that failed leaves an error file and pf on its previous ruleset) or to be what OPNsense wrote (a regular file owned by
// root that nobody else can write: /tmp is shared and a local user could have created the file before it was first generated).
func readActiveRules() ([]byte, bool) {
	if fi, err := os.Stat(localRulesPath + ".error"); err == nil && fi.Size() > 0 {
		return nil, false
	}
	f, err := os.Open(localRulesPath)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o022 != 0 || checkOwnerRoot(fi) != nil || fi.Size() == 0 || fi.Size() > rulesetMax {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(f, rulesetMax+1))
	if err != nil || len(b) == 0 || len(b) > rulesetMax {
		return nil, false
	}
	return b, true
}

// rulesetRetry is the pause after a send that failed or was refused: 3 s, doubling, at most 5 minutes (a refusal is repeated with a
// ruleset of up to 8 MiB: it must not go on every 3 s forever, for example towards a peer that does not run the hot mode).
func rulesetRetry(fails int) time.Duration {
	d := rulesetEvery
	for i := 1; i < fails && d < 5*time.Minute; i++ {
		d *= 2
	}
	return min(d, 5*time.Minute)
}

// rulesetStep runs on the active node: send the ruleset when the standby does not hold it.
func (d *Daemon) rulesetStep() {
	if !d.hotOn() {
		return
	}
	st := d.store.Get()
	if st.State != StateActive || st.PeerFP == "" {
		return
	}
	alive, _, peer := d.hb.Snapshot()
	if !alive || peer.State != StateStandby || peer.Warm == "" { // a peer that does not run the warm or hot mode takes no ruleset
		return
	}
	b, ok := readActiveRules()
	if !ok {
		return
	}
	sha := shaHex(b)
	d.rs.mu.Lock()
	if peer.Ruleset == sha {
		d.rs.fails = 0
		d.rs.mu.Unlock()
		return
	}
	recent := time.Now().Before(d.rs.nextTry) || (d.rs.sentSHA == sha && time.Since(d.rs.sentAt) < rulesetResend)
	if recent {
		d.rs.mu.Unlock()
		return
	}
	d.rs.sentSHA, d.rs.sentAt = sha, time.Now()
	d.rs.nextTry = time.Now().Add(rulesetEvery)
	d.rs.mu.Unlock()
	failed := func() {
		d.rs.mu.Lock()
		d.rs.fails++
		d.rs.nextTry = time.Now().Add(rulesetRetry(d.rs.fails))
		d.rs.sentSHA = "" // not accepted: the pause above decides when to try again
		d.rs.mu.Unlock()
	}
	cl := d.peerClient(st.PeerFP, nil)
	cl.Timeout = 15 * time.Second
	req, _ := http.NewRequest(http.MethodPost, "https://"+net.JoinHostPort(st.PeerAddr, fmt.Sprint(st.PeerPort))+"/v1/ruleset", bytes.NewReader(b))
	req.Header.Set("X-SHA256", sha)
	resp, err := cl.Do(req)
	if err != nil {
		log.Printf("ruleset mirror: %v", err)
		failed()
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Printf("ruleset mirror: the standby answered %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
		failed()
	}
}

var (
	rsHostIDRe = regexp.MustCompile(`^set hostid `)
	rsLimitRe  = regexp.MustCompile(`^set limit (\S+) `)
	// a ruleset of the peer must not make pfctl read other files of this node
	rsFileDirectiveRe = regexp.MustCompile(`(?m)^\s*(include\s|load\s+anchor)`)
)

// localizeRules replaces the node-specific `set` lines of the active node's ruleset with the ones of this node's own ruleset.
func localizeRules(rules, own []byte) ([]byte, error) {
	local := map[string]string{}
	for _, l := range strings.Split(string(own), "\n") {
		switch {
		case rsHostIDRe.MatchString(l):
			local["hostid"] = l
		case rsLimitRe.MatchString(l):
			local["limit "+rsLimitRe.FindStringSubmatch(l)[1]] = l
		}
	}
	if local["hostid"] == "" {
		return nil, errors.New("this node has no ruleset of its own to take its pf hostid from")
	}
	var out []string
	sawHostID := false
	for _, l := range strings.Split(string(rules), "\n") {
		switch {
		case rsHostIDRe.MatchString(l):
			out = append(out, local["hostid"])
			sawHostID = true
		case rsLimitRe.MatchString(l):
			key := "limit " + rsLimitRe.FindStringSubmatch(l)[1]
			if v, ok := local[key]; ok {
				out = append(out, v)
			} else {
				out = append(out, l)
			}
		default:
			out = append(out, l)
		}
	}
	if !sawHostID { // the active node's file has none: this node's own is added, never nothing (pf would pick a random one)
		out = append([]string{local["hostid"]}, out...)
	}
	return []byte(strings.Join(out, "\n")), nil
}

// validRuleset: what a ruleset must be before pfctl is even asked.
func validRuleset(b []byte) error {
	switch {
	case len(b) == 0 || len(b) > rulesetMax:
		return errors.New("empty or too large")
	case bytes.IndexByte(b, 0) >= 0:
		return errors.New("binary content")
	case rsFileDirectiveRe.Match(b):
		return errors.New("a directive that makes pfctl read another file on this node (include, load anchor)")
	case !pfDefaultDeny(string(b)):
		return errors.New("no default deny rule")
	}
	return nil
}

// handleRuleset runs on the standby (hot mode): load the ruleset of the active node.
func (d *Daemon) handleRuleset(w http.ResponseWriter, r *http.Request) {
	st := d.store.Get()
	if st.State != StateStandby || !d.hotOn() {
		http.Error(w, "not a hot standby", http.StatusConflict)
		return
	}
	d.rs.mu.Lock()
	blocked := time.Now().Before(d.rs.blocked)
	d.rs.mu.Unlock()
	if blocked {
		http.Error(w, "the ruleset mirror is off on this node", http.StatusConflict)
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, rulesetMax+1))
	if err != nil || len(b) > rulesetMax {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	sha := shaHex(b)
	if sha != r.Header.Get("X-SHA256") {
		http.Error(w, "checksum mismatch", http.StatusBadRequest)
		return
	}
	if err := validRuleset(b); err != nil {
		http.Error(w, "refused: "+err.Error(), http.StatusBadRequest)
		return
	}
	// the enforcer takes the operations lock every second for a moment: wait a little for it. A promotion or a demotion holds it
	// for seconds and decides the packet filter itself: then the active node simply tries again.
	locked := false
	for i := 0; i < 100 && !locked; i++ {
		if locked = d.ops.mu.TryLock(); !locked {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !locked {
		http.Error(w, "busy", http.StatusConflict)
		return
	}
	// the role may have changed while this waited (the old active node can still be sending during a switchover): an active node
	// never takes the ruleset of its peer
	if d.store.Get().State != StateStandby {
		d.ops.mu.Unlock()
		http.Error(w, "not a hot standby", http.StatusConflict)
		return
	}
	err = d.loadMirror(b, sha)
	d.ops.mu.Unlock() // the check that the peer is still heard does not need the lock (a promotion must not wait for it)
	if err == nil {
		err = d.mirrorSettle(sha)
	}
	if err != nil {
		log.Printf("ruleset mirror: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(200)
}

// loadMirror loads the ruleset (the caller holds the operations lock). The node holds the mirror only once mirrorSettle agrees.
func (d *Daemon) loadMirror(rules []byte, sha string) error {
	own, _ := os.ReadFile(localRulesPath)
	adapted, err := localizeRules(rules, own)
	if err != nil {
		return err
	}
	file := filepath.Join(d.store.dir, "ruleset.mirror")
	if err := writeFileDurable(file, adapted, 0o600); err != nil {
		return err
	}
	if out, err := d.run("pfctl", "-nf", file); err != nil {
		return fmt.Errorf("pfctl does not accept the ruleset of the active node here: %v: %s", err, strings.TrimSpace(out))
	}
	if out, err := d.run("pfctl", "-f", file); err != nil {
		d.filterReloadLocalVia(d.run) // the local ruleset replaces whatever was half loaded: the mirror is gone
		return fmt.Errorf("loading the ruleset: %v: %s", err, strings.TrimSpace(out))
	}
	if out, err := d.run("pfctl", "-si"); err != nil || !pfEnabled(out) {
		d.filterReloadLocalVia(d.run)
		return errors.New("pf is not enabled after loading the ruleset")
	}
	d.rs.set(sha)
	return nil
}

// mirrorSettle: the nodes must still hear each other with the ruleset that was just loaded; if not, the ruleset of the standby view
// is loaded again and the mirror stays off for a while.
func (d *Daemon) mirrorSettle(sha string) error {
	settle := rulesetSettle()
	time.Sleep(settle)
	if alive, ago, _ := d.hb.Snapshot(); alive && ago <= settle+500*time.Millisecond {
		return nil
	}
	// only while this is still a standby that no operation is working on: a promotion that started meanwhile decides the filter
	if !d.ops.mu.TryLock() {
		return nil
	}
	defer d.ops.mu.Unlock()
	if d.store.Get().State != StateStandby || d.rs.get() != sha {
		return nil
	}
	d.filterReloadLocalVia(d.run)
	d.rs.mu.Lock()
	d.rs.blocked = time.Now().Add(rulesetBackoff)
	d.rs.mu.Unlock()
	d.event("error", "ruleset mirror: the peer was no longer heard after loading the ruleset of the active node: the ruleset of the standby view is back and the mirror is off for %s", rulesetBackoff)
	return errors.New("the peer was no longer heard with this ruleset")
}

// rulesetStatus is shown in the status and the pre-flight.
func (d *Daemon) rulesetStatus() map[string]any {
	if !d.hotOn() {
		return nil
	}
	d.rs.mu.Lock()
	defer d.rs.mu.Unlock()
	out := map[string]any{"mirrored": d.rs.applied != ""}
	if d.rs.applied != "" {
		out["sha"] = d.rs.applied[:12]
	}
	if time.Now().Before(d.rs.blocked) {
		out["off_until"] = d.rs.blocked.UTC().Format(time.RFC3339)
	}
	return out
}

// rulesetChecks: the pre-flight view of the mirror.
func (d *Daemon) rulesetChecks() []Check {
	if !d.hotOn() {
		return nil
	}
	st := d.store.Get()
	if st.State == StateActive {
		_, _, peer := d.hb.Snapshot()
		b, ok := readActiveRules()
		switch {
		case !ok:
			return []Check{warn("standby ruleset", "this node's loaded ruleset cannot be read (a failed load leaves an error file): the standby is not mirrored")}
		case peer.Ruleset == shaHex(b):
			return []Check{pass("standby ruleset", "the standby holds the ruleset of this node: a new connection is judged by it at once after a takeover")}
		default:
			return []Check{warn("standby ruleset", "the standby does not hold the ruleset of this node yet (or refused it): a new connection waits for the ruleset of the takeover, about 1 s")}
		}
	}
	m := d.rulesetStatus()
	if m["off_until"] != nil {
		return []Check{warn("standby ruleset", fmt.Sprintf("the mirror is off until %v: the ruleset of the active node cut this node off from its peer", m["off_until"]))}
	}
	if m["mirrored"] == true {
		return []Check{pass("standby ruleset", "this standby holds the ruleset of the active node")}
	}
	return []Check{warn("standby ruleset", "this standby does not hold the ruleset of the active node yet")}
}

// filterReloadLocalVia is filterReloadLocal through a command runner.
func (d *Daemon) filterReloadLocalVia(run func(string, ...string) (string, error)) (string, error) {
	d.rs.set("")
	return run("configctl", "filter", "reload")
}
