// SnapshotRequest handler:恒拒绝。
//
// 这个 RPC 的请求体只有 reason(见 proto),没有目标路径,也就无从决定
// "拍谁"。按 kind 划分的快照点在 ApplyIntent 里建 —— 那次调用同时知道
// kind 与 target,事务层也已经按 kind 判过有没有回滚点。在这里猜一个
// 目录拍下去,只会产出一份与 intent 无关、却被记成"计划前快照"的副本。
//
// 用 gRPC status 而不是 in-body 拒绝:没有快照就没有需要控制面记录的
// 载荷,transport 层的失败语义已经足够。
package rpc

import (
	"context"
	"log"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// SnapshotRequest implements agentv1.AgentServiceServer.SnapshotRequest.
func (s *agentService) SnapshotRequest(ctx context.Context, req *agentv1.SnapshotRequestRequest) (*agentv1.SnapshotResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "SnapshotRequest: nil request")
	}

	log.Printf("agent: SnapshotRequest refused (request carries no target path): reason=%q", req.Reason)
	return nil, status.Error(codes.FailedPrecondition,
		"SnapshotRequest: request carries no target path; per-kind snapshot points are created inside ApplyIntent")
}
