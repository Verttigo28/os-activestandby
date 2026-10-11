package main

import "time"

// The standby has no WAN and therefore no NTP. It follows the active node's clock (carried in the heartbeat
// over the authenticated HA link) whenever it drifts by more than 2 s, at most once every 30 s.

const maxClockStepMs = 5 * 60 * 1000

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
	if drift > maxClockStepMs {
		// a wildly different clock is not a drift: do not let the peer move our time by hours or years
		if time.Since(d.lastClockWarn.Get()) > time.Hour {
			d.lastClockWarn.Set(time.Now())
			d.event("warning", "the peer's clock differs by %d s: not adjusting (limit %d s); fix time on both nodes", drift/1000, maxClockStepMs/1000)
		}
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
