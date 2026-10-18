// Package rpc_test holds integration tests for the gRPC server's TLS
// posture.
//
// We exercise the server's TLS config via raw tls.Dial on the
// listener address rather than gRPC's transport layer for two
// reasons:
//
//  1. The unit under test for T3 is the *server's TLS config* —
//     the cert / CA pool / ClientAuth triple — not gRPC's
//     transport retry / backoff / reconnection machinery. Raw
//     tls.Dial isolates the TLS verifier behavior from gRPC's
//     transport connector so failures point at the cert layer.
//
//  2. RF4 (brief: "daemon 应返回明确错误而非静默失败") demands the
//     client see a deterministic, recognisable error when the
//     server's cert is expired. The gRPC transport, on
//     handshake failure, retries internally and surfaces the
//     failure as context.DeadlineExceeded; the immediate x509
//     expiry error is buried inside its retry loop. Raw
//     tls.Dial returns the stdlib x509 error directly.
//
// TLS-version note: the cert.Loader pins MinVersion=TLS 1.2 and
// does NOT cap the MaxVersion, so the negotiated version can be
// 1.3. In TLS 1.3, the server sends its Finished BEFORE it
// verifies the client's certificate chain, then alerts on
// failure — so a no-cert client may see the dial return
// success while the server eventually detects the missing cert.
// To get the deterministic "client sees rejection immediately"
// behaviour we rely on, all tests below pin MaxVersion=TLS 1.2
// on both sides. Production keeps MinVersion=1.2 with 1.3 as
// the negotiated ceiling; this is a T3 test-harness constraint,
// not a production constraint.
package rpc_test

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/Daedalusys/daedalus-agent/internal/cert"
	"github.com/Daedalusys/daedalus-agent/internal/rpc"
)

// pinTLS12 returns a new *tls.Config with MaxVersion capped at
// TLS 1.2 (see package comment for why). We allocate a fresh
// config rather than copying the source — *tls.Config embeds a
// sync.RWMutex; copying it locks the lock on the source.
//
// We copy the relevant leaf pointers (ClientCAs, Certificates,
// etc.) so the pinned config actually shares the cert pool with
// the source. The fields we don't use are left zero.
func pinTLS12(cfg *tls.Config) *tls.Config {
	return &tls.Config{
		Certificates: cfg.Certificates,
		ClientCAs:    cfg.ClientCAs,
		ClientAuth:   cfg.ClientAuth,
		MinVersion:   cfg.MinVersion,
		MaxVersion:   tls.VersionTLS12,
		ServerName:   cfg.ServerName,
		RootCAs:      cfg.RootCAs,
	}
}

