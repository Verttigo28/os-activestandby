package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Guided upgrade of the pair. The job runs on whichever node is ACTIVE, one phase at a time, and is persisted so it
// survives reboots and the hand-over:
//   r1-update   ask the standby to update itself (through this node's proxy)      -> r1-reboot -> r1-wait
//   failover    graceful, guarded switchover to the updated node (job travels along)
//   r2-update   the new active node updates the old one                           -> r2-reboot -> r2-wait
//   failback    optional: give the active role back to the original node
//   done        maintenance mode off
// A failure stops the job and leaves maintenance mode ON for the operator. Everything the standby is asked to do is only
// accepted from the paired peer while maintenance mode is on.

type Job struct {
	ID        string   `json:"id"`
	Phase     string   `json:"phase"`
	Sub       string   `json:"sub,omitempty"`
	Failback  bool     `json:"failback"`
	Origin    string   `json:"origin"` // role (master/standby) of the node that was active when the job started
	Started   string   `json:"started"`
	Updated   string   `json:"updated"`
	Err       string   `json:"err,omitempty"`
	WaitSince string   `json:"wait_since,omitempty"`
	Log       []string `json:"log"`
}

func (j *Job) terminal() bool {
	// r2-handed-over: the job moved to the other node, which finishes it; here it is over
	return j == nil || j.Phase == "done" || j.Phase == "failed" || j.Phase == "cancelled" || j.Phase == "r2-handed-over"
}

func (d *Daemon) jobLog(msg string) {
	d.store.Update(func(s *State) {
		if s.Job != nil {
			s.Job.Log = append(s.Job.Log, time.Now().UTC().Format("15:04:05")+" "+msg)
			if len(s.Job.Log) > 60 {
				s.Job.Log = s.Job.Log[len(s.Job.Log)-60:]
			}
			s.Job.Updated = time.Now().UTC().Format(time.RFC3339)
		}
	})
	d.event("info", "upgrade: %s", msg)
}

func (d *Daemon) jobSet(phase, sub string) {
	d.store.Update(func(s *State) {
		if s.Job != nil {
			s.Job.Phase, s.Job.Sub, s.Job.WaitSince = phase, sub, ""
			s.Job.Updated = time.Now().UTC().Format(time.RFC3339)
		}
	})
}

func (d *Daemon) jobFail(msg string) {
	d.store.Update(func(s *State) {
		if s.Job != nil {
			s.Job.Phase, s.Job.Err = "failed", msg
			s.Job.Updated = time.Now().UTC().Format(time.RFC3339)
		}
	})
	d.event("error", "upgrade stopped: %s (maintenance mode stays on)", msg)
}

// startUpgrade begins the guided procedure on the active node.
func (d *Daemon) startUpgrade(failback bool) error {
	st := d.store.Get()
	if st.PeerFP == "" || st.State != StateActive {
		return errors.New("start the upgrade on the active node of a paired cluster")
	}
	if !st.Job.terminal() {
		return errors.New("an upgrade is already running")
	}
	if u := loadUpdateInfo(); !u.Available {
		if !u.Checked {
			return errors.New("update status unknown: run \"Check for updates\" first")
		}
		msg := "no updates available (last check " + u.LastCheck + "): nothing to do"
		if a, _, p := d.hb.Snapshot(); a && p.Plug != "" && p.Plug != d.pluginVersion() {
			msg += "; the plugin versions differ (this node " + d.pluginVersion() + ", the peer " + p.Plug + "): use \"Synchronize plugin version\""
		}
		return errors.New(msg)
	}
	pf := d.preflight()
	if pf["summary"] == "fail" {
		return errors.New("pre-flight reported failures: fix them first (see Pre-flight)")
	}
	d.setMaintenance(true, 4, false)
	now := time.Now().UTC().Format(time.RFC3339)
	d.store.Update(func(s *State) {
		s.Job = &Job{ID: fmt.Sprint(time.Now().Unix()), Phase: "r1-update", Failback: failback, Origin: s.Role, Started: now, Updated: now}
	})
	d.jobLog("started (failback after the update: " + fmt.Sprint(failback) + ")")
	return nil
}

