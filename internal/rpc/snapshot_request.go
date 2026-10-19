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
//  2. For non-NONE capabilities, T6 invokes the snapshot
//     backend (btrfs / rsync via internal/rollback) to take
//     a real snapshot and returns the snapshot_id derived
//     from the snapshot path. The btrfs / rsync split is
//     transparent to the control plane — only backend_used
//     names which one was used.
//
// Rationale for the FailedPrecondition (not body-refusal) split:
//
//	ApplyIntent's refusal carries useful information (the
//	intent_id was received, the audit row was emitted) that
//	the control plane should record even on refusal. Hence the
//	in-body refusal code.
//
//	SnapshotRequest's refusal carries no useful information —
//	no snapshot was taken, no audit row makes sense. The
//	control plane's "we wanted a snapshot, did we get one?"
//	question is best answered by gRPC status codes; codes.OK +
//	an empty SnapshotResponse would mask the failure.
package rpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"path/filepath"
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
//   - snapshot_id: derived from the snapshot path, "snap-<hex>".
//     The hex portion is random; the path on disk is
//     <dataDir>/snapshots/<id>.snap (btrfs subvolume) or
//     <dataDir>/snapshots/<id>/ (rsync file-copy dir).
//   - backend_used: the agent's detected snapshot capability
//     (BTRFS_SUBVOLUME or RSYNC_TARGET for P0.5).
//   - created_at_unix: time.Now().Unix().
//
// Response (refusal path, NONE):
//   - gRPC status codes.FailedPrecondition with a clear
//     message naming the missing backends.
//
// Failure paths:
//
//	A real backend failure (btrfs subvolume command error,
//	rsync ENOSPC, etc.) returns codes.Internal with the
//	wrapped error so the control plane's log distinguishes
//	"agent can't reach the snapshot primitive" from
//	"agent has no snapshot primitive at all".
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

	backend, err := s.selectedBackend()
	if err != nil {
		// Defensive — should not happen given the NONE check.
		log.Printf("agent: SnapshotRequest backend selection failed after RF3 admission: reason=%q cap=%v err=%v",
			req.Reason, capability, err)
		return nil, status.Error(codes.FailedPrecondition,
			"SnapshotRequest: backend selection failed after RF3 admission")
	}

	// Build snapshot paths. We snapshot the dataDir's
	// intent-target directory (the same target ApplyIntent
	// modifies). The snapshot_id is randomised so concurrent
	// SnapshotRequest calls don't collide on the snapshot path.
	snapID := "snap-" + randomHexID(8)
	sourcePath := filepath.Join(s.cfg.DataDir, "intent-target")
	snapshotPath := filepath.Join(s.cfg.DataDir, "snapshots", snapID)

	if err := backend.CreateSnapshot(sourcePath, snapshotPath); err != nil {
		log.Printf("agent: SnapshotRequest backend CreateSnapshot failed: snapshot_id=%q backend=%v err=%v",
			snapID, capability, err)
		return nil, status.Error(codes.Internal,
			"SnapshotRequest: backend CreateSnapshot failed: "+err.Error())
	}

	log.Printf("agent: SnapshotRequest success: snapshot_id=%q backend=%v reason=%q path=%q",
		snapID, capability, req.Reason, snapshotPath)
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
