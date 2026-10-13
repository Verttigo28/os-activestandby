package main

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func pushHeaders(rev, sha string) http.Header {
	h := http.Header{}
	if rev != "" {
		h.Set("X-Rev", rev)
	}
	if sha != "" {
		h.Set("X-SHA256", sha)
	}
	return h
}

func TestConfigPushHeadersAreRequiredAndStrict(t *testing.T) {
	good := shaHex([]byte("x"))
	for name, tc := range map[string]struct {
		rev, sha string
		ok       bool
	}{
		"valid":              {"3", good, true},
		"revision absent":    {"", good, false},
		"revision zero":      {"0", good, false},
		"revision negative":  {"-1", good, false},
		"revision text":      {"two", good, false},
		"checksum absent":    {"3", "", false},
		"checksum too short": {"3", good[:60], false},
		"checksum uppercase": {"3", strings.ToUpper(good), false},
		"checksum not hex":   {"3", strings.Repeat("z", 64), false},
	} {
		t.Run(name, func(t *testing.T) {
			rev, _, code, msg := checkConfigPush(pushHeaders(tc.rev, tc.sha), 0)
			if tc.ok != (code == 0) {
				t.Fatalf("code %d (%s)", code, msg)
			}
			if tc.ok {
				if want, _ := strconv.ParseUint(tc.rev, 10, 64); rev != want {
					t.Fatalf("rev %d", rev)
				}
			} else if code != http.StatusBadRequest {
				t.Fatalf("a malformed push must be a 400, got %d", code)
			}
		})
	}
}

func gz(b []byte) *bytes.Buffer {
	var z bytes.Buffer
	w := gzip.NewWriter(&z)
	w.Write(b)
	w.Close()
	return &z
}

// handleConfig refuses what is not a valid push before it touches anything: nothing is applied, nothing is recorded
func TestHandleConfigRefusesBeforeApplying(t *testing.T) {
	body := []byte("<opnsense/>")
	good := shaHex(body)
	for name, tc := range map[string]struct {
		st      State
		hdr     http.Header
		body    *bytes.Buffer
		wantErr int
	}{
		"this node is active": {activeState(RoleMaster, 5), pushHeaders("3", good), gz(body), http.StatusConflict},
		"revision missing":    {standbyState(RoleStandby, 5), pushHeaders("", good), gz(body), http.StatusBadRequest},
		"checksum missing":    {standbyState(RoleStandby, 5), pushHeaders("3", ""), gz(body), http.StatusBadRequest},
		"checksum mismatch":   {standbyState(RoleStandby, 5), pushHeaders("3", shaHex([]byte("other"))), gz(body), http.StatusBadRequest},
		"body is not gzip":    {standbyState(RoleStandby, 5), pushHeaders("3", good), bytes.NewBufferString("<opnsense/>"), http.StatusBadRequest},
		"package list is bad": {standbyState(RoleStandby, 5), withPackages(pushHeaders("3", good), "!!"), gz(body), http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			d := newHANode(t, tc.st)
			req := httptest.NewRequest(http.MethodPost, "/v1/config", tc.body)
			req.Header = tc.hdr
			rec := httptest.NewRecorder()
			d.handleConfig(rec, req)
			if rec.Code != tc.wantErr {
				t.Fatalf("answered %d (%s), want %d", rec.Code, strings.TrimSpace(rec.Body.String()), tc.wantErr)
			}
			if got := d.store.Get(); got.AppliedRev != 0 || got.AppliedSHA != "" || got.SyncRev != 0 {
				t.Fatalf("a refused push left a trace: %+v", got)
			}
		})
	}
}

func withPackages(h http.Header, v string) http.Header {
	h.Set("X-Packages", v)
	return h
}

func withProto(h http.Header, proto, epoch string) http.Header {
	if proto != "" {
		h.Set("X-Proto", proto)
	}
	if epoch != "" {
		h.Set("X-Epoch", epoch)
	}
	return h
}

