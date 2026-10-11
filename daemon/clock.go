package main

import "time"

// The standby has no WAN and therefore no NTP. It follows the active node's clock (carried in the heartbeat
// over the authenticated HA link) whenever it drifts by more than 2 s, at most once every 30 s.

func (d *Daemon) adjustClock(m hbMsg) {
	if m.TimeMs == 0 || m.State != StateActive || d.store.Get().State != StateStandby {
		return
	}
	drift := time.Now().UnixMilli() - m.TimeMs
	if drift < 0 {
		drift = -drift
	}
	if drift < 2000 || time.Since(d.lastClockSet.Get()) < 30*time.Second {
		return
	}
	d.lastClockSet.Set(time.Now())
	t := time.UnixMilli(m.TimeMs + 5).UTC()
	if out, err := sh("date", "-u", t.Format("200601021504.05")); err != nil {
		d.event("warning", "could not set the standby clock: %v: %s", err, out)
		return
	}
	d.event("info", "standby clock was off by %d ms and was set from the active node", drift)
}
