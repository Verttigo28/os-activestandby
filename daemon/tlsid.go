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
	cp, kp := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	c, err := tls.LoadX509KeyPair(cp, kp)
	if err == nil {
		return c, nil
	}
	if _, serr := os.Stat(cp); serr == nil {
		// the files are there but unusable (damaged): a new identity is created, and a paired peer will refuse it until the pair is
		// formed again. Said loudly, never silently.
		log.Printf("ERROR: the TLS identity of this node (%s) cannot be loaded (%v): creating a new one; the peer will refuse this node until the pair is formed again", cp, err)
	}
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
