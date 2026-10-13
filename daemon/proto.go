package main

import (
	"fmt"
	"net/http"
	"strconv"
)

// Protocol version of the peer API and of the heartbeat. It changes when what one node sends or expects from the other changes in a way
// that an older or a newer node would misread, not with every release:
//
//	1  plugin 1.0.1 and older (the field is absent in the heartbeat, which is read as 1)
//	2  plugin 1.0.2: strict /v1/promote, X-Proto and X-Epoch (leadership generation) on the configuration push
//
// A node that is OLDER than its peer cannot know what the peer expects: the operations that need both sides to agree (a configuration
// or ruleset push, a graceful failover) are refused with a message that says which node to update. Operations that exist for safety
// (the automatic takeover after a silence, the resolution of a double active node) are never blocked by a version difference.
const protoVersion = 2

// peerProtoNow is the protocol of the peer as its last heartbeat says; ok is false if the peer is not heard.
func (d *Daemon) peerProtoNow() (proto int, ok bool) {
	alive, _, peer := d.hb.Snapshot()
	if !alive {
		return 0, false
	}
	return heartbeatProto(peer), true
}

func heartbeatProto(m hbMsg) int {
	if m.Proto <= 0 {
		return 1
	}
	return m.Proto
}

// protoBlock is "" or the reason why an operation that needs both nodes to agree must not run: the peer speaks a newer protocol.
func (d *Daemon) protoBlock() string {
	p, ok := d.peerProtoNow()
	if !ok || p <= protoVersion {
		return ""
	}
	return fmt.Sprintf("the peer speaks protocol %d and this node (plugin %s) only %d: update this node to the version of the peer first", p, version, protoVersion)
}

// checkConfigPush validates the headers of a configuration push before anything is read or applied.
//   - the revision and the checksum are required and strictly parsed;
//   - X-Proto, when present, is a number and not newer than this node's protocol;
//   - a sender that states a protocol (2 and later) must also give its leadership epoch, which must not be older than the highest one
//     already accepted from this peer (leaderSeen): a push sent by a node that has been replaced as leader, or a delayed one, does not
//     overwrite a newer configuration. The epoch of this node itself is NOT compared (a standby's own epoch does not follow the active
//     node); leaderSeen starts again at 0 when the pair is formed.
//   - a sender with no X-Proto is a plugin 1.0.1 or older (a rolling upgrade): accepted without an epoch.
//
// code 0 = valid.
func checkConfigPush(h http.Header, leaderSeen uint64) (rev, epoch uint64, code int, msg string) {
	rev, err := strconv.ParseUint(h.Get("X-Rev"), 10, 64)
	if err != nil || rev == 0 {
		return 0, 0, http.StatusBadRequest, "missing or malformed revision"
	}
	if !sha256Re.MatchString(h.Get("X-SHA256")) {
		return 0, 0, http.StatusBadRequest, "missing or malformed checksum"
	}
	ps := h.Get("X-Proto")
	if ps == "" {
		return rev, 0, 0, ""
	}
	p, err := strconv.Atoi(ps)
	if err != nil || p < 1 {
		return 0, 0, http.StatusBadRequest, "malformed protocol version"
	}
	if p > protoVersion {
		return 0, 0, http.StatusConflict, fmt.Sprintf("the sender speaks protocol %d, this node only %d: update this node first", p, protoVersion)
	}
	epoch, err = strconv.ParseUint(h.Get("X-Epoch"), 10, 64)
	if err != nil {
		return 0, 0, http.StatusBadRequest, "missing or malformed epoch"
	}
	if epoch < leaderSeen {
		return 0, 0, http.StatusConflict, fmt.Sprintf("stale leadership epoch (%d, already accepted %d): the sender is no longer the leader", epoch, leaderSeen)
	}
	return rev, epoch, 0, ""
}
