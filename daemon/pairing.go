package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Pairing protocol (TLS; both sides present self-signed certs, no CA involved):
//   standby: `prepare` -> one-time token (shown in the GUI), pairing window 10 min, max 5 failed attempts.
//   master : POST /pair/hello   {nonce_a, info_a}   -> {nonce_b, info_b, proof_b}
//            proof_b = HMAC(token, "B|fpA|fpB|nonceA|nonceB"); master verifies it (standby knows the token).
//            compat check on both sides, then
//            POST /pair/confirm {nonce_a, proof_a}  -> standby verifies, pins the master cert, role=standby.
// fpA/fpB are SHA-256 of the DER certs seen on the actual TLS session, so a MITM cannot relay the proofs.

const pairWindow = 10 * time.Minute

type pairSession struct {
	token   []byte
	expires time.Time
	fails   int
	hellos  map[string]helloState
}

type helloState struct{ nonceB, fpA string }

type helloReq struct {
	NonceA string `json:"nonce_a"`
	Info   Info   `json:"info"`
}
type helloResp struct {
	NonceB string   `json:"nonce_b"`
	Info   Info     `json:"info"`
	Proof  string   `json:"proof_b"`
	Error  string   `json:"error,omitempty"`
	Diffs  []string `json:"diffs,omitempty"`
}
type confirmReq struct {
	NonceA string `json:"nonce_a"`
	Proof  string `json:"proof_a"`
}

func normToken(t string) []byte {
	return []byte(strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(t), "-", "")))
}

func newToken() (string, error) {
	b := make([]byte, 13)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)[:20]
	return s[0:5] + "-" + s[5:10] + "-" + s[10:15] + "-" + s[15:20], nil
}

func proof(token []byte, side, fpA, fpB, nA, nB string) string {
	m := hmac.New(sha256.New, token)
	fmt.Fprintf(m, "%s|%s|%s|%s|%s", side, fpA, fpB, nA, nB)
	return hex.EncodeToString(m.Sum(nil))
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- Daemon side (standby) ----

type Pairing struct {
	mu sync.Mutex
	s  *pairSession
}

func (p *Pairing) Start() (string, time.Time, error) {
	t, err := newToken()
	if err != nil {
		return "", time.Time{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.s = &pairSession{token: normToken(t), expires: time.Now().Add(pairWindow), hellos: map[string]helloState{}}
	return t, p.s.expires, nil
}

func (p *Pairing) Cancel() { p.mu.Lock(); p.s = nil; p.mu.Unlock() }

func (p *Pairing) Active() (bool, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.s == nil || time.Now().After(p.s.expires) {
		return false, time.Time{}
	}
	return true, p.s.expires
}

func peerFP(r *http.Request) (string, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", errors.New("no client certificate")
	}
	return fingerprint(r.TLS.PeerCertificates[0].Raw), nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (d *Daemon) handleHello(w http.ResponseWriter, r *http.Request) {
	fpA, err := peerFP(r)
	if err != nil {
		writeJSON(w, 400, helloResp{Error: err.Error()})
		return
	}
	var req helloReq
	if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req) != nil || req.NonceA == "" {
		writeJSON(w, 400, helloResp{Error: "bad request"})
		return
	}
	p := d.pairing
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.s == nil || time.Now().After(p.s.expires) {
		writeJSON(w, 403, helloResp{Error: "node is not in pairing mode"})
		return
	}
	if d.store.Get().Role != RoleStandalone {
		writeJSON(w, 409, helloResp{Error: "node is already paired; break the pair first"})
		return
	}
	local := collectInfo()
	if diffs := req.Info.Compare(local); len(diffs) > 0 {
		writeJSON(w, 409, helloResp{Error: "nodes are not identical, refusing to pair", Diffs: diffs})
		return
	}
	nB := randHex(16)
	p.s.hellos[req.NonceA] = helloState{nonceB: nB, fpA: fpA}
	writeJSON(w, 200, helloResp{NonceB: nB, Info: local,
		Proof: proof(p.s.token, "B", fpA, d.fp, req.NonceA, nB)})
}

func (d *Daemon) handleConfirm(w http.ResponseWriter, r *http.Request) {
	fpA, err := peerFP(r)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	var req confirmReq
	if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req) != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	p := d.pairing
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.s == nil || time.Now().After(p.s.expires) {
		writeJSON(w, 403, map[string]string{"error": "node is not in pairing mode"})
		return
	}
	h, ok := p.s.hellos[req.NonceA]
	if !ok || h.fpA != fpA ||
		!hmac.Equal([]byte(req.Proof), []byte(proof(p.s.token, "A", fpA, d.fp, req.NonceA, h.nonceB))) {
		p.s.fails++
		if p.s.fails >= 5 {
			p.s = nil
		}
		writeJSON(w, 403, map[string]string{"error": "authentication failed"})
		return
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if err := d.store.PairNow(RoleStandby, host, d.cfg.ListenPort, fpA); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	p.s = nil
	d.event("change", "paired with %s as STANDBY (config will be replaced by the primary)", host)
	go d.pfsyncSetup()
	writeJSON(w, 200, map[string]string{"status": "paired"})
}

// ---- Client side (master) ----

func (d *Daemon) peerClient(pinFP string, got *string) *http.Client {
	return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			Certificates:       []tls.Certificate{d.cert},
			InsecureSkipVerify: true, // identity is checked by fingerprint below, not by a CA
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("peer sent no certificate")
				}
				fp := fingerprint(cs.PeerCertificates[0].Raw)
				if got != nil {
					*got = fp
				}
				if pinFP != "" && fp != pinFP {
					return errors.New("peer certificate does not match the pinned fingerprint")
				}
				return nil
			},
		},
		DisableKeepAlives: true,
	}}
}

