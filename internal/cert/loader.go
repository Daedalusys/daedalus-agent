// Package cert owns the daedalus-agent TLS certificate lifecycle.
//
// At P0.5 the agent self-manages its own CA: on first start it
// generates a CA keypair + a server certificate signed by that CA
// and persists them under CAPath / CAKeyPath / CertPath / KeyPath.
// On every Load() the loader checks the server cert's remaining
// lifetime: if it is within RotationThreshold (24h), the loader
// regenerates the server cert (re-signed by the existing CA) before
// returning a *tls.Config. The CA itself is permanent; only the
// server cert is rotated.
//
// Spec references:
//
//   - §7.3 C3: cert TTL 24h, grace 1h. The cert's NotAfter is
//     set to (now + Validity + Grace), so the effective
//     usable period is 25h. After NotAfter, Go's standard TLS
//     verifier rejects the cert with a clear error — that is
//     what the spec means by "daemon 应返回明确错误而非静默失败"
//     (RF4).
//   - §4.4: transport is gRPC over mTLS. ClientAuth is
//     RequireAndVerifyClientCert; the CA pool is the agent's
//     own CA pub key.
//
// File layout (per T2 FHS):
//
//	/var/lib/daedalus-agent/cert/ca.crt     (0644)  public
//	/var/lib/daedalus-agent/cert/ca.key     (0600)  private
//	/var/lib/daedalus-agent/cert/server.crt (0644)  public
//	/var/lib/daedalus-agent/cert/server.key (0600)  private
//
// The 0600 mode on the private keys is enforced by [writeWithMode].
// Note: process umask is honoured by [os.WriteFile]'s mode argument;
// production deployments must run the agent as root (or with a
// dedicated daedalus-agent user) so 0600 maps to "owner-only" rather
// than "world-unreadable on default umask".
package cert

