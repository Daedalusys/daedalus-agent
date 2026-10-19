// Package rpc owns the daedalus-agent gRPC server.
//
// The server is a thin shell: it accepts a listener address, a
// *tls.Config, and a ServiceDeps bundle; registers the
// agentService implementation; and exposes the standard
// grpc.Server lifecycle (Serve / GracefulStop / Stop).
//
// Why a separate package rather than folding into internal/daemon:
// gRPC has its own opinions about graceful shutdown that we want
// isolated. If the daemon later needs additional RPC servers
// (status-only socket, pprof, etc.) they each become a small file
// in this package rather than another dependency on daemon.
package rpc

import (
	"crypto/tls"
	"fmt"
	"net"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/audit"
	"github.com/Daedalusys/daedalus-agent/internal/cert"
	"github.com/Daedalusys/daedalus-agent/internal/config"
	"github.com/Daedalusys/daedalus-agent/internal/rollback"
	"github.com/Daedalusys/daedalus-agent/internal/snapshot"
	"github.com/Daedalusys/daedalus-agent/internal/tx"
)

// DefaultListenAddr is the port the gRPC server binds to. The
// brief pins 9443 explicitly; surfacing it as a constant lets
// tests and the systemd unit agree on one source of truth.
const DefaultListenAddr = ":9443"

// ServiceDeps bundles the dependencies the agentService needs.
//
// Required for production: Cfg, CertInfo, Snapshot, AuditSeq.
// Optional (zero-value defaults apply):
//   - Backend:  if non-nil, used as the rollback backend
//     instead of SelectBackend(capability) for both ApplyIntent
//     and SnapshotRequest. Tests use this to inject a mock
//     backend that does not require a real btrfs / rsync on
//     the host.
//   - TxFactory: if non-nil, used instead of tx.New to
//     construct the per-RPC transaction in ApplyIntent.
//     Production leaves this nil (handler falls through to
//     tx.New with a stub apply step).
//
// Cfg, CertInfo, Snapshot and Backend are retained by reference —
// the caller must not mutate them after passing them in.
type ServiceDeps struct {
	// Cfg is the daemon's runtime config. The Register handler
	// reads agent_id from here; ApplyIntent reads DataDir for
	// snapshot paths. nil is permitted only in tests that
	// don't reach those handlers; production always supplies a
	// non-nil cfg.
	Cfg *config.Config

	// CertInfo carries the cert's NotAfter + SHA-256
	// fingerprint for the Register response. nil is permitted
	// (Register then emits CertValidUntilUnix=0 and an empty
	// fingerprint).
	CertInfo *cert.Info

	// Snapshot is the agent's snapshot-capability probe.
	// nil is treated as a fresh, empty cache (first
	// Capability() call probes the host).
	Snapshot *snapshot.Detector

	// AuditSeq is the per-process monotonic audit sequence
	// counter shared by ApplyIntent (and future audit-emitting
	// handlers). nil is permitted — the constructor allocates
	// a private counter in that case (only useful in tests
	// that don't care about absolute sequence numbers).
	AuditSeq *atomic.Uint64

	// Backend, if non-nil, overrides the per-RPC backend
	// selection in ApplyIntent and SnapshotRequest. Tests use
	// this to inject a recording fake (avoids requiring real
	// btrfs / rsync on the test runner). Production leaves
	// this nil — handlers call SelectBackend per RPC.
	Backend rollback.Backend

	// TxFactory, if non-nil, overrides tx.New in ApplyIntent.
	// Production leaves this nil and accepts the default
	// (tx.New with the T6 apply stub). Tests can substitute a
	// canned-response Tx.
	TxFactory TxFactory

	// Audit is the per-process hash-chained audit log. nil is
	// permitted — ReportStatus then returns an empty response
	// (the documented "no audit yet" wire shape). Production
	// always supplies a non-nil audit (constructed in main()
	// from cfg.AuditDir / cfg.DataDir). Tests may pass nil
	// when they don't exercise ReportStatus.
	Audit *audit.Local
}

