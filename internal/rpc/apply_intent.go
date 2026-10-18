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
//  2. For non-NONE capabilities, T5 stubs success with a
//     synthetic tx_id. T6 will replace this stub with the real
//     pre-snapshot → apply → post-snapshot → rollback state
//     machine. The stub exists so the gRPC wire + audit shape
//     can be exercised end-to-end at T5 without dragging in T6's
//     filesystem primitives.
//
// Audit logging:
//
// Both the refusal and the stub-success paths emit a single log
// line tagged with the intent_id. T7 will move this into the
// JSONL audit chain (issue #8 in followup); T5 uses the standard
// log package to keep the surface minimal.
package rpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
//   - tx_id: synthetic, "tx-<8-hex>". T6 replaces with the real
//     transaction ID after the apply state machine completes.
//   - audit_seq: 0 in T5 (T7 fills from the JSONL audit chain).
//   - error_message: empty on success.
//
// Response (refusal path, RF3):
//   - result: APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE.
//   - tx_id: "" (no transaction was started).
//   - audit_seq: 0.
//   - error_message: rf3RefusalMessage.
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
func (s *agentService) ApplyIntent(ctx context.Context, req *agentv1.ApplyIntentRequest) (*agentv1.ApplyIntentResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "ApplyIntent: nil request")
	}

	capability := s.snapshot.Capability()

	// RF3: refuse on NONE with a clear explanation.
	if capability == agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE {
		log.Printf("agent: ApplyIntent refused (RF3): intent_id=%q kind=%q target=%q — %s",
			req.IntentId, req.Kind, req.Target, rf3RefusalMessage)
		return &agentv1.ApplyIntentResponse{
			Result:       agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE,
			ErrorMessage: rf3RefusalMessage,
		}, nil
	}

	// Stub success. T6 wires the real pre-snapshot → apply →
	// post-snapshot → rollback state machine in this branch.
	txID := "tx-" + randomHex(8)
	log.Printf("agent: ApplyIntent stub success: intent_id=%q tx_id=%q backend=%v (T5 stub; T6 wires real state machine)",
		req.IntentId, txID, capability)
	return &agentv1.ApplyIntentResponse{
		Result: agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS,
		TxId:   txID,
	}, nil
}

// randomHex returns n random bytes encoded as hex. It panics if
// crypto/rand fails (which it doesn't on linux/darwin in
// practice — same rationale as Register's nodeID fallback).
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// Fall back to a deterministic-but-unique-enough
		// string so the daemon doesn't crash on a hypothetical
		// rand failure. The actual hex value is irrelevant —
		// T5's tx_id is a stub, T6 replaces it.
		return "deadbeef"
	}
	return hex.EncodeToString(buf)
}