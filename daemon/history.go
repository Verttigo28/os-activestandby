package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// History keeps the role changes (promote/demote) of this node for the GUI and the metrics:
// when, why, how long it took. Stored as a small JSON file, newest last.

const maxHistory = 200

type HistoryEntry struct {
	Time       string `json:"time"`
	Type       string `json:"type"` // promote | demote
	Reason     string `json:"reason"`
	DurationMs int64  `json:"duration_ms"`
	Epoch      uint64 `json:"epoch"`
}

type History struct {
	mu   sync.Mutex
	path string
	list []HistoryEntry
}

func openHistory(dir string) *History {
	h := &History{path: filepath.Join(dir, "history.json")}
	if b, err := os.ReadFile(h.path); err == nil {
		json.Unmarshal(b, &h.list)
	}
	return h
}

func (h *History) add(e HistoryEntry) {
	if h == nil {
		return
	}
	e.Time = time.Now().UTC().Format(time.RFC3339)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.list = append(h.list, e)
	if len(h.list) > maxHistory {
		h.list = h.list[len(h.list)-maxHistory:]
	}
	if b, err := json.Marshal(h.list); err == nil {
		writeFileDurable(h.path, b, 0o600)
	}
}

// Recent returns the newest entries first.
func (h *History) Recent(n int) []HistoryEntry {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]HistoryEntry, 0, n)
	for i := len(h.list) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, h.list[i])
	}
	return out
}

func (h *History) totals() (promotes, demotes int, lastPromoteMs int64) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range h.list {
		switch e.Type {
		case "promote":
			promotes++
			lastPromoteMs = e.DurationMs
		case "demote":
			demotes++
		}
	}
	return
}
