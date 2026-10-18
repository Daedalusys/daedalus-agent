// Package rpc owns the daedalus-agent gRPC server.
//
// The server is a thin shell: it accepts a listener address and a
// *tls.Config, registers the unimplemented AgentService stub (T5
// will graft the real handlers on), and exposes the standard
// grpc.Server lifecycle (Serve / GracefulStop / Stop).
//
// T3 does NOT implement Register / Heartbeat / ApplyIntent /
// SnapshotRequest / ReportStatus handlers; those are T5 work.
// T3's job is the wire-protocol scaffolding:
//
//   - mTLS server (RequireAndVerifyClientCert).
//   - Listener bound to :9443 (configurable via Config).
//   - GracefulStop wired into the daemon's lifecycle.
//
// Spec refs:
//
//   - §4.4 wire protocol, transport mTLS.
//   - §7.3 C3 cert lifecycle, validated by the embedded cert
//     package; this package only consumes the resulting
//     *tls.Config.
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

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// DefaultListenAddr is the port the gRPC server binds to. The
// brief pins 9443 explicitly; surfacing it as a constant lets
// tests and the systemd unit agree on one source of truth.
const DefaultListenAddr = ":9443"

// Server wraps a *grpc.Server with the daedalus-agent defaults
// already wired in: TLS credentials from the supplied *tls.Config
// (which the cert.Loader populates from PEM files) and the
// AgentService stub registered as the handler.
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
// The AgentService stub is registered immediately. At T5 the
// caller may replace the stub by adding a real handler alongside
// (RegisterAgentServiceServer panics if called twice, so the
// production wiring is "register the real handler in main, no
// stub"). For T3 the stub exists so a freshly-built agent can
// answer gRPC health checks or be probed for liveness even before
// T5 lands.
func NewServer(addr string, tlsCfg *tls.Config) (*Server, error) {
	if addr == "" {
		addr = DefaultListenAddr
	}
	if tlsCfg == nil {
		return nil, fmt.Errorf("rpc: tls config is required")
	}
	creds := credentials.NewTLS(tlsCfg)
	gs := grpc.NewServer(grpc.Creds(creds))
	agentv1.RegisterAgentServiceServer(gs, &agentService{})
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
