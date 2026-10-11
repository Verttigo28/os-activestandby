package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
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

var notifySlots = make(chan struct{}, 8) // at most 8 notifications in flight: a flood of events cannot spawn unbounded processes

func (d *Daemon) notify(ev Event) {
	select {
	case notifySlots <- struct{}{}:
	default:
		return
	}
	defer func() { <-notifySlots }()
	prio := map[string]string{"error": "daemon.err", "warning": "daemon.warning", "change": "daemon.notice", "info": "daemon.info"}[ev.Kind]
	if prio == "" {
		prio = "daemon.info"
	}
	exec.Command("logger", "-t", "hasyncd", "-p", prio, "--", ev.Msg).Run()
	if d.cfg.NotifyWebhook != "" && kindRank(ev.Kind) >= levelRank(d.cfg.NotifyLevel) {
		d.postWebhook(ev)
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
	resp, err := webhookClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// webhookClient posts to the configured URL without following redirects and never to this host itself or to
// link-local addresses (the daemon runs as root next to services that trust 127.0.0.1; a webhook is an outbound call).
var webhookClient = &http.Client{
	Timeout:       5 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 4 * time.Second,
			Control: func(network, address string, _ syscall.RawConn) error {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return err
				}
				if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()) {
					return errors.New("webhook target is a loopback or link-local address")
				}
				return nil
			},
		}).DialContext,
	},
}