// The leadership generation of a configuration push, and the protocol version of the sender.
func TestConfigPushLeadershipAndProtocol(t *testing.T) {
	sha := shaHex([]byte("x"))
	for name, tc := range map[string]struct {
		proto, epoch string
		seen         uint64
		code         int
		wantEpoch    uint64
	}{
		"plugin 1.0.1 or older (no protocol header): accepted without an epoch": {"", "", 9, 0, 0},
		"current protocol, epoch above the highest accepted":                    {"2", "10", 9, 0, 10},
		"current protocol, same epoch (the same leader pushing again)":          {"2", "9", 9, 0, 9},
		"current protocol, first push of a pair":                                {"2", "1", 0, 0, 1},
		"epoch older than the highest accepted: a deposed or delayed sender":    {"2", "8", 9, http.StatusConflict, 0},
		"current protocol without an epoch":                                     {"2", "", 0, http.StatusBadRequest, 0},
		"epoch not a number":                                                    {"2", "x", 0, http.StatusBadRequest, 0},
		"negative epoch":                                                        {"2", "-3", 0, http.StatusBadRequest, 0},
		"protocol not a number":                                                 {"two", "5", 0, http.StatusBadRequest, 0},
		"protocol zero":                                                         {"0", "5", 0, http.StatusBadRequest, 0},
		"a sender with a newer protocol than this node":                         {"3", "5", 0, http.StatusConflict, 0},
	} {
		t.Run(name, func(t *testing.T) {
			_, epoch, code, msg := checkConfigPush(withProto(pushHeaders("3", sha), tc.proto, tc.epoch), tc.seen)
			if code != tc.code {
				t.Fatalf("code %d (%s), want %d", code, msg, tc.code)
			}
			if code == 0 && epoch != tc.wantEpoch {
				t.Fatalf("epoch %d, want %d", epoch, tc.wantEpoch)
			}
		})
	}
}

// the highest epoch accepted is remembered after a successful push and forgotten when a new pair is formed
func TestLeaderEpochIsKeptPerPair(t *testing.T) {
	s := storeIn(t, nil)
	if err := s.PairNow(RoleStandby, "10.0.0.1", 7690, "fp"); err != nil {
		t.Fatal(err)
	}
	s.Update(func(x *State) { x.LeaderEpoch = 40 })
	if s.Get().LeaderEpoch != 40 {
		t.Fatal("not stored")
	}
	if err := s.PairNow(RoleStandby, "10.0.0.9", 7690, "other"); err != nil {
		t.Fatal(err)
	}
	if s.Get().LeaderEpoch != 0 {
		t.Fatal("what was accepted from a former peer must not stop the configuration of a new one")
	}
}

// a standby refuses the push of a deposed leader before it reads or applies anything
func TestHandleConfigRefusesAStaleLeader(t *testing.T) {
	body := []byte("<opnsense/>")
	d := newHANode(t, standbyState(RoleStandby, 5))
	d.store.Update(func(s *State) { s.LeaderEpoch = 12 })
	req := httptest.NewRequest(http.MethodPost, "/v1/config", gz(body))
	req.Header = withProto(pushHeaders("7", shaHex(body)), "2", "11")
	rec := httptest.NewRecorder()
	d.handleConfig(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("answered %d (%s)", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if got := d.store.Get(); got.AppliedSHA != "" || got.LeaderEpoch != 12 {
		t.Fatalf("a refused push changed the state: %+v", got)
	}
}

// the protocol of the peer: absent means 1; a newer peer blocks what needs both nodes to agree, never the safety rules
func TestProtocolOfThePeer(t *testing.T) {
	if heartbeatProto(hbMsg{}) != 1 || heartbeatProto(hbMsg{Proto: 2}) != 2 || heartbeatProto(hbMsg{Proto: -4}) != 1 {
		t.Fatal("an absent or invalid protocol is 1")
	}
	d := newHANode(t, activeState(RoleMaster, 5))
	if d.protoBlock() != "" {
		t.Fatal("a peer that is not heard blocks nothing")
	}
	seePeer(d, hbMsg{State: StateStandby, Proto: protoVersion})
	if d.protoBlock() != "" {
		t.Fatal("the same protocol blocks nothing")
	}
	seePeer(d, hbMsg{State: StateStandby}) // plugin 1.0.1: no field
	if d.protoBlock() != "" {
		t.Fatal("an older peer is served")
	}
	seePeer(d, hbMsg{State: StateStandby, Proto: protoVersion + 1})
	why := d.protoBlock()
	if !strings.Contains(why, "update this node") {
		t.Fatalf("a newer peer must block with a message that says what to do: %q", why)
	}
	// a graceful failover is refused (nothing is demoted), but the safety paths do not look at the protocol
	var fb fakeSystem
	fb.install(d)
	if _, err := d.failover(true); err == nil || !strings.Contains(err.Error(), "update this node") {
		t.Fatalf("failover towards a newer peer: %v", err)
	}
	if d.store.Get().State != StateActive {
		t.Fatal("a refused failover must leave the node as it was")
	}
}
