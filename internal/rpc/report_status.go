// ReportStatus handler.
//
// Wire-protocol role:
//
//   - 审计事件回报;agent 完成本地事务后批量推
//
// Behaviour:
//
// T5 returns an empty events list with final_seq=0. The
// hash-chained JSONL audit log (spec §3) and the
// ReportStatus-from-disk replay are T7 work (issue #8 in
// followup). T5 stubs the response so the wire shape and the
// pending_intents / audit_seq fields can be exercised
// end-to-end at the gRPC layer.
//
// When T7 lands, the stub body changes to:
//
//   1. Read events from <DataDir>/audit.jsonl whose seq >=
//      since_audit_seq, capped at max_events.
//   2. Build the AuditEvent protos with prev_hash / current_hash
//      from the JSONL hash chain.
//   3. Return events + final_seq = the highest seq we returned.
//
// Until then, returning empty is the contract — the control
// plane treats an empty response as "agent has nothing to report
// yet", which is exactly true for a freshly-started agent.
package rpc

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// ReportStatus implements agentv1.AgentServiceServer.ReportStatus.
//
// Validation:
//   - req must be non-nil (InvalidArgument otherwise).
//
// Response (T5 stub):
//   - events: empty list.
//   - final_seq: 0.
//
// T7 will fill events from the local JSONL audit log and final_seq
// from the highest seq returned.
func (s *agentService) ReportStatus(ctx context.Context, req *agentv1.ReportStatusRequest) (*agentv1.ReportStatusResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "ReportStatus: nil request")
	}
	// T5 stub: no audit chain yet; T7 (issue #8) replaces this
	// with the JSONL replay. Returning an empty events list
	// with final_seq=0 is the documented "agent has nothing to
	// report" wire shape.
	return &agentv1.ReportStatusResponse{
		Events:    []*agentv1.AuditEvent{},
		FinalSeq:  0,
	}, nil
}