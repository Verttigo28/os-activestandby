package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const activeRules = `set limit table-entries 10000000
set limit states 1541200
set limit src-nodes 1541200
set hostid 0xd5ea2414

block drop in log inet all label "default"
block drop in log inet6 all label "default"
pass in quick on vlan0.65 inet from 192.168.65.0/24 to any keep state
`

const ownRules = `set limit table-entries 10000000
set limit states 1528200
set limit src-nodes 1528200
set hostid 0xe56dda91
block drop in log inet all
`

func TestLocalizeRulesKeepsThisNodeHostIDAndLimits(t *testing.T) {
	out, err := localizeRules([]byte(activeRules), []byte(ownRules))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "0xd5ea2414") || !strings.Contains(s, "set hostid 0xe56dda91") {
		t.Errorf("the pf hostid must be the one of this node (two nodes with one hostid collide on the pfsync state ids):\n%s", s)
	}
	if !strings.Contains(s, "set limit states 1528200") || strings.Contains(s, "1541200") {
		t.Errorf("the memory limits are the ones of this node:\n%s", s)
	}
	if !strings.Contains(s, "pass in quick on vlan0.65") || !strings.Contains(s, "set limit table-entries 10000000") {
		t.Errorf("everything else is untouched:\n%s", s)
	}
	if _, err := localizeRules([]byte(activeRules), []byte("block drop in log inet all\n")); err == nil {
		t.Error("a node without a hostid of its own must not load anything")
	}
	// an active ruleset with no hostid line gets the one of this node, never none
	out, _ = localizeRules([]byte("block drop in log inet all\n"), []byte(ownRules))
	if !strings.HasPrefix(string(out), "set hostid 0xe56dda91") {
		t.Errorf("a hostid is always set: %q", out)
	}
}

