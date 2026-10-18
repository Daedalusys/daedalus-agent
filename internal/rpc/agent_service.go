// agent_service.go owns the agentService struct and its 5 RPC
// handlers. T3 left an Unimplemented stub here; T5 wires the
// real handlers on top of it.
//
// Each RPC handler lives in its own file (register.go,
// heartbeat.go, apply_intent.go, snapshot_request.go,
// report_status.go) — this file is the struct composition +
// shared helpers + the per-handler wiring.
//
// We embed agentv1.UnimplementedAgentServiceServer, which
// provides default implementations that return codes.Unimplemented
// for any future RPC the proto grows. T5 implements the 5 RPCs
// in the spec; the embed is forward-compat for additions.
package rpc

import (
	"sync"
	"time"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/cert"
	"github.com/Daedalusys/daedalus-agent/internal/config"
	"github.com/Daedalusys/daedalus-agent/internal/snapshot"
)

// heartbeatIntervalSeconds is the interval the agent advertises
// in its Register response. The control plane can override this
// (the proto's HeartbeatIntervalSeconds field is editable), but
// until it does, this constant is the answer.
//
// 30s is chosen to balance:
//   - Low heartbeat rate: less control-plane-side load, less
//     network chatter.
//   - High heartbeat rate: faster dead-node detection.
//
// 30s is a standard compromise for fleet agents; we can revisit
// if the control plane flags false-positive dead nodes.
const heartbeatIntervalSeconds = 30

// policyHashPlaceholder is the placeholder policy_hash the
// agent returns in Register. The real policy hash comes from
// hashing the active /etc/daedalus-agent/policy.toml file; T5
// stubs the placeholder because the policy loader itself is
// out of scope for P0.5 (issue #8 in the followup queue).
//
// We deliberately return a recognisable, non-empty string
// rather than an empty field so the control plane can tell
// "agent does not yet compute the real hash" apart from
// "agent has empty policy". A future task replaces this with
// the real hash.
const policyHashPlaceholder = "unhashed-policy-v0"

// agentService is the T5 production implementation of
// agentv1.AgentServiceServer. All five RPCs are real handlers
// (each in its own file); the embed keeps us forward-compatible
// if the proto grows new RPCs.
//
// Concurrency: agentService's mutable state (nodeID) is written
// exactly once during Register and only read after. We use a
// mutex so future extensions (e.g. an in-memory pending-intent
// queue in T6) do not have to think about whether the field is
// race-free.
//
// All dependencies (cfg, certInfo, snapshot) are set at
// construction time and not mutated thereafter; the
// snapshot.Detector's own internal cache is itself mutex-guarded.
type agentService struct {
	agentv1.UnimplementedAgentServiceServer

	mu sync.Mutex // protects nodeID (assigned once at first Register)

	// cfg is the daemon's runtime config (agent_id, cert
	// paths, etc.). The Register handler reads agent_id from
	// here. The cert paths in cfg are consumed by main() to
	// construct the TLS config; the handlers themselves do not
	// touch the filesystem.
	cfg *config.Config

	// certInfo carries the cert's NotAfter + SHA-256
	// fingerprint for the Register response. nil is acceptable
	// for tests that don't care about the cert metadata; the
	// Register handler emits CertValidUntilUnix=0 and an empty
	// fingerprint in that case (the proto permits both).
	certInfo *cert.Info

	// snapshot is the agent's snapshot-capability probe.
	// Register reads it; ApplyIntent / SnapshotRequest consult
	// it to enforce RF3 (refuse on NONE) and to refuse
	// SnapshotRequest with FailedPrecondition.
	snapshot *snapshot.Detector

	// nodeID is the control-plane-assigned identifier for
	// this agent. It is empty until the first successful
	// Register, then stable for the daemon's lifetime. The
	// Heartbeat handler rejects requests whose node_id does
	// not match — preventing a stale client from driving an
	// old agent registration.
	nodeID string

	// startedAt is the wall-clock the service was constructed.
	// It is informational only in T5 (a future health-check
	// RPC could surface uptime); not yet surfaced.
	startedAt time.Time
}

// Compile-time assertion that *agentService satisfies the
// interface. If agentv1.AgentServiceServer grows a method
// (forward-compat), the build will fail at this line — which is
// exactly the signal we want: a proto change we must handle.
var _ agentv1.AgentServiceServer = (*agentService)(nil)

// newAgentService constructs a fresh agentService with the
// supplied dependencies. main() (or a test helper) calls this
// when bringing up the RPC server.
//
// startedAt is captured here so uptime is measured from
// construction (which is shortly before serve, modulo
// listener-bind time).
func newAgentService(cfg *config.Config, certInfo *cert.Info, det *snapshot.Detector) *agentService {
	return &agentService{
		cfg:       cfg,
		certInfo:  certInfo,
		snapshot:  det,
		startedAt: time.Now().UTC(),
	}
}