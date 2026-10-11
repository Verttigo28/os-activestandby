package main

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"time"
)

// Is there anything to update? OPNsense's own firmware check writes its result to /tmp/pkg_upgrade.json; the guided
// upgrade only makes sense when that result lists packages to upgrade, install or remove.

const pkgUpgradeJSON = "/tmp/pkg_upgrade.json"

type UpdateInfo struct {
	Available    bool     `json:"available"`
	Count        int      `json:"count"`
	Checked      bool     `json:"checked"`
	LastCheck    string   `json:"last_check,omitempty"`
	Connection   string   `json:"connection,omitempty"`
	NeedsReboot  bool     `json:"needs_reboot"`
	MajorVersion string   `json:"major_version,omitempty"`
	Packages     []string `json:"packages,omitempty"`
}

func loadUpdateInfo() UpdateInfo {
	var u UpdateInfo
	if fi, err := os.Lstat(pkgUpgradeJSON); err != nil || !fi.Mode().IsRegular() || fi.Size() > 1<<20 {
		return u // missing, a link, or implausibly large: nothing to trust
	}
	b, err := os.ReadFile(pkgUpgradeJSON)
	if err != nil {
		return u
	}
	var raw struct {
		Connection   string                  `json:"connection"`
		LastCheck    string                  `json:"last_check"`
		NeedsReboot  string                  `json:"needs_reboot"`
		MajorVersion string                  `json:"upgrade_major_version"`
		New          []struct{ Name string } `json:"new_packages"`
		Upgrade      []struct{ Name string } `json:"upgrade_packages"`
		Reinstall    []struct{ Name string } `json:"reinstall_packages"`
		Remove       []struct{ Name string } `json:"remove_packages"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return u
	}
	u.Checked, u.Connection, u.LastCheck, u.MajorVersion = true, raw.Connection, raw.LastCheck, raw.MajorVersion
	u.NeedsReboot = raw.NeedsReboot == "1"
	for _, l := range [][]struct{ Name string }{raw.New, raw.Upgrade, raw.Reinstall, raw.Remove} {
		for _, p := range l {
			u.Count++
			if len(u.Packages) < 12 {
				u.Packages = append(u.Packages, p.Name)
			}
		}
	}
	u.Available = u.Count > 0 || u.MajorVersion != ""
	return u
}

func (d *Daemon) registerUpdates(mux *http.ServeMux) {
	mux.HandleFunc("/updates", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, loadUpdateInfo())
	})
	mux.HandleFunc("/updates/check", func(w http.ResponseWriter, r *http.Request) {
		// OPNsense's own check (contacts the mirror, takes a few seconds); the result shows up in /updates
		err := exec.Command("configctl", "firmware", "check").Run()
		if err != nil {
			writeJSON(w, 200, map[string]string{"error": "could not start the update check: " + err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"status": "started", "at": time.Now().UTC().Format(time.RFC3339)})
	})
}
