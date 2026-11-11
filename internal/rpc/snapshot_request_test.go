package rpc

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/config"
	"github.com/Daedalusys/daedalus-agent/internal/snapshot"
)

// SnapshotRequest 的请求体只有 reason,没有目标路径 —— 没有"拍谁"的信息
// 就拍不出有意义的快照。按 kind 划分的快照点在 ApplyIntent 里建;在这里
// 猜一个目录拍下去,只会产出一份与 intent 无关的副本。
func TestSnapshotRequest_RefusedNoTarget(t *testing.T) {
	svc := newAgentService(&config.Config{DataDir: t.TempDir()}, nil, snapshot.NewDetector(), nil, noopBackend{}, nil, nil)

	_, err := svc.SnapshotRequest(context.Background(), &agentv1.SnapshotRequestRequest{Reason: "manual"})
	if err == nil {
		t.Fatal("应拒绝")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", st.Code())
	}
	if !strings.Contains(st.Message(), "no target path") {
		t.Errorf("文案 = %q, want 含 no target path", st.Message())
	}
	if !strings.Contains(st.Message(), "ApplyIntent") {
		t.Errorf("文案 = %q, want 指向 ApplyIntent(快照点在那边建)", st.Message())
	}
}
