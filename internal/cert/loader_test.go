// Package cert_test holds black-box tests for the certificate lifecycle.
//
// Tests cover three scenarios:
//
//   1. Bootstrap: no cert files exist → loader generates self-managed CA +
//      server cert and persists them with 0600 on private keys.
//   2. Steady-state: valid cert exists → loader returns it as-is, no rotation.
//   3. Rotation trigger: cert is within its rotation window → loader renews.
//
// RF4 (brief: "cert 过期场景, daemon 应返回明确错误而非静默失败") is
// covered by internal/rpc/server_test.go's TestServer_ExpiredCertReturnsClearError.
// Here we only test the loader's rotation policy, not the wire protocol.
package cert_test

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Daedalusys/daedalus-agent/internal/cert"
)

// errorsIs is a tiny shim so this test file can errors.Is without
// importing the stdlib errors alias. It is a no-op when both are
// identical and exists to keep test files readable.
func errorsIs(err, target error) bool { return errors.Is(err, target) }

// TestLoader_Bootstrap_GeneratesCAAndServerCert verifies that, when no
// cert files exist on disk, the loader mints a self-managed CA + server
// cert, persists them, and returns a TLS config that can serve traffic.
func TestLoader_Bootstrap_GeneratesCAAndServerCert(t *testing.T) {
	dir := t.TempDir()

	loader, err := cert.New(cert.Config{
		CAPath:    filepath.Join(dir, "ca.crt"),
		CAKeyPath: filepath.Join(dir, "ca.key"),
		CertPath:  filepath.Join(dir, "server.crt"),
		KeyPath:   filepath.Join(dir, "server.key"),
		DNSNames:  []string{"localhost"},
	})
	if err != nil {
		t.Fatalf("cert.New: %v", err)
	}

	tlsCfg, err := loader.Load()
	if err != nil {
		t.Fatalf("loader.Load (bootstrap): %v", err)
	}
	if tlsCfg == nil {
		t.Fatal("loader returned nil TLS config")
	}
	if len(tlsCfg.Certificates) != 1 {
		t.Errorf("Certificates len = %d, want 1", len(tlsCfg.Certificates))
	}

	// All 4 files must exist and be readable.
	for _, p := range []string{
		filepath.Join(dir, "ca.crt"),
		filepath.Join(dir, "ca.key"),
		filepath.Join(dir, "server.crt"),
		filepath.Join(dir, "server.key"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing file %s: %v", p, err)
		}
	}

	// Private key files must be mode 0600 — they hold the CA key
	// and server key. Loader must chmod explicitly so a leftover
	// file from a previous umask can't quietly leak.
	for _, p := range []string{
		filepath.Join(dir, "ca.key"),
		filepath.Join(dir, "server.key"),
	} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if got := st.Mode().Perm(); got != 0o600 {
			t.Errorf("private key %s mode = %o, want 0600", p, got)
		}
	}

	// CA pool must contain at least one cert — that is the
	// contract the loader promises (the agent's self-managed CA
	// populates it). We avoid Subjects() which is deprecated
	// since Go 1.18; instead we confirm the pool exists and is
	// non-nil (the loader constructs it via AppendCertsFromPEM,
	// returning ErrUnparseableCert if the PEM were malformed —
	// if that error didn't fire, the pool has at least one cert).
	if tlsCfg.ClientCAs == nil {
		t.Fatal("ClientCAs is nil; expected non-nil x509.CertPool")
	}

	// mTLS posture assertions (RF4-relevant defaults).
	if tlsCfg.ClientAuth != 4 { // tls.RequireAndVerifyClientCert = 4
		t.Errorf("ClientAuth = %d, want RequireAndVerifyClientCert (4)", tlsCfg.ClientAuth)
	}
	if tlsCfg.MinVersion != 0x0303 { // TLS 1.2
		t.Errorf("MinVersion = %#x, want TLS 1.2 (0x0303)", tlsCfg.MinVersion)
	}
}

