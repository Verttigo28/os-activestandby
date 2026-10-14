package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// loadOrCreateCert returns this node's long-lived self-signed identity.
// Trust is never based on a CA: peers pin the SHA-256 of the DER certificate at pairing time.
func loadOrCreateCert(dir string) (tls.Certificate, error) {
	c, _, err := loadIdentity(dir)
	return c, err
}

// loadIdentity returns the identity of this node and whether it had to be REPLACED (it existed and neither it nor its backup copy
// could be read). The identity is what the peer pins: a node that loses it can no longer prove who it is, so it is kept twice
// (cert.pem/key.pem and a .bak copy of each), a damaged copy is restored from the other, and damaged files are moved aside, never
// overwritten. Only when both copies are unusable is a new identity created; the caller must then treat the node as unpaired.
func loadIdentity(dir string) (cert tls.Certificate, replaced bool, err error) {
	cp, kp := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	cb, kb := cp+".bak", kp+".bak"
	c, loadErr := tls.LoadX509KeyPair(cp, kp)
	if loadErr == nil {
		backupIdentity(cp, kp, cb, kb)
		return c, false, nil
	}
	_, ce := os.Stat(cp)
	_, ke := os.Stat(kp)
	existed := ce == nil || ke == nil // something was there: damaged or half written
	if c, berr := tls.LoadX509KeyPair(cb, kb); berr == nil {
		if existed {
			keepAside(cp, kp)
			log.Printf("ERROR: the TLS identity of this node was damaged (%v): restored from its backup copy, the damaged files are kept aside", loadErr)
		} else {
			log.Printf("the TLS identity of this node was missing: restored from its backup copy")
		}
		restoreIdentity(cp, kp, cb, kb)
		return c, false, nil
	}
	if existed {
		log.Printf("ERROR: the TLS identity of this node (%s) and its backup copy cannot be loaded (%v): creating a new one; the damaged files are kept aside", cp, loadErr)
		keepAside(cp, kp)
	}
	c, err = createIdentity(cp, kp)
	if err != nil {
		return tls.Certificate{}, existed, err
	}
	backupIdentity(cp, kp, cb, kb)
	return c, existed, nil
}

// backupIdentity keeps the .bak copies equal to the identity in use.
func backupIdentity(cp, kp, cb, kb string) {
	for _, f := range [][2]string{{cp, cb}, {kp, kb}} {
		a, err := os.ReadFile(f[0])
		if err != nil {
			continue
		}
		if b, err := os.ReadFile(f[1]); err == nil && string(a) == string(b) {
			continue
		}
		if err := writeFileDurable(f[1], a, 0o600); err != nil {
			log.Printf("TLS identity: backup copy %s not written: %v", f[1], err)
		}
	}
}

func restoreIdentity(cp, kp, cb, kb string) {
	for _, f := range [][2]string{{cb, cp}, {kb, kp}} {
		if b, err := os.ReadFile(f[0]); err == nil {
			if err := writeFileDurable(f[1], b, 0o600); err != nil {
				log.Printf("TLS identity: %s not restored: %v", f[1], err)
			}
		}
	}
}

// keepAside renames damaged identity files instead of overwriting them (they may still be recoverable by hand).
func keepAside(files ...string) {
	stamp := asideStamp()
	for _, f := range files {
		if _, err := os.Stat(f); err == nil {
			if err := os.Rename(f, f+".damaged-"+stamp); err != nil {
				log.Printf("TLS identity: %s could not be kept aside: %v", f, err)
			}
		}
	}
}

func createIdentity(cp, kp string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	host, _ := os.Hostname()
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "hasyncd-" + host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(20, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	// durable writes: a key torn by a power loss would make the next start create another identity, and the peer (which pins the
	// fingerprint) would refuse this node until it is paired again
	if err := writeFileDurable(kp, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := writeFileDurable(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return tls.Certificate{}, err
	}
	return tls.LoadX509KeyPair(cp, kp)
}

func fingerprint(der []byte) string {
	h := sha256.Sum256(der)
	return hex.EncodeToString(h[:])
}

// asideStamp names the copy of a damaged file (a variable for the tests).
var asideStamp = func() string { return time.Now().UTC().Format("20060102T150405Z") }
