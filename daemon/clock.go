package main

import (
	"sync"
	"time"
)

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
	if !d.clockBudgetAllows(drift) {
		if time.Since(d.lastClockWarn.Get()) > time.Hour {
			d.lastClockWarn.Set(time.Now())
			d.event("warning", "the standby clock has already been moved by %d s in the last hour on the word of the peer: not adjusting again (limit %d s per hour); fix time on both nodes", clockSpentMs(time.Now())/1000, clockBudgetMs/1000)
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

// A step is limited to maxClockStepMs, but the limit alone lets a peer that claims to be active walk the clock of the standby
// forward by that much every 30 s for ever (each step brings the drift back to zero). The total moved in the last hour is limited too.
const clockBudgetMs = 15 * 60 * 1000

var clockSteps struct {
	mu    sync.Mutex
	steps []clockStep
}

type clockStep struct {
	at time.Time
	ms int64
}

func clockSpentMs(now time.Time) int64 {
	clockSteps.mu.Lock()
	defer clockSteps.mu.Unlock()
	var n int64
	for _, s := range clockSteps.steps {
		if now.Sub(s.at) < time.Hour {
			n += s.ms
		}
	}
	return n
}

// clockBudgetAllows records a step of ms milliseconds if the budget of the last hour still covers it.
func (d *Daemon) clockBudgetAllows(ms int64) bool {
	now := time.Now()
	if clockSpentMs(now)+ms > clockBudgetMs {
		return false
	}
	clockSteps.mu.Lock()
	var keep []clockStep
	for _, s := range clockSteps.steps {
		if now.Sub(s.at) < time.Hour {
			keep = append(keep, s)
		}
	}
	clockSteps.steps = append(keep, clockStep{now, ms})
	clockSteps.mu.Unlock()
	return true
}
