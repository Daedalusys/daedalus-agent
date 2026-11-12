package rpc

import (
	"context"
	"strings"
	"testing"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/audit"
	"github.com/Daedalusys/daedalus-agent/internal/config"
	"github.com/Daedalusys/daedalus-agent/internal/rollback"
	"github.com/Daedalusys/daedalus-agent/internal/snapshot"
)

// 无回滚点的 kind 走到后端选择时必须被拒并落链:service 按低危放行到
// selectedBackend(PointNone),在这里以"没有回滚点"终局,而不是进事务。
func TestApplyIntent_BackendSelectionFailureWritesAuditEvent(t *testing.T) {
	dir := t.TempDir()
	auditLog, err := audit.New(dir)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	det := snapshot.NewDetector()
	det.SetCapability(agentv1.SnapshotCapability(99))
	svc := newAgentService(&config.Config{DataDir: dir}, nil, det, nil, nil, nil, auditLog)

	resp, err := svc.ApplyIntent(context.Background(), &agentv1.ApplyIntentRequest{
		IntentId: "intent-bad-backend", Kind: "service", Target: "nginx.service",
	})
	if err != nil {
		t.Fatalf("ApplyIntent: %v", err)
	}
	if resp.Result != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE {
		t.Fatalf("Result = %v, want REFUSED_SNAPSHOT_CAPABILITY_NONE", resp.Result)
	}

	events, final, err := auditLog.Since(0)
	if err != nil {
		t.Fatalf("audit.Since: %v", err)
	}
	if final != 1 || len(events) != 1 {
		t.Fatalf("chain events = %d (final %d), want 1 (final 1)", len(events), final)
	}
	ev := events[0]
	if ev.Tool != "apply_intent.backend_unavailable" {
		t.Errorf("tool = %q, want apply_intent.backend_unavailable", ev.Tool)
	}
	if ev.Identity != "intent-bad-backend" {
		t.Errorf("identity = %q, want intent-bad-backend", ev.Identity)
	}
	if !strings.Contains(ev.Outcome, "backend_unavailable:") {
		t.Errorf("outcome = %q, must be prefixed backend_unavailable:", ev.Outcome)
	}
	if !strings.Contains(ev.Outcome, "no rollback point") {
		t.Errorf("outcome = %q, must carry the SelectBackend error", ev.Outcome)
	}
	if ev.Args["target"] != "nginx.service" || ev.Args["kind"] != "service" {
		t.Errorf("args = %+v, want target=nginx.service kind=service", ev.Args)
	}
	if ev.Args["error"] == "" {
		t.Error("args[error] empty; the raw backend-selection error must be preserved")
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

func TestApplyIntent_BackendSelectionFailureNilAuditDoesNotPanic(t *testing.T) {
	det := snapshot.NewDetector()
	det.SetCapability(agentv1.SnapshotCapability(99))
	svc := newAgentService(&config.Config{DataDir: t.TempDir()}, nil, det, nil, nil, nil, nil)

	resp, err := svc.ApplyIntent(context.Background(), &agentv1.ApplyIntentRequest{
		IntentId: "intent-bad-backend-nil", Kind: "service", Target: "nginx.service",
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

func TestApplyIntent_BackendSelectionFailureIsNotPending(t *testing.T) {
	dir := t.TempDir()
	auditLog, err := audit.New(dir)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	det := snapshot.NewDetector()
	det.SetCapability(agentv1.SnapshotCapability(99))
	svc := newAgentService(&config.Config{DataDir: dir}, nil, det, nil, nil, nil, auditLog)

	if _, err := svc.ApplyIntent(context.Background(), &agentv1.ApplyIntentRequest{
		IntentId: "intent-bad-backend-pending", Kind: "service", Target: "nginx.service",
	}); err != nil {
		t.Fatalf("ApplyIntent: %v", err)
	}
	if got := svc.pendingIntentCount(); got != 0 {
		t.Errorf("pending = %d, want 0 (backend failure ends the RPC before beginIntent)", got)
	}
}
