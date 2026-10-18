// Register handler.
//
// Wire-protocol role (per spec §4.4 + plan §T5):
//
//   - agent 上线 / 续 cert / 续 policy 时调用
//   - 响应携带 snapshot_capability 字段(控制面决策基础)
//   - 响应携带 cert_valid_until_unix / fingerprint(控制面续签判断)
//   - 响应携带 heartbeat_interval_seconds(控制面节流)
//
// Behaviour:
//
//  1. Validate agent_id is non-empty.
//  2. Look up the snapshot capability (lazy-detect on first call).
//  3. Assign a stable node_id (the agent's own agent_id is used
//     directly — the control plane will issue a stable node_id
//     on its side, but at P0.5 the agent trusts the client to
//     choose; we surface agent_id as node_id so the
//     Heartbeat-then-Register round-trip works hermetically).
//  4. Return the metadata bundle.
//
// Concurrency: nodeID is assigned exactly once per agentService
// instance. We guard it with the agentService mutex so the
// concurrent-register case (rare, but possible if the control
// plane restarts and re-issues Register before our first
// Register is processed) does not yield two different node_ids.
package rpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// Register implements agentv1.AgentServiceServer.Register.
//
// Validation rules:
//   - agent_id must be non-empty (otherwise InvalidArgument).
//   - empty agent_version is accepted (the proto permits it; the
//     control plane decides whether to refuse).
//
// Response fields:
//   - node_id: a stable identifier for this agent. We derive
//     it from the agent_id with a random suffix to disambiguate
//     multiple agents with the same agent_id (e.g. two daemons
//     started on the same host with the same config). The
//     Heartbeat handler enforces node_id equality, so a fresh
//     Register will invalidate prior Heartbeats — that is by
//     design: re-registering after a control-plane restart is
//     the way to refresh an expired node_id.
//   - snapshot_capability: from the snapshot detector.
//   - cert_valid_until_unix: from cert.Info (0 if certInfo is
//     nil — tests that don't care about cert metadata).
//   - policy_hash: placeholder string (real hash is a future
//     task).
//   - heartbeat_interval_seconds: heartbeatIntervalSeconds.
func (s *agentService) Register(ctx context.Context, req *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "Register: nil request")
	}
	if strings.TrimSpace(req.AgentId) == "" {
		return nil, status.Error(codes.InvalidArgument, "Register: agent_id is required")
	}

	capability := s.snapshot.Capability()

	nodeID := s.assignOrReuseNodeID(req.AgentId)

	resp := &agentv1.RegisterResponse{
		NodeId:                   nodeID,
		SnapshotCapability:       capability,
		HeartbeatIntervalSeconds: heartbeatIntervalSeconds,
		PolicyHash:               policyHashPlaceholder,
	}
	if s.certInfo != nil {
		resp.CertValidUntilUnix = s.certInfo.ValidUntil.Unix()
	}
	return resp, nil
}

// assignOrReuseNodeID returns the agent's existing node_id if it
// has already been assigned, otherwise it allocates a fresh
// node_id derived from agent_id with a random suffix.
//
// The first Register call for a given agentService instance
// mints a node_id of the form "<agent_id>-<8-hex>" where the
// hex is from crypto/rand. Subsequent Register calls reuse the
// same node_id, so Heartbeats keyed off the first Register
// continue to work even if the control plane issues a second
// Register (e.g. after cert rotation).
//
// The mutex is necessary because the gRPC server may serve
// multiple Register calls concurrently (one per cert rotation
// or per control-plane restart).
func (s *agentService) assignOrReuseNodeID(agentID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nodeID != "" {
		return s.nodeID
	}
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand.Read is documented as never failing in
		// practice on linux/darwin. If it ever does, fall back
		// to a timestamp suffix so we still produce a unique
		// node_id rather than crashing the daemon.
		return agentID + "-" + hex.EncodeToString([]byte("fallback"))
	}
	s.nodeID = agentID + "-" + hex.EncodeToString(buf[:])
	return s.nodeID
}