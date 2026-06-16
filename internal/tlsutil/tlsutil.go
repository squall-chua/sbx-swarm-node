// Package tlsutil loads a TLS certificate or generates a self-signed one for
// development when none is configured.
package tlsutil

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// LoadOrGenerate loads the cert/key pair from the given paths. If both paths
// are empty it generates a self-signed cert, persists it to dir/tls.crt and
// dir/tls.key, and returns it.
func LoadOrGenerate(certFile, keyFile, dir string) (tls.Certificate, error) {
	if certFile != "" && keyFile != "" {
		return tls.LoadX509KeyPair(certFile, keyFile)
	}

	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	if _, err := os.Stat(certPath); err == nil {
		return tls.LoadX509KeyPair(certPath, keyPath)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate key: %w", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "sbx-swarm-node"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// GenerateForKey builds an in-memory self-signed Ed25519 leaf certificate whose
// key IS the node key, so peers can pin the TLS channel to the gossiped node
// pubkey (ADR-0004). The cert is deterministic-enough to regenerate each boot.
func GenerateForKey(priv ed25519.PrivateKey) (tls.Certificate, error) {
	pub := priv.Public().(ed25519.PublicKey)
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "sbx-swarm-node"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost", "sbx-swarm-node"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, pub, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create node cert: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}

// LeafPublicKey returns the Ed25519 public key in a certificate's leaf.
func LeafPublicKey(cert tls.Certificate) (ed25519.PublicKey, error) {
	if len(cert.Certificate) == 0 {
		return nil, errors.New("tlsutil: empty certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("tlsutil: parse leaf: %w", err)
	}
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("tlsutil: leaf is not Ed25519")
	}
	return pub, nil
}

// PinnedVerify returns a tls.Config.VerifyPeerCertificate that requires the
// presented leaf's public key to equal expected. Accepts any crypto.PublicKey
// and compares structurally (ed25519/rsa/ecdsa public keys all implement
// Equal(crypto.PublicKey)). Pair with InsecureSkipVerify:true (default CA chain
// disabled; this pin is the real check).
func PinnedVerify(expected crypto.PublicKey) func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	type equaler interface{ Equal(crypto.PublicKey) bool }
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("tlsutil: peer presented no certificate")
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("tlsutil: parse peer leaf: %w", err)
		}
		eq, ok := leaf.PublicKey.(equaler)
		if !ok || !eq.Equal(expected) {
			return errors.New("tlsutil: peer certificate pin mismatch")
		}
		return nil
	}
}
