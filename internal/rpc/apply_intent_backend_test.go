package rpc

import (
	"context"
	"sync/atomic"
	"testing"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/audit"
	"github.com/Daedalusys/daedalus-agent/internal/config"
	"github.com/Daedalusys/daedalus-agent/internal/rollback"
	"github.com/Daedalusys/daedalus-agent/internal/snapshot"
	"github.com/Daedalusys/daedalus-agent/internal/tx"
)

// stubTx 记录收到的 intent 并直接成功:本文件只验准入与后端选择的接线,
// 不验事务状态机(那是 internal/tx 的事)。
type stubTx struct{ seen **agentv1.ApplyIntentRequest }

func (s stubTx) Run(_ context.Context, req *agentv1.ApplyIntentRequest) (*agentv1.ApplyIntentResponse, error) {
	if s.seen != nil {
		*s.seen = req
	}
	return &agentv1.ApplyIntentResponse{
		Result: agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS,
		TxId:   "tx-" + req.IntentId,
	}, nil
}

func stubTxFactory(seen **agentv1.ApplyIntentRequest) TxFactory {
	return func(rollback.Backend, string, *atomic.Uint64, *audit.Local) tx.Tx {
		return stubTx{seen: seen}
	}
}

// 无回滚点的 kind **不进后端选择**:它根本不建快照,所以「选不出后端」不该
// 成为拒绝理由(D9:低危不因缺回滚点被拒)。
//
// 2026-11-12 真机回归:这里曾经断言「service 走到 selectedBackend 必须以
// backend_unavailable 终局」,把 D9 与「PointNone 也选后端」的矛盾当成契约钉住
// 了,vma(AlmaLinux 9)上的低危 package 因此被拒。现在钉的是正面契约:照常
// 进事务、链上不出现 backend_unavailable、pending 归零。未知能力(99)也不该
// 影响 PointNone —— 没有回滚点就不看能力。
func TestApplyIntent_PointNoneSkipsBackendSelectionAndRuns(t *testing.T) {
	dir := t.TempDir()
	auditLog, err := audit.New(dir)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	det := snapshot.NewDetector()
	det.SetCapability(agentv1.SnapshotCapability(99))
	var got *agentv1.ApplyIntentRequest
	svc := newAgentService(&config.Config{DataDir: dir}, nil, det, nil, nil, stubTxFactory(&got), auditLog)

	resp, err := svc.ApplyIntent(context.Background(), &agentv1.ApplyIntentRequest{
		IntentId: "intent-point-none", Kind: "service", Target: "nginx.service",
		RiskClass: agentv1.RiskClass_RISK_CLASS_LOW,
	})
	if err != nil {
		t.Fatalf("ApplyIntent: %v", err)
	}
	if resp.Result == agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE {
		t.Fatalf("低危 service 应放行, got REFUSED: %q", resp.GetErrorMessage())
	}
	if got == nil {
		t.Fatal("intent 没有进入事务")
	}
	if n := svc.pendingIntentCount(); n != 0 {
		t.Errorf("pending = %d, want 0(事务已收尾)", n)
	}

	events, _, err := auditLog.Since(0)
	if err != nil {
		t.Fatalf("audit.Since: %v", err)
	}
	for _, ev := range events {
		if ev.Tool == "apply_intent.backend_unavailable" {
			t.Errorf("PointNone 不该产生 backend_unavailable 事件, got %+v", ev)
		}
	}
}

// 无回滚点 + 未配置审计链:照常进事务,不得 panic(recordOutcome 在 nil 链上跳过)。
func TestApplyIntent_PointNoneNilAuditDoesNotPanic(t *testing.T) {
	det := snapshot.NewDetector()
	det.SetCapability(agentv1.SnapshotCapability(99))
	var got *agentv1.ApplyIntentRequest
	svc := newAgentService(&config.Config{DataDir: t.TempDir()}, nil, det, nil, nil, stubTxFactory(&got), nil)

	resp, err := svc.ApplyIntent(context.Background(), &agentv1.ApplyIntentRequest{
		IntentId: "intent-point-none-nil-audit", Kind: "service", Target: "nginx.service",
		RiskClass: agentv1.RiskClass_RISK_CLASS_LOW,
	})
	if err != nil {
		t.Fatalf("ApplyIntent: %v", err)
	}
	if resp.Result == agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE {
		t.Errorf("低危 service 应放行, got REFUSED: %q", resp.GetErrorMessage())
	}
	if got == nil {
		t.Fatal("intent 没有进入事务")
	}
}

// selectedBackend 把回滚点形态转发给 SelectBackend:PointVolume + 未知
// 能力必须拒绝。本期 PointVolume 无 producer,这里钉的是 RPC 层转发契约。
func TestSelectedBackend_RefusesUnknownCapabilityForVolume(t *testing.T) {
	det := snapshot.NewDetector()
	det.SetCapability(agentv1.SnapshotCapability(99))
	svc := newAgentService(&config.Config{DataDir: t.TempDir()}, nil, det, nil, nil, nil, nil)

	if _, err := svc.selectedBackend(rollback.PointVolume); err == nil {
		t.Fatal("selectedBackend(PointVolume, unknown): nil err; want refusal")
	}
}