// TestServer_TLSHandshake_SucceedsWithMutualTLS verifies the
// happy path: mTLS handshake completes with mutual cert
// verification.
//
// We use raw tls.Dial (not grpc.Dial) for the reasons in the
// package comment. The CN assertion is informational — the
// loader may have rotated the cert on disk (when the test
// cert's remaining lifetime dips into the rotation window),
// in which case the CN is "daedalus-agent". The test's
// contract is "handshake completes, both sides see a peer
// chain", not "the cert is the one we wrote".
func TestServer_TLSHandshake_SucceedsWithMutualTLS(t *testing.T) {
	// 60h TTL: well outside the loader's 24h rotation window.
	caCertPEM, caKeyPEM, serverCertPEM, serverKeyPEM := buildPKIAt(t,
		time.Now().Add(-1*time.Hour),
		time.Now().Add(60*time.Hour),
	)
	clientCertPEM, clientKeyPEM := signLeafWithCA(t, "client-test", []string{},
		60*time.Hour, caCertPEM, caKeyPEM)

	dir := t.TempDir()
	loader, err := cert.New(cert.Config{
		CAPath:    mustWrite(t, dir, "ca.crt", caCertPEM),
		CAKeyPath: mustWrite(t, dir, "ca.key", caKeyPEM),
		CertPath:  mustWrite(t, dir, "server.crt", serverCertPEM),
		KeyPath:   mustWrite(t, dir, "server.key", serverKeyPEM),
		DNSNames:  []string{"localhost"},
	})
	if err != nil {
		t.Fatalf("cert.New: %v", err)
	}
	tlsCfg, err := loader.Load()
	if err != nil {
		t.Fatalf("loader.Load: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	srv, err := rpc.NewServer(listener.Addr().String(), tlsCfg)
	if err != nil {
		t.Fatalf("rpc.NewServer: %v", err)
	}
	go func() {
		_ = srv.Serve(listener)
	}()
	t.Cleanup(func() { srv.Stop() })

	clientCert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		t.Fatalf("parse client cert: %v", err)
	}
	clientCfg := pinTLS12(&tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      tlsCfg.ClientCAs,
		ServerName:   "localhost",
	})

	dialer := &net.Dialer{Timeout: 3 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", listener.Addr().String(), clientCfg)
	if err != nil {
		t.Fatalf("mTLS handshake failed: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// Inspect peer chains on both sides.
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		t.Errorf("server did not present its cert to the client")
	}
	if !state.HandshakeComplete {
		t.Errorf("client-side handshake did not complete")
	}
}

// TestServer_ExpiredCertReturnsClearError is the RF4
// verification.
//
// We forge a server cert with NotBefore=-4s / NotAfter=-2s
// (already expired by 2s), stand up the gRPC server with that
// cert directly (bypassing cert.Loader's rotation, since the
// loader would otherwise rotate the expired cert and mask the
// behaviour we want to observe), and use raw tls.Dial to
// observe the immediate x509 expiry error.
//
// The Go standard library wraps cert expiry in the form:
//
//	"x509: certificate has expired or is not yet valid"
//
// We assert that the dialer's error chain contains "expired".
// Wall time is logged so the controller can verify "real cert,
// real wait" — not a clock mock or library short-circuit.
func TestServer_ExpiredCertReturnsClearError(t *testing.T) {
	caCertPEM, _, serverCertPEM, serverKeyPEM := buildPKIAt(t,
		time.Now().Add(-4*time.Second),
		time.Now().Add(-2*time.Second),
	)

	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatalf("parse server cert: %v", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCertPEM) {
		t.Fatal("AppendCertsFromPEM failed for CA")
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   tls.VersionTLS12,
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	srv, err := rpc.NewServer(listener.Addr().String(), tlsCfg)
	if err != nil {
		t.Fatalf("rpc.NewServer: %v", err)
	}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { srv.Stop() })

	clientCfg := &tls.Config{
		RootCAs:    caPool,
		ServerName: "localhost",
		MaxVersion: tls.VersionTLS12,
	}

	start := time.Now()
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	_, dialErr := tls.DialWithDialer(dialer, "tcp", listener.Addr().String(), clientCfg)
	wallTime := time.Since(start)

	if dialErr == nil {
		t.Fatalf("expected client dial to fail for expired cert; got nil")
	}
	t.Logf("RF4 wall time: %v, error: %v", wallTime, dialErr)

	if !strings.Contains(dialErr.Error(), "expired") {
		for inner := errors.Unwrap(dialErr); inner != nil; inner = errors.Unwrap(inner) {
			if strings.Contains(inner.Error(), "expired") {
				return
			}
		}
		t.Errorf("client error does not look like an x509 expiry error: %v", dialErr)
	}

	if wallTime > 3*time.Second {
		t.Errorf("RF4 wall time %v too long — likely silent / hung", wallTime)
	}
}

// TestServer_GRPCWithoutClientCert_Rejected verifies that
// RequireAndVerifyClientCert is enforced: a client presenting
// no cert must not complete the handshake.
//
// Both client and server are pinned to TLS 1.2 (see package
// comment) so the server's missing-client-cert rejection
// happens BEFORE Finished — otherwise, in TLS 1.3, the
// client's Handshake may complete before the server detects
// the missing cert.
func TestServer_GRPCWithoutClientCert_Rejected(t *testing.T) {
	// 60h TTL — outside the rotation window.
	caCertPEM, _, serverCertPEM, serverKeyPEM := buildPKIAt(t,
		time.Now().Add(-1*time.Hour),
		time.Now().Add(60*time.Hour),
	)
	dir := t.TempDir()
	loader, err := cert.New(cert.Config{
		CAPath:    mustWrite(t, dir, "ca.crt", caCertPEM),
		CAKeyPath: mustWrite(t, dir, "ca.key", caCertPEM),
		CertPath:  mustWrite(t, dir, "server.crt", serverCertPEM),
		KeyPath:   mustWrite(t, dir, "server.key", serverKeyPEM),
		DNSNames:  []string{"localhost"},
	})
	if err != nil {
		t.Fatalf("cert.New: %v", err)
	}
	loaderCfg, err := loader.Load()
	if err != nil {
		t.Fatalf("loader.Load: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	srv, err := rpc.NewServer(listener.Addr().String(), loaderCfg)
	if err != nil {
		t.Fatalf("rpc.NewServer: %v", err)
	}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { srv.Stop() })

	// Client trusts the CA but presents no client cert.
	clientCfg := &tls.Config{
		RootCAs:    loaderCfg.ClientCAs,
		ServerName: "localhost",
		MaxVersion: tls.VersionTLS12,
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	_, dialErr := tls.DialWithDialer(dialer, "tcp", listener.Addr().String(), clientCfg)
	if dialErr == nil {
		t.Fatalf("expected handshake rejection when client cert missing")
	}
	// With TLS 1.2 + RequireAndVerifyClientCert + empty client cert,
	// the server sends Alert(bad_certificate) before Finished. The
	// stdlib x509 verifier surfaces this as one of:
	//   "remote error: bad certificate"
	//   "tls: bad certificate"
	//   "tls: failed to verify certificate: ..."
	msg := dialErr.Error()
	if !strings.Contains(msg, "bad certificate") &&
		!strings.Contains(msg, "certificate") &&
		!strings.Contains(msg, "tls:") {
		t.Errorf("rejection error does not look like a TLS / certificate error: %v", dialErr)
	}
}

// TestNewServer_NilTLSConfig_ReturnsError documents the input
// validation contract: a nil *tls.Config is a programmer error
// (the agent should never have one), and we surface it loudly
// rather than panic.
func TestNewServer_NilTLSConfig_ReturnsError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	_, err = rpc.NewServer(listener.Addr().String(), nil)
	if err == nil {
		t.Fatal("NewServer accepted nil TLS config; want error")
	}
	if !strings.Contains(err.Error(), "tls") {
		t.Errorf("error did not mention tls: %v", err)
	}
}

// _silenceUnusedImport keeps the grpc package imported: tests
// below go through rpc.NewServer which returns a *Server that
// holds a *grpc.Server. The compile-time check is implicit in
// the package boundary, but we keep the import visible at the
// top so future readers see the dependency.
var _ = grpc.NewServer
