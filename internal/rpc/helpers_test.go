// Test-only helpers for the rpc package tests.
package rpc_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// signLeafWithCA mints a leaf cert signed by the supplied CA.
func signLeafWithCA(t *testing.T, commonName string, dnsNames []string, validFor time.Duration, caCertPEM, caKeyPEM []byte) (leafCertPEM, leafKeyPEM []byte) {
	t.Helper()
	caCertBlock, _ := pem.Decode(caCertPEM)
	if caCertBlock == nil {
		t.Fatalf("signLeafWithCA: bad CA cert PEM")
	}
	caCert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	caKeyBlock, _ := pem.Decode(caKeyPEM)
	if caKeyBlock == nil {
		t.Fatalf("signLeafWithCA: bad CA key PEM")
	}
	caKey, err := x509.ParseECPrivateKey(caKeyBlock.Bytes)
	if err != nil {
		t.Fatalf("parse CA key: %v", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen leaf key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("leaf serial: %v", err)
	}
	notBefore := time.Now().Add(-1 * time.Hour)
	notAfter := time.Now().Add(validFor)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     dnsNames,
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("CreateCertificate (leaf): %v", err)
	}
	leafCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey (leaf): %v", err)
	}
	leafKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return leafCertPEM, leafKeyPEM
}

// buildPKIAt lets tests pin NotBefore and NotAfter exactly. Used
// to fabricate an expired cert for the RF4 test.
func buildPKIAt(t *testing.T, notBefore, notAfter time.Time) (caCertPEM, caKeyPEM, leafCertPEM, leafKeyPEM []byte) {
	t.Helper()
	caCertPEM, caKeyPEM = mintCAAt(t, notBefore, notAfter)
	leafCertPEM, leafKeyPEM = signLeafWithCA(t, "test-server", []string{"localhost"},
		time.Until(notAfter), caCertPEM, caKeyPEM)
	return caCertPEM, caKeyPEM, leafCertPEM, leafKeyPEM
}

// mintCAAt is mintCA() but pins NotBefore / NotAfter.
func mintCAAt(t *testing.T, notBefore, notAfter time.Time) (caCertPEM, caKeyPEM []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen CA key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("CA serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	caCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		t.Fatalf("marshal CA key: %v", err)
	}
	caKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return caCertPEM, caKeyPEM
}

// mustWrite writes data to dir/name with mode 0600.
func mustWrite(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}
