package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"time"
)

// Notifications. Every event goes to syslog (tag hasyncd). At or above the configured level it is also POSTed as
// JSON to the webhook. E-mail is deliberately not built in: OPNsense 26.7 mails through Monit, and `hasyncd ctl health`
// (exit status 0/1/2) plus scripts/.../monit_check.sh let Monit alert on the HA state with its own SMTP settings.

func kindRank(k string) int {
	switch k {
	case "error":
		return 2
	case "warning", "change":
		return 1
	}
	return 0
}

func levelRank(l string) int {
	switch l {
	case "info":
		return 0
	case "error":
		return 2
	}
	return 1 // warning (default): changes, warnings, errors
}

func (d *Daemon) notify(ev Event) {
	prio := map[string]string{"error": "daemon.err", "warning": "daemon.warning", "change": "daemon.notice", "info": "daemon.info"}[ev.Kind]
	if prio == "" {
		prio = "daemon.info"
	}
	go exec.Command("logger", "-t", "hasyncd", "-p", prio, "--", ev.Msg).Run()
	if d.cfg.NotifyWebhook != "" && kindRank(ev.Kind) >= levelRank(d.cfg.NotifyLevel) {
		go d.postWebhook(ev)
	}
}

func (d *Daemon) postWebhook(ev Event) {
	host, _ := os.Hostname()
	st := d.store.Get()
	body, _ := json.Marshal(map[string]string{
		"time": ev.Time, "kind": ev.Kind, "node": host, "role": st.Role, "state": st.State, "message": ev.Msg,
	})
	req, err := http.NewRequest(http.MethodPost, d.cfg.NotifyWebhook, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "hasyncd/"+version)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}