type PairResult struct {
	Peer Info   `json:"peer"`
	FP   string `json:"peer_fp"`
}

func (d *Daemon) doPair(peer string, port int, tokenStr string) (*PairResult, []string, error) {
	if d.store.Get().Role != RoleStandalone {
		return nil, nil, errors.New("this node is already paired; break the pair first")
	}
	token := normToken(tokenStr)
	var fpB string
	cl := d.peerClient("", &fpB)
	base := fmt.Sprintf("https://%s", net.JoinHostPort(peer, fmt.Sprint(port)))
	local := collectInfo()
	nA := randHex(16)

	post := func(path string, in, out any) (int, error) {
		b, _ := json.Marshal(in)
		resp, err := cl.Post(base+path, "application/json", bytes.NewReader(b))
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	}

	var hr helloResp
	code, err := post("/pair/hello", helloReq{NonceA: nA, Info: local}, &hr)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot reach peer: %w", err)
	}
	if code != 200 {
		return nil, hr.Diffs, errors.New(hr.Error)
	}
	fpA := fingerprint(d.cert.Certificate[0])
	if !hmac.Equal([]byte(hr.Proof), []byte(proof(token, "B", fpA, fpB, nA, hr.NonceB))) {
		return nil, nil, errors.New("peer failed authentication (wrong token or man-in-the-middle)")
	}
	if diffs := local.Compare(hr.Info); len(diffs) > 0 {
		return nil, diffs, errors.New("nodes are not identical, refusing to pair")
	}
	var cr map[string]string
	code, err = post("/pair/confirm", confirmReq{NonceA: nA, Proof: proof(token, "A", fpA, fpB, nA, hr.NonceB)}, &cr)
	if err != nil || code != 200 {
		if err == nil {
			err = errors.New(cr["error"])
		}
		return nil, nil, fmt.Errorf("pairing confirmation failed: %w", err)
	}
	if err := d.store.PairNow(RoleMaster, peer, port, fpB); err != nil {
		return nil, nil, err
	}
	d.event("change", "paired with %s as MASTER", peer)
	go d.pfsyncSetup()
	return &PairResult{Peer: hr.Info, FP: fpB}, nil, nil
}
