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
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/audit"
	"github.com/Daedalusys/daedalus-agent/internal/cert"
	"github.com/Daedalusys/daedalus-agent/internal/config"
	"github.com/Daedalusys/daedalus-agent/internal/rollback"
	"github.com/Daedalusys/daedalus-agent/internal/snapshot"
	"github.com/Daedalusys/daedalus-agent/internal/tx"
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

	// auditSeq is the per-process monotonic audit sequence
	// counter. ApplyIntent (via the tx state machine) bumps it
	// on each tx outcome (success / transient / persistent).
	// nil is permitted only in tests that don't reach
	// ApplyIntent; the constructor seeds a private counter
	// otherwise.
	auditSeq *atomic.Uint64

	// audit is the per-process hash-chained audit log (T7).
	// ReportStatus streams events from it; tx.Local writes
	// success / failure events to it on every ApplyIntent
	// outcome. nil is permitted for tests that don't
	// exercise ReportStatus / ApplyIntent; the
	// report_status handler then returns an empty response
	// (the documented "no audit yet" wire shape).
	audit *audit.Local

	// backend is an optional override for the per-RPC backend
	// selection. If non-nil, it is used as-is by ApplyIntent
	// and SnapshotRequest (skipping SelectBackend). Production
	// leaves this nil; tests inject a mock backend so the
	// handler exercises the wired-up state machine without
	// requiring real btrfs/rsync on the test runner.
	backend rollback.Backend

	// txFactory, if non-nil, is used instead of tx.New to
	// construct the per-RPC ApplyIntent transaction. nil
	// means use the production default (tx.New with the T6
	// apply stub).
	txFactory TxFactory

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
// T6 adds auditSeq / backend / txFactory:
//   - auditSeq is bumped by tx.Local on every ApplyIntent outcome
//     (success / transient / persistent). Tests may pass nil —
//     the agentService seeds a private counter in that case.
//   - backend is an optional override for ApplyIntent /
//     SnapshotRequest. Production passes nil and the handler
//     selects via rollback.SelectBackend per RPC. Tests pass a
//     fake that records invocations without touching the host.
//   - txFactory is the injection point for ApplyIntent's tx
//     construction. Production passes nil and the handler uses
//     the default tx.New with the T6 apply stub. Tests can
//     substitute a fake.
//
// T7 adds audit:
//   - audit is the per-process hash-chained audit log. nil
//     is permitted for tests that don't exercise
//     ReportStatus / ApplyIntent; the corresponding handlers
//     then return empty responses (or no audit writes,
//     respectively).
//
// startedAt is captured here so uptime is measured from
// construction (which is shortly before serve, modulo
// listener-bind time).
func newAgentService(cfg *config.Config, certInfo *cert.Info, det *snapshot.Detector, auditSeq *atomic.Uint64, backend rollback.Backend, txFactory TxFactory, auditLog *audit.Local) *agentService {
	if auditSeq == nil {
		auditSeq = new(atomic.Uint64)
	}
	return &agentService{
		cfg:       cfg,
		certInfo:  certInfo,
		snapshot:  det,
		auditSeq:  auditSeq,
		backend:   backend,
		txFactory: txFactory,
		audit:     auditLog,
		startedAt: time.Now().UTC(),
	}
}

// selectedBackend returns the rollback backend for the
// current RPC invocation. If a test-injected backend is set,
// it is returned as-is (the detector is bypassed). Otherwise
// the agent selects via rollback.SelectBackend based on the
// live detector capability.
//
// Returns ErrNoBackend when the capability is NONE / UNSPECIFIED
// / an unknown enum value. Callers are responsible for checking
// the snapshot capability separately (RF3 admission check).
func (s *agentService) selectedBackend() (rollback.Backend, error) {
	if s.backend != nil {
		return s.backend, nil
	}
	return rollback.SelectBackend(s.snapshot.Capability())
}

// buildTx constructs the per-RPC ApplyIntent transaction. If a
// test-injected txFactory is set, it is used; otherwise the
// production default (tx.New with the T6 apply stub) is used.
// The passed backend and auditSeq are forwarded to the tx.
//
// T7: also forwards the per-process hash-chained audit log
// (shared with report_status.go). tx.Local writes success /
// failure events to it on every ApplyIntent outcome.
func (s *agentService) buildTx(backend rollback.Backend) tx.Tx {
	if s.txFactory != nil {
		return s.txFactory(backend, s.cfg.DataDir, s.auditSeq, s.audit)
	}
	return tx.New(backend, applyStub, s.cfg.DataDir, s.auditSeq, s.audit)
}

// applyStub is the T6 apply step. It only logs the intent and
// returns nil; T7+ replaces it with a real package manager /
// filesystem executor that respects ctx.Done().
//
// Exported at package scope (not method) so tx.New can capture
// it as an ApplyFunc without instantiating agentService.
func applyStub(_ context.Context, intent *agentv1.ApplyIntentRequest) error {
	log.Printf("agent: apply stub (T6): intent_id=%q kind=%q target=%q (T7+ replaces with real ops)",
		intent.IntentId, intent.Kind, intent.Target)
	return nil
}
