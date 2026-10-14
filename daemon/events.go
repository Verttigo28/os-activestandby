package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
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
	// Local is Time in the time zone of this server, filled when the events are read (localtime.go), never stored
	Local string `json:"local,omitempty"`
}

type Events struct {
	recent map[string]time.Time // message -> last time it was recorded (flood protection)
	mu     sync.Mutex
	list   []Event
	path   string
	sink   func(Event) // notification dispatcher (syslog, webhook), called asynchronously
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
	e.mu.Lock()
	if e.recent == nil {
		e.recent = map[string]time.Time{}
	}
	key := kind + "|" + msg
	if t, ok := e.recent[key]; ok && time.Since(t) < 20*time.Second && !strings.HasPrefix(msg, "AUDIT") {
		e.mu.Unlock() // the same event again within seconds: a flapping peer must not flood the log and the notifiers
		return
	}
	e.recent[key] = time.Now()
	if len(e.recent) > 200 {
		e.recent = map[string]time.Time{key: time.Now()}
	}
	e.mu.Unlock()
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
	if e.sink != nil {
		go e.sink(ev)
	}
}

// Recent returns the newest events first.
func (e *Events) Recent(n int) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Event, 0, n)
	for i := len(e.list) - 1; i >= 0 && len(out) < n; i-- {
		ev := e.list[i]
		ev.Local = localStamp(ev.Time)
		out = append(out, ev)
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
