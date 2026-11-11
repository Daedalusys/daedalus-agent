package rpc

import (
	"context"
	"fmt"
	"sync"
	"testing"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	sdkAudit "github.com/Daedalusys/daedalus-sdk/audit"

	"github.com/Daedalusys/daedalus-agent/internal/audit"
	"github.com/Daedalusys/daedalus-agent/internal/config"
	"github.com/Daedalusys/daedalus-agent/internal/snapshot"
)

// 并发拒绝不得写坏链:磁盘上必须仍是 SDK 可全链校验的哈希链,行号连续。
// 注意断言 Record.Seq(磁盘行号),不要断言 Log 返回值 —— audit.Local 的
// 内存 seq 明确是 best-effort 缓存(internal/audit/local.go 包注释)。
func TestApplyIntent_ConcurrentRefusalsKeepChainIntact(t *testing.T) {
	dir := t.TempDir()
	auditLog, err := audit.New(dir)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	det := snapshot.NewDetector()
	det.SetCapability(agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE)
	svc := newAgentService(&config.Config{DataDir: dir}, nil, det, nil, nil, nil, auditLog)

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := svc.ApplyIntent(context.Background(), &agentv1.ApplyIntentRequest{
				IntentId:  fmt.Sprintf("intent-concurrent-%d", i),
				Kind:      "package",
				Target:    "nginx",
				RiskClass: agentv1.RiskClass_RISK_CLASS_HIGH,
			}); err != nil {
				t.Errorf("ApplyIntent[%d]: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	got, err := sdkAudit.Verify(auditLog.ChainPath())
	if err != nil {
		t.Fatalf("hash chain is not intact: %v", err)
	}
	if got != n {
		t.Errorf("verified entries = %d, want %d", got, n)
	}
	records, final, err := auditLog.Since(0)
	if err != nil {
		t.Fatalf("audit.Since: %v", err)
	}
	if final != uint64(n) || len(records) != n {
		t.Fatalf("records = %d (final %d), want %d", len(records), final, n)
	}
	for i, rec := range records {
		if rec.Seq != uint64(i+1) {
			t.Errorf("record[%d].Seq = %d, want %d (line-number seq must be contiguous)", i, rec.Seq, i+1)
		}
		if rec.Tool != "apply_intent.refused" {
			t.Errorf("record[%d].Tool = %q, want apply_intent.refused", i, rec.Tool)
		}
	}
}

func TestReportStatus_IncludesBackendUnavailableOutcome(t *testing.T) {
	dir := t.TempDir()
	auditLog, err := audit.New(dir)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	det := snapshot.NewDetector()
	det.SetCapability(agentv1.SnapshotCapability(99))
	svc := newAgentService(&config.Config{DataDir: dir}, nil, det, nil, nil, nil, auditLog)
	if _, err := svc.ApplyIntent(context.Background(), &agentv1.ApplyIntentRequest{
		IntentId: "intent-report-backend", Kind: "service", Target: "nginx.service",
	}); err != nil {
		t.Fatalf("ApplyIntent: %v", err)
	}
	resp, err := svc.ReportStatus(context.Background(), &agentv1.ReportStatusRequest{SinceAuditSeq: 0})
	if err != nil {
		t.Fatalf("ReportStatus: %v", err)
	}
	if len(resp.Events) != 1 || resp.Events[0].EventType != "apply_intent.backend_unavailable" {
		t.Fatalf("events = %+v, want one apply_intent.backend_unavailable", resp.Events)
	}
	if resp.FinalSeq != 1 {
		t.Errorf("FinalSeq = %d, want 1", resp.FinalSeq)
	}
}