func (d *Daemon) cancelUpgrade() {
	d.store.Update(func(s *State) {
		if s.Job != nil && !s.Job.terminal() {
			s.Job.Phase = "cancelled"
			s.Job.Updated = time.Now().UTC().Format(time.RFC3339)
		}
	})
	d.event("warning", "upgrade cancelled by the operator (maintenance mode stays on)")
}

// jobLoop drives the job; only the active node does it.
func (d *Daemon) jobLoop() {
	for range time.Tick(3 * time.Second) {
		d.jobStep()
	}
}

func (d *Daemon) jobStep() {
	st := d.store.Get()
	j := st.Job
	if j.terminal() || st.State != StateActive || st.PeerFP == "" {
		return
	}
	alive, _, peer := d.hb.Snapshot()
	switch j.Phase {
	case "r1-update", "r2-update":
		if j.Sub == "" {
			if !alive || peer.State != StateStandby {
				// right after a failover the old active node may still be finishing its demote: give it a minute
				if time.Since(parseTime(j.Updated)) < time.Minute {
					return
				}
				d.jobFail("the standby is not reachable or not standby")
				return
			}
			if _, err := d.callPeer("/v1/update/start", 20*time.Second); err != nil {
				d.jobFail("could not start the update on the standby: " + err.Error())
				return
			}
			d.jobLog("update of the standby started (opnsense-update, pkg upgrade)")
			d.jobSet(j.Phase, "running")
			return
		}
		m, err := d.callPeer("/v1/update/status", 20*time.Second)
		if err != nil {
			// The update of OPNsense itself reboots the node when it replaced the kernel or the base system (the firmware script does
			// it, as the GUI's update does): the silence of the standby is then the expected end of the update, not a failure.
			if !alive && d.hb.silentFor() > 15*time.Second {
				d.jobLog("the standby went down: the update replaced the kernel or the base system and rebooted it")
				d.jobSet(strings.Replace(j.Phase, "update", "wait", 1), "")
				return
			}
			if time.Since(parseTime(j.Updated)) > updateTimeout+10*time.Minute {
				d.jobFail("the standby did not report on its update in time: " + err.Error())
			}
			return // the standby may be busy; keep waiting
		}
		if done, _ := m["done"].(bool); !done {
			if time.Since(parseTime(j.Updated)) > updateTimeout+10*time.Minute {
				d.jobFail("the update on the standby did not finish in time")
			}
			return
		}
		if rc, _ := m["rc"].(float64); rc != 0 {
			d.jobFail(fmt.Sprintf("the update on the standby failed (rc %v): %v", rc, m["tail"]))
			return
		}
		if reboot, _ := m["reboot"].(bool); reboot {
			d.jobLog("update finished on the standby, which reboots by itself (kernel or base system replaced)")
			d.jobSet(strings.Replace(j.Phase, "update", "wait", 1), "")
			return
		}
		d.jobLog("update finished on the standby")
		d.jobSet(strings.Replace(j.Phase, "update", "reboot", 1), "")
	case "r1-reboot", "r2-reboot":
		if j.Sub == "" {
			if _, err := d.callPeer("/v1/reboot", 10*time.Second); err != nil {
				d.jobFail("could not reboot the standby: " + err.Error())
				return
			}
			d.jobLog("rebooting the standby")
			d.jobSet(j.Phase, "wait-down")
			return
		}
		if !alive { // it went down
			d.jobSet(strings.Replace(j.Phase, "reboot", "wait", 1), "")
			return
		}
		if time.Since(parseTime(j.Updated)) > 2*time.Minute {
			d.jobFail("the standby did not go down after the reboot request")
		}
	case "r1-wait", "r2-wait":
		ok := alive && peer.State == StateStandby
		d.store.Update(func(s *State) {
			if s.Job == nil {
				return
			}
			if ok && s.Job.WaitSince == "" {
				s.Job.WaitSince = time.Now().UTC().Format(time.RFC3339)
			}
			if !ok {
				s.Job.WaitSince = ""
			}
		})
		j = d.store.Get().Job
		if ok && j.WaitSince != "" && time.Since(parseTime(j.WaitSince)) > 20*time.Second {
			d.jobLog("the standby is back (" + peer.Ver + ")")
			// never "done" on the word of a log: ask the standby what it still has to update
			d.jobSet(strings.Replace(j.Phase, "wait", "verify", 1), "")
			return
		}
		if time.Since(parseTime(j.Updated)) > 15*time.Minute {
			d.jobFail("the standby did not come back within 15 minutes")
		}
	case "r1-verify", "r2-verify":
		if !alive || peer.State != StateStandby {
			if time.Since(parseTime(j.Updated)) > 15*time.Minute {
				d.jobFail("the standby is not available to verify its update")
			}
			return
		}
		pending, err := d.verifyPeerUpdate()
		if err != nil {
			if time.Since(parseTime(j.Updated)) > 15*time.Minute {
				d.jobFail("the standby could not verify its update: " + err.Error())
			}
			return // tried again at the next step
		}
		if len(pending) > 0 {
			d.jobFail("the update did not complete on the standby, still pending: " + strings.Join(pending, ", ") + " (the log of the update is on the standby: /conf/activestandby/update.log)")
			return
		}
		d.jobLog("verified: nothing is left to update on the standby")
		if j.Phase == "r1-verify" {
			d.jobSet("failover", "")
		} else {
			d.jobFinish()
		}
	case "plugin":
		d.pluginStep(j, alive, peer)
	case "failover":
		d.jobLog("failing over to the updated node")
		// the job moves with the active role: the peer continues with round 2
		j2 := *j
		j2.Phase, j2.Sub, j2.WaitSince = "r2-update", "", ""
		j2.Log = append(append([]string{}, j.Log...), time.Now().UTC().Format("15:04:05")+" failover done, continuing on the new active node")
		j2.Updated = time.Now().UTC().Format(time.RFC3339)
		if _, err := d.peerPostJSON("/v1/job", j2); err != nil {
			d.jobFail("could not hand the job to the peer: " + err.Error())
			return
		}
		if _, err := d.failover(false); err != nil {
			d.peerPostJSON("/v1/job?cancel=1", j2) // the peer must not continue
			d.jobFail("failover refused: " + err.Error())
			return
		}
		d.jobSet("r2-handed-over", "") // informational on this node; the peer drives the rest
	case "failback":
		d.jobLog("giving the active role back")
		if _, err := d.failover(false); err != nil {
			d.jobFail("failback refused: " + err.Error())
			return
		}
		d.jobDone()
	}
}

