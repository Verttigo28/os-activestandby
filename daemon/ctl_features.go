package main

import (
	"net/http"
	"strconv"
	"strings"
)

// control socket endpoints added with maintenance, health, metrics, history, pre-flight and the guided upgrade.

func (d *Daemon) registerCtl2(mux *http.ServeMux) {
	mux.HandleFunc("/maintenance", func(w http.ResponseWriter, r *http.Request) {
		if d.store.Get().Role == RoleStandalone {
			writeJSON(w, 200, map[string]string{"error": "node is not paired"})
			return
		}
		hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
		d.setMaintenance(r.URL.Query().Get("on") == "1", hours, false)
		writeJSON(w, 200, map[string]any{"status": "ok", "maintenance": d.maintenanceOn()})
	})
	mux.HandleFunc("/history", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"history": d.hist.Recent(50), "tz": serverZone()})
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		w.Write([]byte(d.metricsText()))
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		checks := d.localChecks()
		level := 0
		var worstMsgs []string
		for _, c := range checks {
			switch c.Status {
			case "fail":
				level = 2
				worstMsgs = append(worstMsgs, c.Name+": "+c.Detail)
			case "warn":
				if level < 1 {
					level = 1
				}
				worstMsgs = append(worstMsgs, c.Name+": "+c.Detail)
			}
		}
		summary := "ok"
		if len(worstMsgs) > 0 {
			summary = strings.Join(worstMsgs, "; ")
		}
		writeJSON(w, 200, map[string]any{"level": level, "summary": summary})
	})
	mux.HandleFunc("/preflight", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, d.preflight())
	})
	mux.HandleFunc("/plugin-sync", func(w http.ResponseWriter, r *http.Request) {
		// it takes minutes (the older node installs the plugin and restarts its daemon): started in the background, the result is an event
		if msg, err := d.startPluginSync(); err != nil {
			writeJSON(w, 200, map[string]string{"error": err.Error()})
		} else {
			writeJSON(w, 200, map[string]string{"status": "started", "message": msg})
		}
	})
	mux.HandleFunc("/upgrade/start", func(w http.ResponseWriter, r *http.Request) {
		if err := d.startUpgrade(r.URL.Query().Get("failback") == "true"); err != nil {
			writeJSON(w, 200, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "started"})
	})
	mux.HandleFunc("/upgrade/cancel", func(w http.ResponseWriter, r *http.Request) {
		d.cancelUpgrade()
		writeJSON(w, 200, map[string]string{"status": "cancelled"})
	})
	mux.HandleFunc("/upgrade/status", func(w http.ResponseWriter, r *http.Request) {
		// the job travels with the active role: show the most recently updated copy of either node
		job := d.store.Get().Job
		if d.store.Get().PeerFP != "" {
			if m, err := d.peerCall("/v1/job?get=1", 5*1e9); err == nil {
				if pj, ok := m["job"].(map[string]any); ok && pj != nil {
					if job == nil || strings.Compare(str(pj["updated"]), job.Updated) > 0 {
						writeJSON(w, 200, map[string]any{"job": pj})
						return
					}
				}
			}
		}
		writeJSON(w, 200, map[string]any{"job": job})
	})
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
