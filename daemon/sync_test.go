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
			rev, code, msg := checkConfigPush(pushHeaders(tc.rev, tc.sha))
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