// jobFinish: both nodes are updated; the last step makes sure they run the same version of this plugin (it is not in the OPNsense
// repositories, so the firmware update never touches it). It runs on the active node, before the optional failback: a node that has
// just given the role back does not drive the job any more.
func (d *Daemon) jobFinish() {
	d.jobSet("plugin", "")
}

// pluginStep waits for the plugin versions of both nodes to be equal, starting the synchronisation if they are not.
func (d *Daemon) pluginStep(j *Job, alive bool, peer hbMsg) {
	if !alive {
		if time.Since(parseTime(j.Updated)) > 15*time.Minute {
			d.jobFail("the peer is not reachable to compare the plugin versions")
		}
		return
	}
	if peer.Plug == d.pluginVersion() {
		if j.Failback {
			d.jobSet("failback", "")
		} else {
			d.jobDone()
		}
		return
	}
	if j.Sub == "" {
		d.jobLog(fmt.Sprintf("the plugin versions differ (this node %s, the peer %s): synchronising", d.pluginVersion(), peer.Plug))
		d.jobSet("plugin", "syncing")
		if msg, err := d.startPluginSync(); err != nil {
			d.jobFail("plugin synchronisation: " + err.Error())
		} else {
			d.jobLog(msg)
		}
		return
	}
	if time.Since(parseTime(j.Updated)) > 10*time.Minute {
		d.jobFail(fmt.Sprintf("the plugin versions are still different (this node %s, the peer %s)", d.pluginVersion(), peer.Plug))
	}
}

