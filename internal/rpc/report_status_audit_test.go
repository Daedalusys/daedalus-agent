package rpc_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// 控制面只通过 ReportStatus 看链:拒绝事件必须在 wire 上可见,且 payload
// 能还原 identity/outcome/args,否则"拒绝"对控制面等于没发生。
func TestReportStatus_SurfacesRefusedApplyIntent(t *testing.T) {
	rig := startTestServerWithSnapshot(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := rig.Client.Register(ctx, &agentv1.RegisterRequest{AgentId: "agent-test-1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := rig.Client.ApplyIntent(ctx, &agentv1.ApplyIntentRequest{
		IntentId: "intent-wire-rf3", Kind: "package", Target: "nginx",
	}); err != nil {
		t.Fatalf("ApplyIntent: %v", err)
	}

	resp, err := rig.Client.ReportStatus(ctx, &agentv1.ReportStatusRequest{SinceAuditSeq: 0, MaxEvents: 100})
	if err != nil {
		t.Fatalf("ReportStatus: %v", err)
	}
	if resp.FinalSeq != 1 {
		t.Fatalf("FinalSeq = %d, want 1", resp.FinalSeq)
	}
	if len(resp.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(resp.Events))
	}
	ev := resp.Events[0]
	if ev.EventType != "apply_intent.refused" {
		t.Errorf("EventType = %q, want apply_intent.refused", ev.EventType)
	}
	if ev.PrevHash == "" || ev.CurrentHash == "" {
		t.Errorf("hashes empty (prev=%q cur=%q); chain evidence must be on the wire", ev.PrevHash, ev.CurrentHash)
	}
	var payload struct {
		Identity string            `json:"identity"`
		Outcome  string            `json:"outcome"`
		Args     map[string]string `json:"args"`
	}
	if err := json.Unmarshal([]byte(ev.PayloadJson), &payload); err != nil {
		t.Fatalf("PayloadJson: %v", err)
	}
	if payload.Identity != "intent-wire-rf3" {
		t.Errorf("payload.identity = %q, want intent-wire-rf3", payload.Identity)
	}
	if payload.Outcome != "refused: snapshot_capability_none" {
		t.Errorf("payload.outcome = %q, want refused: snapshot_capability_none", payload.Outcome)
	}
	if payload.Args["target"] != "nginx" || payload.Args["kind"] != "package" {
		t.Errorf("payload.args = %+v, want target=nginx kind=package", payload.Args)
	}

	// 游标推进后不再重复投递同一条。
	next, err := rig.Client.ReportStatus(ctx, &agentv1.ReportStatusRequest{SinceAuditSeq: resp.FinalSeq, MaxEvents: 100})
	if err != nil {
		t.Fatalf("ReportStatus(since=%d): %v", resp.FinalSeq, err)
	}
	if len(next.Events) != 0 || next.FinalSeq != 1 {
		t.Errorf("caught-up report = %d events (final %d), want 0 (final 1)", len(next.Events), next.FinalSeq)
	}
}