import (
	"crypto/ecdsa"
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

// Validity is the period the cert is expected to be alive before
// rotation. Spec §7.3 C3: 24h.
const Validity = 24 * time.Hour

// Grace is the additional period baked into NotAfter past Validity,
// during which Go's TLS verifier continues to accept the cert.
//
// Why bake the grace into NotAfter rather than implement a custom
// verifier: the spec's "grace 1h" is a property of the wire
// protocol, not of the cert lifecycle. Baking it into NotAfter
// means standard Go TLS verifiers (the control plane will speak
// the same TLS stack) get grace for free, without us having to
// reimplement the verify chain — which is exactly the kind of
// "custom crypto code" TigerStyle warns against.
const Grace = 1 * time.Hour

// RotationThreshold is the remaining lifetime at which Load()
// decides to re-issue the server certificate.
//
// Semantics: if (cert.NotAfter - now) ≤ RotationThreshold the cert
// is inside its rotation window — we have less than one full
// validity period of headroom left, so renew now rather than wait
// for the actual expiry. The loader is also called when the cert
// has already expired (in which case the rotation obviously fires).
//
// 24h is chosen so a freshly-generated cert is NOT rotated on its
// first Load(): its remaining lifetime is 25h (validity + grace),
// which is strictly greater than 24h. The cert only enters the
// rotation window after ~1h of uptime, at which point the loader
// swaps in a fresh 25h cert.
const RotationThreshold = Validity // 24h

// Sentinel errors. Tests errors.Is against these.
var (
	// ErrMissingConfig is returned by New when the Config is
	// empty or required paths are unset.
	ErrMissingConfig = errors.New("cert: missing required config")

	// ErrUnparseableCert is returned when an on-disk file does
	// not decode as a valid certificate.
	ErrUnparseableCert = errors.New("cert: unparseable cert file")
)

// Config parameterises a Loader. All four paths are required.
//
// CAPath / CAKeyPath point to the agent's self-managed CA.
// CertPath / KeyPath point to the server certificate (leaf) that
// the gRPC server presents.
//
// DNSNames is the SAN list for the server certificate. In real
// deployments this is the agent's hostname (or a wildcard under
// `*.daedalus-agent.local`); tests pin it to "localhost" so the
// cert verifies against the loopback dial.
//
// CommonName is the cert's CN; defaults to "daedalus-agent"
// when empty.
type Config struct {
	CAPath     string
	CAKeyPath  string
	CertPath   string
	KeyPath    string
	DNSNames   []string
	CommonName string
}

// Loader loads (and rotates) the agent's server certificate.
//
// Loader is safe for concurrent use, but in practice only main()
// calls Load() once at startup; the rotation check is on
// subsequent Load() calls. A Loader has no internal state beyond
// its Config pointer.
type Loader struct {
	cfg Config
	// now is overridable for tests. Production always leaves it
	// nil and uses time.Now.
	now func() time.Time
}

// New constructs a Loader. It returns ErrMissingConfig if any
// required path is empty.
//
// New does NOT touch disk. Call Load() to actually read or
// generate the cert material.
func New(cfg Config) (*Loader, error) {
	if cfg.CAPath == "" || cfg.CAKeyPath == "" ||
		cfg.CertPath == "" || cfg.KeyPath == "" {
		return nil, ErrMissingConfig
	}
	if cfg.CommonName == "" {
		cfg.CommonName = "daedalus-agent"
	}
	if len(cfg.DNSNames) == 0 {
		cfg.DNSNames = []string{"localhost"}
	}
	return &Loader{cfg: cfg, now: time.Now}, nil
}

// Load returns a *tls.Config ready for grpc.NewServer to consume.
//
// Load:
//
//  1. If the server cert file is missing, mints a self-managed CA
//     and a server cert signed by it; writes all 4 files.
//  2. If the server cert file exists but its remaining lifetime
//     is ≤ RotationThreshold (24h), regenerates the server cert
//     in place. The CA is preserved (no CA rotation at P0.5).
//  3. Otherwise returns a TLS config that pins the existing cert.
//
// The returned *tls.Config enforces:
//
//   - ClientAuth: RequireAndVerifyClientCert (mTLS).
//   - MinVersion: TLS 1.2.
//   - ClientCAs: x509.CertPool containing the agent's CA.
//
// It is the caller's responsibility to feed the config into a
// grpc.NewServer (see internal/rpc/server.go).
func (l *Loader) Load() (*tls.Config, error) {
	if err := l.ensureCertFiles(); err != nil {
		return nil, fmt.Errorf("cert: load: %w", err)
	}
	return l.buildTLSConfig()
}

// ensureCertFiles is the core load-or-rotate state machine.
//
// Algorithm:
//
//   a. If CertPath is missing → bootstrap: generate CA + server
//      cert, write all 4 files.
//   b. If CertPath exists AND cert.NotAfter ≤ now+RotationThreshold
//      → rotate server cert in place; CA is read from CAPath and
//      not regenerated.
//   c. Otherwise → no-op. The on-disk files are returned as-is to
//      buildTLSConfig.
func (l *Loader) ensureCertFiles() error {
	existing, err := readCertIfExists(l.cfg.CertPath)
	if err != nil {
		return err
	}

	if existing == nil {
		// Case (a): bootstrap.
		return l.bootstrap()
	}

	// Case (b) vs (c): read remaining lifetime.
	remaining := existing.NotAfter.Sub(l.now())
	if remaining <= RotationThreshold {
		return l.rotateServer()
	}
	// Case (c): steady state.
	return nil
}

// bootstrap mints a fresh CA + signed server cert and writes
// all four files. Used on first start when no cert exists.
func (l *Loader) bootstrap() error {
	caCertPEM, caKeyPEM, err := generateCA(l.now)
	if err != nil {
		return fmt.Errorf("generate CA: %w", err)
	}
	if err := writeWithMode(l.cfg.CAPath, caCertPEM, 0o644); err != nil {
		return err
	}
	if err := writeWithMode(l.cfg.CAKeyPath, caKeyPEM, 0o600); err != nil {
		return err
	}

	caCert, err := parseCert(caCertPEM)
	if err != nil {
		return err
	}
	caKey, err := parseECKey(caKeyPEM)
	if err != nil {
		return err
	}

	serverCertPEM, serverKeyPEM, err := generateServerCert(
		l.cfg.CommonName, l.cfg.DNSNames, l.now, caCert, caKey)
	if err != nil {
		return fmt.Errorf("generate server cert: %w", err)
	}
	if err := writeWithMode(l.cfg.CertPath, serverCertPEM, 0o644); err != nil {
		return err
	}
	return writeWithMode(l.cfg.KeyPath, serverKeyPEM, 0o600)
}

// rotateServer re-issues the server cert with the same CA. The
// existing CA is loaded from disk (read-only); the server cert
// is rewritten in place.
func (l *Loader) rotateServer() error {
	caCertPEM, err := os.ReadFile(l.cfg.CAPath)
	if err != nil {
		return fmt.Errorf("read CA cert: %w", err)
	}
	caKeyPEM, err := os.ReadFile(l.cfg.CAKeyPath)
	if err != nil {
		return fmt.Errorf("read CA key: %w", err)
	}
	caCert, err := parseCert(caCertPEM)
	if err != nil {
		return err
	}
	caKey, err := parseECKey(caKeyPEM)
	if err != nil {
		return err
	}

	serverCertPEM, serverKeyPEM, err := generateServerCert(
		l.cfg.CommonName, l.cfg.DNSNames, l.now, caCert, caKey)
	if err != nil {
		return fmt.Errorf("rotate server cert: %w", err)
	}
	if err := writeWithMode(l.cfg.CertPath, serverCertPEM, 0o644); err != nil {
		return err
	}
	return writeWithMode(l.cfg.KeyPath, serverKeyPEM, 0o600)
}

// buildTLSConfig reads the (possibly just-rotated) cert/key pair
// off disk and assembles a *tls.Config.
func (l *Loader) buildTLSConfig() (*tls.Config, error) {
	certPEM, err := os.ReadFile(l.cfg.CertPath)
	if err != nil {
		return nil, fmt.Errorf("read cert: %w", err)
	}
	keyPEM, err := os.ReadFile(l.cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("read key: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse cert+key pair: %w", err)
	}

	caCertPEM, err := os.ReadFile(l.cfg.CAPath)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCertPEM) {
		return nil, ErrUnparseableCert
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ============== crypto helpers ==============
//
// All helpers below use ECDSA P-256 and the stdlib only. No
// external PKI dependency.

// generateCA mints a self-signed CA certificate. Validity = 10y
// (effectively permanent for P0.5). The CA never rotates during
// the agent's lifetime; if the operator wants to rotate, they
// delete /var/lib/daedalus-agent/cert/{ca.crt,ca.key} and restart.
func generateCA(now func() time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	notBefore := now().Add(-1 * time.Hour)
	notAfter := now().Add(10 * 365 * 24 * time.Hour) // 10 years
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "daedalus-agent-ca",
			Organization: []string{"Daedalusys"},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// generateServerCert mints a leaf server cert signed by ca. Its
// NotAfter is now + Validity + Grace (25h total). The CA is
// unchanged across rotations.
func generateServerCert(
	commonName string,
	dnsNames []string,
	now func() time.Time,
	caCert *x509.Certificate,
	caKey *ecdsa.PrivateKey,
) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	notBefore := now().Add(-1 * time.Hour)
	notAfter := now().Add(Validity + Grace)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   commonName,
			Organization: []string{"Daedalusys"},
		},
		NotBefore:   notBefore,
		NotAfter:    notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:    dnsNames,
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// ============== file / parsing helpers ==============

// writeWithMode writes data to path and chmod's it to mode. Fails
// if the directory does not exist (caller is responsible for
// mkdir -p). Private key files must be 0600, cert files 0644.
func writeWithMode(path string, data []byte, mode os.FileMode) error {
	// Ensure parent dir exists (best-effort — only fails loudly
	// for root-level paths the operator already created).
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("cert: mkdir %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		return fmt.Errorf("cert: write %s: %w", path, err)
	}
	// os.WriteFile already applies mode on create, but chmod
	// explicitly in case the file pre-existed with looser perms.
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("cert: chmod %s: %w", path, err)
	}
	return nil
}

// readCertIfExists returns the parsed leaf cert at path, or nil
// if the file does not exist. Returns ErrUnparseableCert if the
// file exists but cannot be parsed.
func readCertIfExists(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("cert: read %s: %w", path, err)
	}
	return parseCert(b)
}

func parseCert(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, ErrUnparseableCert
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnparseableCert, err)
	}
	return cert, nil
}

func parseECKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("cert: bad key PEM")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}
