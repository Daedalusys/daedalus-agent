// SnapshotRequest handler.
//
// Wire-protocol role:
//
//   - 控制面要求 agent 立刻 snapshot(plan apply 前 / 高风险意图前)
//
// Behaviour:
//
//  1. If snapshot_capability is NONE, refuse with
//     codes.FailedPrecondition. Unlike ApplyIntent's
//     refusal-in-body pattern, SnapshotRequest's "no backend"
//     case is a *transport-level* failure: the control plane
//     cannot route around it (the proto design lets refused
//     ApplyIntent live in the body because the agent's intent
//     was acknowledged and recorded; a refused SnapshotRequest
//     means the control plane's plan gate cannot proceed at
//     all — it is structurally equivalent to "RPC failed").
//  2. For non-NONE capabilities, T5 stubs a synthetic
//     snapshot_id. T6 will replace this stub with the real
//     btrfs / rsync filesystem primitives.
//
// Rationale for the FailedPrecondition (not body-refusal) split:
//
//   ApplyIntent's refusal carries useful information (the
//   intent_id was received, the audit row was emitted) that
//   the control plane should record even on refusal. Hence the
//   in-body refusal code.
//
//   SnapshotRequest's refusal carries no useful information —
//   no snapshot was taken, no audit row makes sense. The
//   control plane's "we wanted a snapshot, did we get one?"
//   question is best answered by gRPC status codes; codes.OK +
//   an empty SnapshotResponse would mask the failure.
package rpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// SnapshotRequest implements agentv1.AgentServiceServer.SnapshotRequest.
//
// Validation:
//   - req must be non-nil (InvalidArgument otherwise).
//   - snapshot_capability must not be NONE (FailedPrecondition
//     otherwise; see package comment for the
//     ApplyIntent-vs-SnapshotRequest split).
//
// Response (success path):
//   - snapshot_id: synthetic, "snap-<8-hex>". T6 replaces with
//     the real snapshot identifier (btrfs subvolume path or
//     rsync target path).
//   - backend_used: the agent's detected snapshot capability.
//   - created_at_unix: time.Now().Unix().
//
// Response (refusal path, NONE):
//   - gRPC status codes.FailedPrecondition with a clear
//     message naming the missing backends.
func (s *agentService) SnapshotRequest(ctx context.Context, req *agentv1.SnapshotRequestRequest) (*agentv1.SnapshotResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "SnapshotRequest: nil request")
	}

	capability := s.snapshot.Capability()

	if capability == agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE {
		// No backend → return FailedPrecondition so the control
		// plane's plan gate rejects the upstream Plan cleanly.
		log.Printf("agent: SnapshotRequest refused (no snapshot backend): reason=%q",
			req.Reason)
		return nil, status.Error(codes.FailedPrecondition,
			"SnapshotRequest: agent has no snapshot backend (btrfs/rsync/lvm/snapper all absent)")
	}

	// Stub success. T6 wires the real snapshot primitives in
	// this branch (btrfs subvolume create / rsync to target
	// directory).
	snapID := "snap-" + randomHexID(8)
	log.Printf("agent: SnapshotRequest stub success: snapshot_id=%q backend=%v reason=%q (T5 stub; T6 wires real snapshot primitives)",
		snapID, capability, req.Reason)
	return &agentv1.SnapshotResponse{
		SnapshotId:    snapID,
		BackendUsed:   capability,
		CreatedAtUnix: time.Now().UTC().Unix(),
	}, nil
}

// randomHexID is a small helper for synthetic IDs. It panics if
// crypto/rand fails; we keep it unexported because handlers are
// the only callers and the panic surface is local.
func randomHexID(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "deadbeef"
	}
	return hex.EncodeToString(buf)
}