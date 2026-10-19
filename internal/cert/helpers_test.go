// Test-only helpers for the cert package tests.
package cert_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

// mintCA mints a self-signed CA cert. Validity = 10y so the CA
// is effectively permanent for the test's lifetime.
func mintCA(t *testing.T) (caCertPEM, caKeyPEM []byte) {
	t.Helper()
	return mintCAAt(t, time.Now().Add(-1*time.Hour),
		time.Now().Add(10*365*24*time.Hour))
}

// signLeafWithCA mints a leaf cert signed by the CA in
// (caCertPEM, caKeyPEM). Leaf has whatever validFor is passed.
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