func TestValidRuleset(t *testing.T) {
	if err := validRuleset([]byte(activeRules)); err != nil {
		t.Errorf("a normal ruleset: %v", err)
	}
	for name, b := range map[string][]byte{
		"empty":           nil,
		"no default deny": []byte("pass in quick all\n"),
		"binary":          append([]byte(activeRules), 0),
		"too large":       bytes.Repeat([]byte("a"), rulesetMax+1),
		"only anti-spoof": []byte("block drop in log on ! igb0 inet from 10.0.0.0/24 to any\n"),
	} {
		if validRuleset(b) == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

type pfFake struct {
	mu   sync.Mutex
	cmds []string
	fail map[string]bool // command prefix -> fails
}

func (p *pfFake) sh(name string, args ...string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := name + " " + strings.Join(args, " ")
	p.cmds = append(p.cmds, c)
	for pre := range p.fail {
		if strings.HasPrefix(c, pre) {
			return "boom", os.ErrInvalid
		}
	}
	if name == "pfctl" && len(args) > 0 && args[0] == "-si" {
		return "Status: Enabled for 0 days 00:00:10           Debug: Urgent\n", nil
	}
	return "", nil
}

func (p *pfFake) has(prefix string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.cmds {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func hotStandbyForMirror(t *testing.T) (*Daemon, *pfFake) {
	t.Helper()
	old := rulesetSettle
	rulesetSettle = func() time.Duration { return 10 * time.Millisecond }
	t.Cleanup(func() { rulesetSettle = old })
	d := newHANode(t, standbyState(RoleStandby, 5))
	d.cfg.HAMode = "hot"
	pf := &pfFake{fail: map[string]bool{}}
	d.act.sh = pf.sh
	seePeer(d, hbMsg{State: StateActive, Epoch: 5})
	own := filepath.Join(t.TempDir(), "rules.debug")
	if err := os.WriteFile(own, []byte(ownRules), 0o600); err != nil {
		t.Fatal(err)
	}
	oldPath := localRulesPath
	localRulesPath = own
	t.Cleanup(func() { localRulesPath = oldPath })
	return d, pf
}

func postRules(d *Daemon, body string, sha string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/ruleset", strings.NewReader(body))
	r.Header.Set("X-SHA256", sha)
	w := httptest.NewRecorder()
	d.handleRuleset(w, r)
	return w
}

func TestMirrorLoadsTheActiveRulesetAfterChecking(t *testing.T) {
	d, pf := hotStandbyForMirror(t)
	w := postRules(d, activeRules, shaHex([]byte(activeRules)))
	if w.Code != 200 {
		t.Fatalf("code %d: %s", w.Code, w.Body.String())
	}
	if !pf.has("pfctl -nf") || !pf.has("pfctl -f") {
		t.Fatalf("syntax check, then load: %v", pf.cmds)
	}
	if i, j := indexOf(pf.cmds, "pfctl -nf"), indexOf(pf.cmds, "pfctl -f"); i > j {
		t.Errorf("the syntax check comes first: %v", pf.cmds)
	}
	if d.rs.get() != shaHex([]byte(activeRules)) || d.rulesetHB(d.store.Get()) != d.rs.get() {
		t.Error("the standby must report the hash of what it holds")
	}
	loaded, _ := os.ReadFile(filepath.Join(d.store.dir, "ruleset.mirror"))
	if !strings.Contains(string(loaded), "0xe56dda91") {
		t.Errorf("the loaded file carries the hostid of this node: %s", loaded)
	}
	// a local filter reload replaces the mirror: it must be declared gone so that the active node sends it again
	d.filterReloadLocal()
	if d.rs.get() != "" || d.rulesetHB(d.store.Get()) != "" {
		t.Error("after a local reload the standby no longer holds the mirror")
	}
}

func TestMirrorRefusals(t *testing.T) {
	d, pf := hotStandbyForMirror(t)
	if w := postRules(d, activeRules, "deadbeef"); w.Code != 400 {
		t.Errorf("wrong checksum: %d", w.Code)
	}
	if w := postRules(d, "pass in all\n", shaHex([]byte("pass in all\n"))); w.Code != 400 {
		t.Errorf("no default deny: %d", w.Code)
	}
	if pf.has("pfctl -f") {
		t.Error("nothing refused may reach pf")
	}
	pf.fail["pfctl -nf"] = true
	if w := postRules(d, activeRules, shaHex([]byte(activeRules))); w.Code != 500 || pf.has("pfctl -f") {
		t.Errorf("a ruleset pfctl does not accept is not loaded: %d %v", w.Code, pf.cmds)
	}
	delete(pf.fail, "pfctl -nf")
	d.cfg.HAMode = "classic"
	if w := postRules(d, activeRules, shaHex([]byte(activeRules))); w.Code != 409 {
		t.Errorf("only a hot standby takes a ruleset: %d", w.Code)
	}
	d.cfg.HAMode = "hot"
	d.store.Update(func(s *State) { s.State = StateActive })
	if w := postRules(d, activeRules, shaHex([]byte(activeRules))); w.Code != 409 {
		t.Errorf("an active node never takes a ruleset: %d", w.Code)
	}
}

func TestMirrorBusyAndFailedLoadFallBack(t *testing.T) {
	d, pf := hotStandbyForMirror(t)
	d.ops.mu.Lock() // a promotion is running (it holds the lock for seconds)
	if w := postRules(d, activeRules, shaHex([]byte(activeRules))); w.Code != 409 || pf.has("pfctl -f") {
		t.Errorf("nothing is loaded during a promotion: %d", w.Code)
	}
	d.ops.mu.Unlock()
	pf.fail["pfctl -f"] = true
	if w := postRules(d, activeRules, shaHex([]byte(activeRules))); w.Code != 500 || !pf.has("configctl filter reload") {
		t.Errorf("a failed load puts the local ruleset back: %d %v", w.Code, pf.cmds)
	}
}

// a ruleset that cuts the nodes off each other must not stay
func TestMirrorRollsBackWhenThePeerIsNoLongerHeard(t *testing.T) {
	d, pf := hotStandbyForMirror(t)
	silence(d, hbDead+time.Second) // no heartbeat any more
	if w := postRules(d, activeRules, shaHex([]byte(activeRules))); w.Code != 500 {
		t.Fatalf("code %d", w.Code)
	}
	if !pf.has("configctl filter reload") || d.rs.get() != "" {
		t.Errorf("the ruleset of the standby view must be back: %v", pf.cmds)
	}
	if w := postRules(d, activeRules, shaHex([]byte(activeRules))); w.Code != 409 {
		t.Errorf("the mirror stays off for a while: %d", w.Code)
	}
	if m := d.rulesetStatus(); m["off_until"] == nil {
		t.Error("the status says it is off")
	}
}

// found by the review: the role is checked again once the lock is obtained
func TestMirrorNeverLoadsIntoANodeThatBecameActive(t *testing.T) {
	d, pf := hotStandbyForMirror(t)
	d.ops.mu.Lock() // the promotion of this node is running ...
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- postRules(d, activeRules, shaHex([]byte(activeRules))) }()
	time.Sleep(60 * time.Millisecond)
	d.store.Update(func(s *State) { s.State = StateActive }) // ... and ends with this node active
	d.ops.mu.Unlock()
	if w := <-done; w.Code != 409 || pf.has("pfctl -f") {
		t.Fatalf("the ruleset of the old active node must not replace the one of the new: %d %v", w.Code, pf.cmds)
	}
}

func TestMirrorSendBackoff(t *testing.T) {
	want := []time.Duration{3 * time.Second, 3 * time.Second, 6 * time.Second, 12 * time.Second, 24 * time.Second}
	for i, w := range want {
		if got := rulesetRetry(i); got != w {
			t.Errorf("after %d failures: %v, want %v", i, got, w)
		}
	}
	if got := rulesetRetry(40); got != 5*time.Minute {
		t.Errorf("at most 5 minutes: %v", got)
	}
}

func TestReadActiveRulesRefusesWhatIsNotRootsOwn(t *testing.T) {
	dir := t.TempDir()
	old := localRulesPath
	localRulesPath = filepath.Join(dir, "rules.debug")
	t.Cleanup(func() { localRulesPath = old })
	if err := os.WriteFile(localRulesPath, []byte(activeRules), 0o666); err != nil {
		t.Fatal(err)
	}
	os.Chmod(localRulesPath, 0o666) // the umask may have cleared it
	if _, ok := readActiveRules(); ok {
		t.Error("a world-writable file is not what OPNsense wrote")
	}
	os.Chmod(localRulesPath, 0o600)
	if runtime.GOOS != "js" && runtime.GOOS != "windows" && os.Getuid() == 0 {
		if _, ok := readActiveRules(); !ok {
			t.Error("a root-owned 0600 file is read")
		}
	}
	os.WriteFile(localRulesPath+".error", []byte("syntax error"), 0o600)
	if _, ok := readActiveRules(); ok {
		t.Error("a load that failed leaves an error file: nothing is sent")
	}
}
