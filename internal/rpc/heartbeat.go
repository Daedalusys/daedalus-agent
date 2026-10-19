// Heartbeat handler.
//
// Wire-protocol role:
//
//   - 周期性保活 (default 30s, 控制面可通过 Register 响应调整)
//   - ack + next_heartbeat_seconds + pending_intents 通知
//
// Behaviour:
//
//  1. Validate node_id matches the one Register assigned. If
//     they disagree the client is either stale (server rotated
//     and re-issued a new node_id) or hostile (replaying an
//     old Heartbeat). Either way we reject with FailedPrecondition.
//  2. Return ack=true with the configured heartbeat interval and
//     pending_intents=0 (T6 will track the real pending-intents
//     count from the local transaction queue).
//
// We intentionally do NOT log every Heartbeat: at 30s cadence a
// noisy log fills quickly. Diagnostics live in the agent's
// metrics layer (out of scope for T5).
package rpc

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// Heartbeat implements agentv1.AgentServiceServer.Heartbeat.
//
// Validation:
//   - node_id must match the node_id assigned at Register.
//     Failure is FailedPrecondition (the agent's state is
//     inconsistent with the client's expectation, but the
//     client can recover by calling Register again).
//
// Response:
//   - ack: true.
//   - next_heartbeat_seconds: heartbeatIntervalSeconds.
//   - pending_intents: 0 (T6 will fill from the local queue).
func (s *agentService) Heartbeat(ctx context.Context, req *agentv1.HeartbeatRequest) (*agentv1.HeartbeatResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "Heartbeat: nil request")
	}

	s.mu.Lock()
	assigned := s.nodeID
	s.mu.Unlock()

	if assigned == "" {
		// Heartbeat arrived before any Register — caller is
		// using a server they have not registered with. We
		// surface this as FailedPrecondition so the client
		// knows to call Register first.
		return nil, status.Error(codes.FailedPrecondition,
			"Heartbeat: agent has not yet been registered; call Register first")
	}
	if req.NodeId != assigned {
		return nil, status.Errorf(codes.FailedPrecondition,
			"Heartbeat: node_id mismatch (assigned=%q, got=%q); re-register required",
			assigned, req.NodeId)
	}

	return &agentv1.HeartbeatResponse{
		Ack:                  true,
		NextHeartbeatSeconds: heartbeatIntervalSeconds,
		PendingIntents:       0,
	}, nil
}