// agent_service.go holds the T3 stub for the AgentService gRPC
// interface. T5 will replace this stub with real handlers; the
// T3 wiring just needs to compile and serve so the daemon
// actually listens on :9443 with mTLS.
//
// We embed agentv1.UnimplementedAgentServiceServer, which
// provides default implementations that return codes.Unimplemented
// for every RPC. That is the canonical "skeleton" pattern when
// adding a new service: it lets the wire-protocol and TLS layers
// be exercised without a real handler, and any unimplemented RPC
// surfaces as Unimplemented to the client (which is itself a
// clear signal — better than silent drop).
package rpc

import (
	"context"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// agentService is the T3 stub implementation of
// agentv1.AgentServiceServer. All RPCs return codes.Unimplemented
// via the embedded UnimplementedAgentServiceServer's defaults.
//
// T5 will replace this with the real handlers; the production
// wiring in NewServer is expected to swap in a real
// implementation then.
type agentService struct {
	agentv1.UnimplementedAgentServiceServer
}

// Compile-time assertion that *agentService satisfies the
// interface. If agentv1.AgentServiceServer grows a method
// (forward-compat), the build will fail at this line — which is
// exactly the signal we want: a proto change we must handle.
//
// NOTE: we ignore the ctx — the embedded stub ignores it too,
// and this file's job is wiring, not behaviour.
var _ agentv1.AgentServiceServer = (*agentService)(nil)

// _silenceUnusedImport keeps "context" imported for the doc
// comment above (which mentions ctx). It is not otherwise used
// in this file.
var _ = context.Background