// jobDone ends the job: both nodes are updated and run the same plugin.
func (d *Daemon) jobDone() {
	d.jobSet("done", "")
	d.jobLog("upgrade finished on both nodes (same OPNsense and plugin versions)")
	d.setMaintenance(false, 0, false)
}

// verifyPeerUpdate asks the standby to look again for updates (through this node's proxy) and returns what is still pending.
func (d *Daemon) verifyPeerUpdate() ([]string, error) {
	if d.act != nil && d.act.verify != nil {
		return d.act.verify()
	}
	st := d.store.Get()
	cl := d.peerClient(st.PeerFP, nil)
	cl.Timeout = 6 * time.Minute
	resp, err := cl.Post("https://"+net.JoinHostPort(d.peerEndpoint(st), fmt.Sprint(st.PeerPort))+"/v1/update/status?verify=1", "application/json", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Checked bool     `json:"checked"`
		Pending []string `json:"pending"`
		Error   string   `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil || resp.StatusCode != 200 {
		return nil, fmt.Errorf("the standby answered %d", resp.StatusCode)
	}
	if out.Error != "" {
		return nil, errors.New(out.Error)
	}
	if !out.Checked {
		return nil, errors.New("the standby could not check for updates")
	}
	return out.Pending, nil
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func (d *Daemon) peerPostJSON(path string, v any) (map[string]any, error) {
	st := d.store.Get()
	b, _ := json.Marshal(v)
	cl := d.peerClient(st.PeerFP, nil)
	cl.Timeout = 15 * time.Second
	resp, err := cl.Post("https://"+net.JoinHostPort(d.peerEndpoint(st), fmt.Sprint(st.PeerPort))+path, "application/json", strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out)
	if resp.StatusCode != 200 {
		return out, fmt.Errorf("peer answered %d: %v", resp.StatusCode, out["error"])
	}
	return out, nil
}

// handleJobAdopt: the peer hands its job to this node (or asks to cancel its copy / reads it).
func (d *Daemon) handleJobAdopt(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("get") == "1" {
		writeJSON(w, 200, map[string]any{"job": d.store.Get().Job})
		return
	}
	var j Job
	if json.NewDecoder(io.LimitReader(r.Body, 1<<18)).Decode(&j) != nil || j.ID == "" {
		http.Error(w, "bad job", 400)
		return
	}
	if r.URL.Query().Get("cancel") == "1" {
		d.store.Update(func(s *State) {
			if s.Job != nil && s.Job.ID == j.ID {
				s.Job.Phase = "cancelled"
			}
		})
	} else {
		// the only legitimate hand-off: round 2 of an upgrade, delivered to the node that is about to become active
		if j.Phase != "r2-update" || d.store.Get().State != StateStandby {
			http.Error(w, "job not accepted", http.StatusBadRequest)
			return
		}
		d.store.Update(func(s *State) { s.Job = &j })
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// ---- standby side primitives (only for the paired peer, only in maintenance mode) ----

type updateRun struct {
	mu      sync.Mutex
	running bool
	done    bool
	rc      int
	reboot  bool // the update replaced the kernel or the base system and reboots the node itself
}

// kept in the root-only state directory: a fixed name in /tmp could be pre-created as a symlink by a local user
const updateLog = "/conf/activestandby/update.log"

func (d *Daemon) handleUpdateStart(w http.ResponseWriter, r *http.Request) {
	st := d.store.Get()
	if st.State != StateStandby || !d.maintenanceOn() {
		http.Error(w, "refused: only a standby in maintenance mode may be updated", http.StatusConflict)
		return
	}
	d.upd.mu.Lock()
	if d.upd.running {
		d.upd.mu.Unlock()
		http.Error(w, "an update is already running", http.StatusConflict)
		return
	}
	d.upd.running, d.upd.done, d.upd.rc = true, false, 0
	d.upd.mu.Unlock()
	d.setUnlocked(time.Now().Add(60 * time.Minute)) // migrations must be able to write the config
	unlockConfig()
	proxy := "http://" + peerHostPort(st, proxyPort(d.cfg))
	// The update that the firmware page of OPNsense runs (launcher.sh update: the packages, then the base system and the kernel, and a
	// reboot when it replaced the kernel or the base), with the proxy of the active node in its environment. The sequence this daemon
	// used before (opnsense-update, then pkg upgrade) never replaced the base system and the kernel, and reported success.
	started := time.Now()
	os.WriteFile(updateStarted, []byte(started.UTC().Format(time.RFC3339)), 0o600)
	go func() {
		_, err := execShellFor(updateTimeout, updateScript(proxy))
		done, ok, reboot := updateOutcome(readProgress(started))
		d.upd.mu.Lock()
		d.upd.running, d.upd.done, d.upd.reboot = false, true, reboot
		if err != nil || !done || !ok {
			d.upd.rc = 1
		}
		d.upd.mu.Unlock()
		d.event("change", "software update on this standby finished (rc %d, reboot %v)", d.upd.rc, reboot)
	}()
	writeJSON(w, 200, map[string]string{"status": "started"})
}

const (
	firmwareProgress = "/tmp/pkg_upgrade.progress"          // written by the firmware scripts of OPNsense (the page of the GUI reads it)
	updateStarted    = "/conf/activestandby/update.started" // its modification time: when this daemon started the update
)

// updateScript is the update that the firmware page of OPNsense runs, with the proxy of the active node (the standby has no WAN).
func updateScript(proxy string) string {
	return "export HTTP_PROXY=" + proxy + " HTTPS_PROXY=" + proxy + "; /usr/local/opnsense/scripts/firmware/launcher.sh update > " + updateLog + " 2>&1"
}

// updateOutcome reads the progress file of the firmware script: done when it wrote its end marker, ok when nothing went wrong
// ("Partial update failure" is what update.sh writes when the packages could not be updated), reboot when the node reboots by itself.
// The progress of another request (a check) or of nothing is not the progress of an update.
func updateOutcome(progress string) (done, ok, reboot bool) {
	if !strings.Contains(progress, "***GOT REQUEST TO UPDATE***") {
		return false, false, false
	}
	reboot = strings.Contains(progress, "***REBOOT***")
	done = reboot || strings.Contains(progress, "***DONE***")
	ok = done && !strings.Contains(progress, "Partial update failure") && !strings.Contains(progress, "safety violation")
	return
}

// readProgress returns the progress file when it is the one of an update started at or after since: a file from before (or none) means
// that the firmware script did not run (it exits silently when another update holds its lock), and it must not be taken for ours.
func readProgress(since time.Time) string {
	fi, err := os.Lstat(firmwareProgress)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 4<<20 || fi.Mode().Perm()&0o022 != 0 || checkOwnerRoot(fi) != nil {
		return ""
	}
	if fi.ModTime().Before(since.Add(-2 * time.Second)) {
		return ""
	}
	b, _ := os.ReadFile(firmwareProgress)
	return string(b)
}

// updateFromProgress: the state of an update that this daemon did not start itself (it was restarted while the update ran: the upgrade of
// this plugin restarts it). The update went on; its progress file says how far it is. started is when it was started (zero: never).
func updateFromProgress(progress string, started time.Time) (running, done bool, rc int, reboot bool) {
	if started.IsZero() || progress == "" {
		return false, false, 0, false
	}
	done, ok, reboot := updateOutcome(progress)
	switch {
	case !done && strings.Contains(progress, "***GOT REQUEST TO UPDATE***"):
		return true, false, 0, false
	case done && ok:
		return false, true, 0, reboot
	case done:
		return false, true, 1, reboot
	}
	return false, false, 0, false
}

func (d *Daemon) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("verify") == "1" {
		d.handleUpdateVerify(w)
		return
	}
	d.upd.mu.Lock()
	running, done, rc, reboot := d.upd.running, d.upd.done, d.upd.rc, d.upd.reboot
	d.upd.mu.Unlock()
	tail := ""
	if b, err := os.ReadFile(updateLog); err == nil {
		tail = string(b)
		if len(tail) > 400 {
			tail = tail[len(tail)-400:]
		}
	}
	if !running && !done {
		var started time.Time
		if fi, err := os.Stat(updateStarted); err == nil {
			started = fi.ModTime()
		}
		running, done, rc, reboot = updateFromProgress(readProgress(started), started)
	}
	writeJSON(w, 200, map[string]any{"running": running, "done": done, "rc": rc, "reboot": reboot, "tail": tail})
}

// handleUpdateVerify: the standby looks for updates again (the check of the firmware page, through the proxy of the active node) and
// says what is still pending. The guided upgrade is over only when this answer is empty: never on the word of a log.
func (d *Daemon) handleUpdateVerify(w http.ResponseWriter) {
	st := d.store.Get()
	if st.State != StateStandby || !d.maintenanceOn() {
		http.Error(w, "refused: only a standby in maintenance mode may be checked", http.StatusConflict)
		return
	}
	if d.updateInProgress() {
		http.Error(w, "an update is running", http.StatusConflict)
		return
	}
	proxy := "http://" + peerHostPort(st, proxyPort(d.cfg))
	_, err := execShellFor(4*time.Minute, "export HTTP_PROXY="+proxy+" HTTPS_PROXY="+proxy+"; /usr/local/opnsense/scripts/firmware/launcher.sh check > /dev/null 2>&1")
	pending, checked, perr := pendingUpdates(loadUpdateInfo())
	out := map[string]any{"checked": checked, "pending": pending}
	if perr != "" {
		out["error"] = perr
	} else if err != nil && !checked {
		out["error"] = "the update check did not complete: " + err.Error()
	}
	writeJSON(w, 200, out)
}

// pendingUpdates turns the result of a check into what is left to do. A check that did not reach the repository is not "nothing
// pending": it is an error.
func pendingUpdates(u UpdateInfo) (pending []string, checked bool, problem string) {
	if !u.Checked {
		return nil, false, "the check left no result"
	}
	if u.Connection != "" && u.Connection != "ok" {
		return nil, false, "the standby could not reach the repository (" + u.Connection + ")"
	}
	pending = append(pending, u.Packages...)
	if u.Count > len(pending) {
		pending = append(pending, fmt.Sprintf("and %d more", u.Count-len(pending)))
	}
	if u.MajorVersion != "" {
		pending = append(pending, "major upgrade to "+u.MajorVersion)
	}
	return pending, true, ""
}

func (d *Daemon) handleReboot(w http.ResponseWriter, r *http.Request) {
	if d.store.Get().State != StateStandby || !d.maintenanceOn() {
		http.Error(w, "refused: only a standby in maintenance mode may be rebooted", http.StatusConflict)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "rebooting"})
	go func() {
		sh("sync")
		time.Sleep(3 * time.Second)
		sh("shutdown", "-r", "now")
	}()
}

// updateInProgress: the software update that the guided upgrade runs on this node has not ended.
func (d *Daemon) updateInProgress() bool {
	if d.upd == nil {
		return false
	}
	d.upd.mu.Lock()
	defer d.upd.mu.Unlock()
	return d.upd.running
}

// readSmall reads a small file that may not exist.
func readSmall(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}