// TxFactory constructs a per-RPC ApplyIntent transaction. Tests
// inject a canned-response fake; production passes nil to
// ServiceDeps and accepts the default tx.New.
//
// The audit parameter is the per-process hash-chained audit
// log; tx.Local writes success / failure events to it. Tests
// may pass nil to opt out of audit (handy for unit tests
// that don't want to set up a chain file).
type TxFactory func(rollback.Backend, string, *atomic.Uint64, *audit.Local) tx.Tx

// Server wraps a *grpc.Server with the daedalus-agent defaults
// already wired in: TLS credentials from the supplied *tls.Config
// (which the cert.Loader populates from PEM files) and the
// AgentService implementation registered as the handler.
//
// Server is safe to call Serve / GracefulStop / Stop from any
// goroutine; the underlying grpc.Server serialises these.
type Server struct {
	grpc *grpc.Server
	addr string
}

// NewServer constructs a Server bound to addr (e.g. ":9443" or
// "127.0.0.1:0" for tests). The TLS config must have at least one
// certificate and a non-nil ClientCAs if mTLS is to work; this
// function does NOT add defaults — the cert.Loader is the source
// of truth for mTLS posture.
//
// The agentService implementation is registered immediately
// against the gRPC server. At T5 the production wiring is
// "construct the deps, call NewServer, no further registration
// needed"; tests that want to drive a specific snapshot
// capability pre-load it via detector.SetCapability before
// calling NewServer.
func NewServer(addr string, tlsCfg *tls.Config, deps ServiceDeps) (*Server, error) {
	if addr == "" {
		addr = DefaultListenAddr
	}
	if tlsCfg == nil {
		return nil, fmt.Errorf("rpc: tls config is required")
	}
	det := deps.Snapshot
	if det == nil {
		det = snapshot.NewDetector()
	}
	if deps.AuditSeq == nil {
		deps.AuditSeq = new(atomic.Uint64)
	}
	creds := credentials.NewTLS(tlsCfg)
	gs := grpc.NewServer(grpc.Creds(creds))
	agentv1.RegisterAgentServiceServer(gs, newAgentService(deps.Cfg, deps.CertInfo, det, deps.AuditSeq, deps.Backend, deps.TxFactory, deps.Audit))
	return &Server{grpc: gs, addr: addr}, nil
}

// Serve binds the listener and serves until grpc.Server.Stop is
// called. Serve blocks; run it from a goroutine in production.
//
// Errors from net.Listen are surfaced so the caller (main) can
// distinguish "address in use" from "TLS handshake failed" etc.
func (s *Server) Serve(ln net.Listener) error {
	return s.grpc.Serve(ln)
}

// Listener parses s.addr into a TCP listener without serving
// yet. Production code uses NewListener + go Serve in main so
// errors at bind time are not hidden in a goroutine. Tests use
// NewListener to grab a kernel-assigned port via ":0".
func (s *Server) Listener() (net.Listener, error) {
	return net.Listen("tcp", s.addr)
}

// GracefulStop signals existing RPCs to drain and refuses new
// ones, returning when all in-flight RPCs have completed or the
// supplied context expires.
//
// Called from the daemon's shutdown hook (added by T3 alongside
// this package). The daemon bounds the call with its 30s
// ShutdownTimeout; if GracefulStop exceeds it the daemon's hook
// returns whatever ctx.Err() says and we rely on the supervisor
// (systemd) to SIGKILL.
func (s *Server) GracefulStop() {
	s.grpc.GracefulStop()
}

// Stop is the hard-cancel sibling of GracefulStop. It cancels
// every active stream and active RPC; intended for test cleanup
// where we do not need a graceful drain window.
func (s *Server) Stop() {
	s.grpc.Stop()
}

// Addr returns the address the listener was bound to. Only valid
// after Listener() has been called.
func (s *Server) Addr() string {
	return s.addr
}
