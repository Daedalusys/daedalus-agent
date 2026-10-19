// ApplyIntent handler — RF3 critical.
//
// Wire-protocol role:
//
//   - 包/文件意图落地
//   - agent 跑本地事务 + 落审计 + 一次性回报
//
// Behaviour:
//
//  1. **RF3钉** (plan §RF3 + spec §5.2): if the agent's snapshot
//     capability is NONE, refuse immediately. Return
//     APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE with
//     a clear error_message explaining *why* — the operator /
//     control plane reviewer reading the failure must understand
//     "this machine has no rollback path" and not "best-effort
//     will handle it". The error message is structured:
//
//        "this machine has no snapshot backend
//         (btrfs/rsync/lvm/snapper all absent);
//         ApplyIntent refused per fail-closed policy;
//         only local audit + state report available"
//
//     Note we say "fail-closed policy", not "best-effort policy" —
//     the spec's "best-effort rollback" phrasing reads to a hurried
//     reviewer as "we will handle it" (false); the actual contract
//     is "ApplyIntent refuses; the host has no recovery path".
//
//  2. For non-NONE capabilities, T6 wires the real pre-snapshot
//     → apply → post-snapshot / rollback state machine via
//     internal/tx.Local. The state machine handles pre-snapshot
//     failure, apply failure with rollback success (TRANSIENT_FAILURE,
//     safe to retry), and apply failure with rollback failure
//     (PERSISTENT_FAILURE, host may be in undefined state).
//
// Audit logging:
//
// Every state-machine outcome emits a log line tagged with the
// intent_id (the apply stub also emits its own log line). T7
// will move this into the JSONL audit chain (issue #8 in
// followup); T6 uses the standard log package to keep the
// surface minimal.
package rpc

import (
	"context"
	"log"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// rf3RefusalMessage is the human-readable explanation surfaced
// when ApplyIntent is invoked on a host with no snapshot backend.
// The wording is intentionally structured so the control plane's
// failure-bucket rendering picks up both the "no snapshot
// backend" phrase (used by alerting rules) and the
// "ApplyIntent refused" phrase (used by reviewer guidance).
//
// We DO NOT use the word "best-effort" alone — see the package
// comment for the human-reviewer misreading concern.
const rf3RefusalMessage = "this machine has no snapshot backend (btrfs/rsync/lvm/snapper all absent); ApplyIntent refused per fail-closed policy; only local audit + state report available"

// ApplyIntent implements agentv1.AgentServiceServer.ApplyIntent.
//
// Validation:
//   - req must be non-nil (InvalidArgument otherwise).
//   - RF3: snapshot_capability must not be NONE
//     (REFUSED_SNAPSHOT_CAPABILITY_NONE otherwise; see constant
//     rf3RefusalMessage for the explanation).
//
// Response (success path):
//   - result: APPLY_INTENT_RESULT_SUCCESS.
//   - tx_id: deterministic from intent_id, "tx-<intent_id>".
//   - audit_seq: monotonic counter (per-process).
//   - error_message: empty on success.
//
// Response (refusal path, RF3):
//   - result: APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE.
//   - tx_id: "" (no transaction was started).
//   - audit_seq: monotonic counter at the refusal point.
//   - error_message: rf3RefusalMessage.
//
// Response (failure paths via tx state machine):
//   - pre-snapshot fail: TRANSIENT_FAILURE with explanation.
//   - apply fail + rollback ok: TRANSIENT_FAILURE
//     ("rolled back successfully").
//   - apply fail + rollback fail: PERSISTENT_FAILURE
//     (host may be in undefined state).
//
// Error semantics:
//
// We do NOT return a gRPC status error on the RF3 refusal — the
// refusal is a *successful* RPC whose body carries a refusal
// code. This matches the proto's design (refused_* are values of
// ApplyIntentResult, not separate status codes). Returning
// codes.FailedPrecondition here would force the control plane
// to distinguish "RPC transport failure" from "logical refusal",
// and the proto is explicit that the latter lives in the body.
//
// True transport failures (e.g. the tx state machine panicking)
// DO return a non-nil error — they map to codes.Internal via
// the gRPC default handler.
func (s *agentService) ApplyIntent(ctx context.Context, req *agentv1.ApplyIntentRequest) (*agentv1.ApplyIntentResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "ApplyIntent: nil request")
	}

	capability := s.snapshot.Capability()

	// RF3: refuse on NONE with a clear explanation.
	if capability == agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE {
		// audit_seq is bumped even on refusal so the
		// control plane's audit log shows every
		// admission-control event.
		auditSeq := s.auditSeq.Add(1)
		log.Printf("agent: ApplyIntent refused (RF3): intent_id=%q kind=%q target=%q audit_seq=%d — %s",
			req.IntentId, req.Kind, req.Target, auditSeq, rf3RefusalMessage)
		return &agentv1.ApplyIntentResponse{
			Result:       agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE,
			ErrorMessage: rf3RefusalMessage,
			AuditSeq:     auditSeq,
		}, nil
	}

	// Non-NONE path: build a per-RPC tx and run it. The
	// tx-local state machine handles snapshot / apply /
	// rollback semantically — we just forward the outcome.
	backend, err := s.selectedBackend()
	if err != nil {
		// Should not happen given the RF3 admission check
		// above, but the tx-local surface MUST be defended
		// against a malformed detector state (e.g. an
		// unexpected enum from a future proto change).
		auditSeq := s.auditSeq.Add(1)
		log.Printf("agent: ApplyIntent backend selection failed after RF3 admission: intent_id=%q cap=%v err=%v audit_seq=%d",
			req.IntentId, capability, err, auditSeq)
		return &agentv1.ApplyIntentResponse{
			Result:       agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE,
			ErrorMessage: rf3RefusalMessage,
			AuditSeq:     auditSeq,
		}, nil
	}

	tx := s.buildTx(backend)
	return tx.Run(ctx, req)
}