package main

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// phaseLog records how long each stage of a promotion took (offset from its start), for the history and the benchmarks:
// nothing can be optimised that was not measured first.
type phaseLog struct {
	t0 time.Time
	mu sync.Mutex
	p  []Phase
}

type Phase struct {
	Name string `json:"name"`
	Ms   int64  `json:"ms"` // since the start of the operation
}

func (p *phaseLog) mark(what string) {
	ms := time.Since(p.t0).Milliseconds()
	log.Printf("  +%4dms %s", ms, what)
	p.mu.Lock()
	p.p = append(p.p, Phase{Name: what, Ms: ms})
	p.mu.Unlock()
}

func (p *phaseLog) list() []Phase {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Phase(nil), p.p...)
}

// markDegraded records that the node is active but something it should provide is not working. It is shown in the
// status and logged as an error; it is cleared by the next promotion or demotion.
func (d *Daemon) markDegraded(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if prev, _ := d.degraded.Load().(string); prev != "" {
		msg = prev + "; " + msg
	}
	d.degraded.Store(msg)
	d.event("error", "DEGRADED: %s", msg)
}

func (d *Daemon) degradedReason() string {
	s, _ := d.degraded.Load().(string)
	return s
}
