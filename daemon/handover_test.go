package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// handoverLink delivers /v1/handover of a to b's handler (and the rest as deliver does).
func handoverLink(b *Daemon) func(string, time.Duration) (map[string]any, error) {
	return func(path string, _ time.Duration) (map[string]any, error) {
		if strings.HasPrefix(path, "/v1/handover") {
			rec := httptest.NewRecorder()
			b.handleHandover(rec, httptest.NewRequest(http.MethodPost, path, nil))
			if rec.Code != http.StatusOK {
				return nil, errors.New(strings.TrimSpace(rec.Body.String()))
			}
			return map[string]any{}, nil
		}
		return deliver(b, path)
	}
}

// Reported by the review of 11372fc: two standby nodes asked each other to hand over, each holding its own failover lock while
// waiting for the other: a loop that kept both "in a failover" and blocked their election.
func TestHandoverBetweenTwoStandbysEndsAtOnce(t *testing.T) {
	a := newHANode(t, standbyState(RoleMaster, 5))
	b := newHANode(t, standbyState(RoleStandby, 5))
	a.act.peerCall, b.act.peerCall = handoverLink(b), handoverLink(a)
	seePeer(a, hbMsg{State: StateStandby})
	seePeer(b, hbMsg{State: StateStandby})
	done := make(chan error, 1)
	go func() { _, err := a.failover(true); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("there is no active node to hand over: the request must fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the two standbys are waiting for each other")
	}
	if a.failing.Load() || b.failing.Load() {
		t.Fatal("a node is left 'in a failover', which blocks its election")
	}
}

// a hand-over request reaches a node that is not active: refused, nothing is done
func TestHandoverIsAnsweredOnlyByTheActiveNode(t *testing.T) {
	b := newHANode(t, standbyState(RoleStandby, 5))
	var fb fakeSystem
	fb.install(b)
	rec := httptest.NewRecorder()
	b.handleHandover(rec, httptest.NewRequest(http.MethodPost, "/v1/handover?force=1", nil))
	if rec.Code != http.StatusConflict || fb.demotes.Load() != 0 || fb.promotes.Load() != 0 {
		t.Fatalf("code %d, demotes %d, promotes %d", rec.Code, fb.demotes.Load(), fb.promotes.Load())
	}
}
