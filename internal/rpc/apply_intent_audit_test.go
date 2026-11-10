package rpc

import (
	"context"
	"sync/atomic"
	"testing"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/audit"
	"github.com/Daedalusys/daedalus-agent/internal/config"
	"github.com/Daedalusys/daedalus-agent/internal/snapshot"
)

// 拒绝结局必须落链:tool / outcome / identity / args 四元组是控制面的
// 分桶依据,少一个都读不出"是谁、想改什么、为什么被拒"。
func TestApplyIntent_RefusalWritesAuditEvent(t *testing.T) {
	dir := t.TempDir()
	auditLog, err := audit.New(dir)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	det := snapshot.NewDetector()
	det.SetCapability(agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE)
	svc := newAgentService(&config.Config{DataDir: dir}, nil, det, nil, nil, nil, auditLog)

	resp, err := svc.ApplyIntent(context.Background(), &agentv1.ApplyIntentRequest{
		IntentId: "intent-rf3", Kind: "package", Target: "nginx",
	})
	if err != nil {
		t.Fatalf("ApplyIntent: %v", err)
	}
	if resp.Result != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE {
		t.Fatalf("Result = %v, want REFUSED_SNAPSHOT_CAPABILITY_NONE", resp.Result)
	}
	if resp.AuditSeq != 1 {
		t.Errorf("AuditSeq = %d, want 1 (per-process counter unchanged)", resp.AuditSeq)
	}

	events, final, err := auditLog.Since(0)
	if err != nil {
		t.Fatalf("audit.Since: %v", err)
	}
	if final != 1 {
		t.Fatalf("chain final seq = %d, want 1", final)
	}
	if len(events) != 1 {
		t.Fatalf("chain events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Tool != "apply_intent.refused" {
		t.Errorf("tool = %q, want apply_intent.refused", ev.Tool)
	}
	if ev.Identity != "intent-rf3" {
		t.Errorf("identity = %q, want intent-rf3", ev.Identity)
	}
	if ev.Outcome != "refused: snapshot_capability_none" {
		t.Errorf("outcome = %q, want refused: snapshot_capability_none", ev.Outcome)
	}
	if ev.Args["target"] != "nginx" {
		t.Errorf("args[target] = %q, want nginx", ev.Args["target"])
	}
	if ev.Args["kind"] != "package" {
		t.Errorf("args[kind] = %q, want package", ev.Args["kind"])
	}
}

// 测试/未配置链时 audit 为 nil:拒绝路径不得 panic,响应形状不变。
func TestApplyIntent_RefusalNilAuditDoesNotPanic(t *testing.T) {
	det := snapshot.NewDetector()
	det.SetCapability(agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE)
	svc := newAgentService(&config.Config{DataDir: t.TempDir()}, nil, det, nil, nil, nil, nil)

	resp, err := svc.ApplyIntent(context.Background(), &agentv1.ApplyIntentRequest{
		IntentId: "intent-nil-audit", Kind: "package", Target: "nginx",
	})
	if err != nil {
		t.Fatalf("ApplyIntent: %v", err)
	}
	if resp.Result != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE {
		t.Errorf("Result = %v, want REFUSED_SNAPSHOT_CAPABILITY_NONE", resp.Result)
	}
	if resp.AuditSeq != 1 {
		t.Errorf("AuditSeq = %d, want 1", resp.AuditSeq)
	}
}

// 拒绝不应计入 pending:beginIntent 只在事务真正开始时调用。
func TestApplyIntent_RefusalIsNotPending(t *testing.T) {
	dir := t.TempDir()
	auditLog, err := audit.New(dir)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	det := snapshot.NewDetector()
	det.SetCapability(agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE)
	svc := newAgentService(&config.Config{DataDir: dir}, nil, det, nil, nil, nil, auditLog)

	if _, err := svc.ApplyIntent(context.Background(), &agentv1.ApplyIntentRequest{
		IntentId: "intent-pending", Kind: "package", Target: "nginx",
	}); err != nil {
		t.Fatalf("ApplyIntent: %v", err)
	}
	if got := svc.pendingIntentCount(); got != 0 {
		t.Errorf("pending after refusal = %d, want 0 (beginIntent must not run)", got)
	}
}

// audit_seq(进程内计数)与链 seq(文件行号)是两条序列:重启后链继续、
// 计数器归零。这里先播一条链记录模拟"上一个进程写过链",再走拒绝。
func TestApplyIntent_AuditSeqIsNotChainSeq(t *testing.T) {
	dir := t.TempDir()
	auditLog, err := audit.New(dir)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	if _, err := auditLog.Log(audit.Event{
		Identity: "previous-process", Tool: "apply_intent.success", Outcome: "ok",
	}); err != nil {
		t.Fatalf("seed chain: %v", err)
	}

	det := snapshot.NewDetector()
	det.SetCapability(agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE)
	svc := newAgentService(&config.Config{DataDir: dir}, nil, det, new(atomic.Uint64), nil, nil, auditLog)

	resp, err := svc.ApplyIntent(context.Background(), &agentv1.ApplyIntentRequest{
		IntentId: "intent-after-restart", Kind: "package", Target: "nginx",
	})
	if err != nil {
		t.Fatalf("ApplyIntent: %v", err)
	}
	if resp.AuditSeq != 1 {
		t.Errorf("AuditSeq = %d, want 1 (fresh per-process counter)", resp.AuditSeq)
	}

	events, final, err := auditLog.Since(1)
	if err != nil {
		t.Fatalf("audit.Since: %v", err)
	}
	if final != 2 {
		t.Errorf("chain final seq = %d, want 2", final)
	}
	if len(events) != 1 || events[0].Seq != 2 {
		t.Fatalf("new chain records = %+v, want exactly seq 2", events)
	}
	if events[0].Tool != "apply_intent.refused" {
		t.Errorf("tool = %q, want apply_intent.refused", events[0].Tool)
	}
}
