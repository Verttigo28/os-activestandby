package main

import (
	"fmt"
	"os"
	"strings"
)

// metricsText renders the node state in the Prometheus text exposition format
// (served through the GUI API so authentication and TLS come from OPNsense).
func (d *Daemon) metricsText() string {
	st := d.store.Get()
	alive, ago, peer := d.hb.Snapshot()
	var b strings.Builder
	g := func(name, help string, val any, labels ...string) {
		l := ""
		if len(labels) > 0 {
			l = "{" + strings.Join(labels, ",") + "}"
		}
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s%s %v\n", name, help, name, name, l, val)
	}
	bi := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	g("activestandby_info", "Static information about this node.", 1,
		fmt.Sprintf(`version="%s"`, version), fmt.Sprintf(`role="%s"`, st.Role), fmt.Sprintf(`os="%s"`, d.localVer))
	g("activestandby_active", "1 when this node is the active node.", bi(st.State == StateActive))
	g("activestandby_maintenance", "1 while maintenance mode pauses automatic failover.", bi(st.Maintenance))
	g("activestandby_epoch", "Role-change epoch (higher wins a split-brain).", st.Epoch)
	g("activestandby_peer_alive", "1 when the peer answers heartbeats on any path.", bi(alive))
	g("activestandby_peer_active", "1 when the peer reports itself active.", bi(peer.State == StateActive))
	g("activestandby_heartbeat_age_seconds", "Seconds since the last heartbeat from the peer.", fmt.Sprintf("%.3f", ago.Seconds()))
	for _, p := range []string{"ha", "mgmt"} {
		g("activestandby_path_up", "1 when the heartbeat path is alive.", bi(d.hb.PathAlive(p)), fmt.Sprintf(`path="%s"`, p))
		g("activestandby_heartbeat_rtt_seconds", "Last heartbeat round-trip time per path.", fmt.Sprintf("%.6f", d.hb.RTT(p).Seconds()), fmt.Sprintf(`path="%s"`, p))
	}
	sync := d.syncStatus(st)
	g("activestandby_sync_revision", "Current config sync revision.", st.SyncRev)
	g("activestandby_sync_ok", "1 when configuration is in sync with the peer.", bi(sync["state"] == "in_sync"))
	if n, ok := sync["behind"].(int); ok {
		g("activestandby_sync_revisions_behind", "Saved changes not yet replicated (active node).", n)
	}
	pv, mism := d.versionInfo()
	_ = pv
	g("activestandby_version_mismatch", "1 when the nodes run different OPNsense or plugin versions.", bi(mism))
	g("activestandby_config_locked", "1 when config.xml carries the immutable flag (standby).", bi(isImmutable(configPath())))
	if st.State == StateActive {
		if raw, err := os.ReadFile(configPath()); err == nil {
			g("activestandby_unreplicated_changes", "1 when the active node holds config the peer does not have.", bi(shaHex(raw) != st.AckedSHA))
		}
	}
	prom, dem, last := d.hist.totals()
	g("activestandby_promotions_total", "Promotions recorded on this node.", prom)
	g("activestandby_demotions_total", "Demotions recorded on this node.", dem)
	g("activestandby_last_promote_duration_seconds", "Duration of the most recent promotion.", fmt.Sprintf("%.3f", float64(last)/1000))
	g("activestandby_health_failing", "1 when the active node currently fails a health check.", bi(d.healthFailing()))
	g("activestandby_drift", "1 when drift between the nodes' saved configuration was detected.", bi(d.driftDetected()))
	return b.String()
}