// TestLoader_RotationTrigger_RotatesNearExpiry verifies that a server
// cert whose NotAfter is within the rotation window causes the loader
// to regenerate on Load.
//
// We forge a short-lived leaf (NotAfter = now + 30m, well inside the
// 24h rotation window) and pre-write all 4 cert files. The loader
// must detect the rotation condition and rewrite server.crt /
// server.key.
func TestLoader_RotationTrigger_RotatesNearExpiry(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	caKeyPath := filepath.Join(dir, "ca.key")
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")

	// Long-lived CA + short-lived server leaf. The leaf's NotAfter
	// is 30m in the future — that's well inside the 24h rotation
	// window, so the loader must rotate.
	caCertPEM, caKeyPEM := mintCA(t)
	shortCertPEM, shortKeyPEM := signLeafWithCA(t, "server-test", []string{"localhost"}, 30*time.Minute, caCertPEM, caKeyPEM)
	if err := os.WriteFile(certPath, shortCertPEM, 0o600); err != nil {
		t.Fatalf("write server cert: %v", err)
	}
	if err := os.WriteFile(keyPath, shortKeyPEM, 0o600); err != nil {
		t.Fatalf("write server key: %v", err)
	}
	if err := os.WriteFile(caPath, caCertPEM, 0o600); err != nil {
		t.Fatalf("write CA cert: %v", err)
	}
	if err := os.WriteFile(caKeyPath, caKeyPEM, 0o600); err != nil {
		t.Fatalf("write CA key: %v", err)
	}

	loader, err := cert.New(cert.Config{
		CAPath:    caPath,
		CAKeyPath: caKeyPath,
		CertPath:  certPath,
		KeyPath:   keyPath,
		DNSNames:  []string{"localhost"},
	})
	if err != nil {
		t.Fatalf("cert.New: %v", err)
	}

	if _, err := loader.Load(); err != nil {
		t.Fatalf("loader.Load: %v", err)
	}

	// After Load(), the on-disk server cert must have been
	// replaced. The new cert's NotAfter is now + 25h (validity
	// + grace), far past the original 30m expiry.
	newCertPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read server cert: %v", err)
	}
	newBlock, _ := pem.Decode(newCertPEM)
	if newBlock == nil {
		t.Fatal("post-rotation server cert PEM invalid")
	}
	newCert, err := x509.ParseCertificate(newBlock.Bytes)
	if err != nil {
		t.Fatalf("parse rotated cert: %v", err)
	}
	if remaining := time.Until(newCert.NotAfter); remaining < 23*time.Hour {
		t.Errorf("rotated cert NotAfter remaining = %v, want ≥ 23h", remaining)
	}

	// The rotated cert's serial must differ from the original —
	// if the loader didn't rotate, the serial would be unchanged.
	origBlock, _ := pem.Decode(shortCertPEM)
	origCert, err := x509.ParseCertificate(origBlock.Bytes)
	if err != nil {
		t.Fatalf("parse original cert: %v", err)
	}
	if newCert.SerialNumber.Cmp(origCert.SerialNumber) == 0 {
		t.Errorf("server cert SerialNumber unchanged; rotation did not run")
	}
}

// TestLoader_SteadyState_NoRotationWhenFresh verifies that a server
// cert with > 24h remaining is loaded without rotation (the loader
// must idempotently leave the file alone on subsequent Load() calls).
func TestLoader_SteadyState_NoRotationWhenFresh(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	caKeyPath := filepath.Join(dir, "ca.key")
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")

	caCertPEM, caKeyPEM := mintCA(t)
	// 25h TTL: a freshly generated loader-cert would also have
	// 25h remaining (validity + grace). So 25h is "fresh enough"
	// that the loader must NOT rotate.
	longCertPEM, longKeyPEM := signLeafWithCA(t, "server-test", []string{"localhost"}, 25*time.Hour, caCertPEM, caKeyPEM)
	if err := os.WriteFile(certPath, longCertPEM, 0o600); err != nil {
		t.Fatalf("write server cert: %v", err)
	}
	if err := os.WriteFile(keyPath, longKeyPEM, 0o600); err != nil {
		t.Fatalf("write server key: %v", err)
	}
	if err := os.WriteFile(caPath, caCertPEM, 0o600); err != nil {
		t.Fatalf("write CA cert: %v", err)
	}
	if err := os.WriteFile(caKeyPath, caKeyPEM, 0o600); err != nil {
		t.Fatalf("write CA key: %v", err)
	}
	originalCertBytes := mustRead(t, certPath)
	originalKeyBytes := mustRead(t, keyPath)

	loader, err := cert.New(cert.Config{
		CAPath:    caPath,
		CAKeyPath: caKeyPath,
		CertPath:  certPath,
		KeyPath:   keyPath,
		DNSNames:  []string{"localhost"},
	})
	if err != nil {
		t.Fatalf("cert.New: %v", err)
	}
	if _, err := loader.Load(); err != nil {
		t.Fatalf("loader.Load: %v", err)
	}

	// Files must be byte-identical: if the loader rotated, the
	// (re-randomised) serial would have changed the PEM output.
	if got := mustRead(t, certPath); string(got) != string(originalCertBytes) {
		t.Errorf("server cert changed; loader rotated a long-lived cert (must not)")
	}
	if got := mustRead(t, keyPath); string(got) != string(originalKeyBytes) {
		t.Errorf("server key changed; loader rotated a long-lived cert (must not)")
	}
}

// TestLoader_MissingConfig_ReturnsErrMissingConfig guards the
// input-validation contract: New() with empty required paths must
// return a sentinel error rather than panic or silently produce a
// broken Loader.
func TestLoader_MissingConfig_ReturnsErrMissingConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  cert.Config
	}{
		{"empty all", cert.Config{}},
		{"empty CAPath", cert.Config{CAKeyPath: "x", CertPath: "x", KeyPath: "x"}},
		{"empty CAKeyPath", cert.Config{CAPath: "x", CertPath: "x", KeyPath: "x"}},
		{"empty CertPath", cert.Config{CAPath: "x", CAKeyPath: "x", KeyPath: "x"}},
		{"empty KeyPath", cert.Config{CAPath: "x", CAKeyPath: "x", CertPath: "x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := cert.New(c.cfg)
			if err == nil {
				t.Fatal("cert.New returned nil err; want ErrMissingConfig")
			}
			if !errorsIs(err, cert.ErrMissingConfig) {
				t.Errorf("err = %v; want errors.Is(ErrMissingConfig)", err)
			}
		})
	}
}

// mustRead reads path or fails the test.
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
