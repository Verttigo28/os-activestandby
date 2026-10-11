package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Events is a small persistent ring of operational events (role changes, sync problems, split-brain, ...),
// shown in the GUI. One JSON object per line in /conf/activestandby/events.log, trimmed at start.

const maxEvents = 100

type Event struct {
	Time string `json:"time"`
	Kind string `json:"kind"` // info | change | warning | error
	Msg  string `json:"msg"`
}

type Events struct {
	mu   sync.Mutex
	list []Event
	path string
}

func openEvents(dir string) *Events {
	e := &Events{path: filepath.Join(dir, "events.log")}
	if f, err := os.Open(e.path); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var ev Event
			if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Msg != "" {
				e.list = append(e.list, ev)
			}
		}
	}
	if len(e.list) > maxEvents {
		e.list = e.list[len(e.list)-maxEvents:]
	}
	return e
}

func (e *Events) add(kind, msg string) {
	ev := Event{Time: time.Now().UTC().Format(time.RFC3339), Kind: kind, Msg: msg}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, ev)
	if len(e.list) > maxEvents {
		e.list = e.list[len(e.list)-maxEvents:]
	}
	if f, err := os.OpenFile(e.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		b, _ := json.Marshal(ev)
		f.Write(append(b, '\n'))
		f.Close()
	}
}

// Recent returns the newest events first.
func (e *Events) Recent(n int) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Event, 0, n)
	for i := len(e.list) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, e.list[i])
	}
	return out
}

func (d *Daemon) event(kind, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	log.Printf("event[%s] %s", kind, msg)
	if d.ev != nil {
		d.ev.add(kind, msg)
	}
}
