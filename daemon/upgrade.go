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
		return errors.New("no updates available (last check " + u.LastCheck + "): nothing to do")
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
			if _, err := d.peerCall("/v1/update/start", 20*time.Second); err != nil {
				d.jobFail("could not start the update on the standby: " + err.Error())
				return
			}
			d.jobLog("update of the standby started (opnsense-update, pkg upgrade)")
			d.jobSet(j.Phase, "running")
			return
		}
		m, err := d.peerCall("/v1/update/status", 20*time.Second)
		if err != nil {
			return // the standby may be busy; keep waiting
		}
		if done, _ := m["done"].(bool); !done {
			return
		}
		if rc, _ := m["rc"].(float64); rc != 0 {
			d.jobFail(fmt.Sprintf("the update on the standby failed (rc %v): %v", rc, m["tail"]))
			return
		}
		d.jobLog("update finished on the standby")
		d.jobSet(strings.Replace(j.Phase, "update", "reboot", 1), "")
	case "r1-reboot", "r2-reboot":
		if j.Sub == "" {
			if _, err := d.peerCall("/v1/reboot", 10*time.Second); err != nil {
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
			if j.Phase == "r1-wait" {
				d.jobSet("failover", "")
			} else if j.Failback {
				d.jobSet("failback", "")
			} else {
				d.jobFinish()
			}
			return
		}
		if time.Since(parseTime(j.Updated)) > 15*time.Minute {
			d.jobFail("the standby did not come back within 15 minutes")
		}
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
		d.jobFinish()
	}
}

func (d *Daemon) jobFinish() {
	d.jobSet("done", "")
	d.jobLog("upgrade finished on both nodes")
	d.setMaintenance(false, 0, false)
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
	d.unlockedUntil.Set(time.Now().Add(60 * time.Minute)) // migrations must be able to write the config
	unlockConfig()
	proxy := "http://" + peerHostPort(st, proxyPort(d.cfg))
	go func() {
		out, err := execShell(
			"export HTTP_PROXY=" + proxy + " HTTPS_PROXY=" + proxy + "; (opnsense-update; echo \"opnsense-update rc=$?\"; pkg upgrade -y; echo \"pkg upgrade rc=$?\") > " + updateLog + " 2>&1; " +
				"tail -c 4000 " + updateLog + " | grep -q 'pkg upgrade rc=0'")
		_ = out
		d.upd.mu.Lock()
		d.upd.running, d.upd.done = false, true
		if err != nil {
			d.upd.rc = 1
		}
		d.upd.mu.Unlock()
		d.event("change", "software update on this standby finished (rc %d)", d.upd.rc)
	}()
	writeJSON(w, 200, map[string]string{"status": "started"})
}

func (d *Daemon) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	d.upd.mu.Lock()
	running, done, rc := d.upd.running, d.upd.done, d.upd.rc
	d.upd.mu.Unlock()
	tail := ""
	if b, err := os.ReadFile(updateLog); err == nil {
		s := string(b)
		if len(s) > 400 {
			s = s[len(s)-400:]
		}
		tail = s
	}
	writeJSON(w, 200, map[string]any{"running": running, "done": done, "rc": rc, "tail": tail})
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
