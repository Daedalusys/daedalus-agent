package rpc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/config"
	"github.com/Daedalusys/daedalus-agent/internal/rollback"
	"github.com/Daedalusys/daedalus-agent/internal/snapshot"
)

// 目录级 file intent 在 RPC 层要端到端接通:handler 解析 spec、注入路径、
// 事务用集合后端快照、执行器逐条写。这里用真实的 tx + exec,只把后端换成
// 指向临时目录的 tree 后端,确保接线本身(而不是替身)被验证。
func TestApplyIntent_TreeUsesPathSetBackend(t *testing.T) {
	dataDir := t.TempDir()
	target := t.TempDir()
	targetFile := filepath.Join(target, "a.conf")
	if err := os.WriteFile(targetFile, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	svc := newAgentService(&config.Config{DataDir: dataDir}, nil, snapshot.NewDetector(), nil,
		rollback.NewTreeBackend(dataDir), nil, nil)

	resp, err := svc.ApplyIntent(context.Background(), &agentv1.ApplyIntentRequest{
		IntentId:  "intent-tree",
		Kind:      "file",
		Target:    target,
		SpecJson:  `{"entries":[{"path":"a.conf","op":"write","content":"v2"}]}`,
		RiskClass: agentv1.RiskClass_RISK_CLASS_LOW,
	})
	if err != nil {
		t.Fatalf("ApplyIntent: %v", err)
	}
	if resp.Result != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS {
		t.Fatalf("Result = %v, want SUCCESS(%s)", resp.Result, resp.GetErrorMessage())
	}
	got, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2" {
		t.Errorf("内容 = %q, want v2", got)
	}

	manifestPath := filepath.Join(dataDir, "snapshots", "intent-tree", "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("目录快照 manifest 缺失(接线没走到集合后端): %v", err)
	}
	if !strings.Contains(string(raw), targetFile) {
		t.Errorf("manifest 没记录 %q: %s", targetFile, raw)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "snapshots", "intent-tree.snap")); !os.IsNotExist(err) {
		t.Errorf("目录 intent 不该产生单文件 .snap, stat err = %v", err)
	}
}

// 越界 entry 在进入事务之前就被拒:不建快照、不写任何文件。
func TestApplyIntent_TreeRejectsEscape(t *testing.T) {
	dataDir := t.TempDir()
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}

	svc := newAgentService(&config.Config{DataDir: dataDir}, nil, snapshot.NewDetector(), nil,
		rollback.NewTreeBackend(dataDir), nil, nil)

	resp, err := svc.ApplyIntent(context.Background(), &agentv1.ApplyIntentRequest{
		IntentId:  "intent-escape",
		Kind:      "file",
		Target:    target,
		SpecJson:  `{"entries":[{"path":"../escape.conf","op":"write","content":"x"}]}`,
		RiskClass: agentv1.RiskClass_RISK_CLASS_LOW,
	})
	if err != nil {
		t.Fatalf("ApplyIntent: %v", err)
	}
	if resp.Result == agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS {
		t.Fatal("越界 entry 不该成功")
	}
	if !strings.Contains(resp.GetErrorMessage(), "escape") {
		t.Errorf("error_message = %q, want 说明越界", resp.GetErrorMessage())
	}
	if _, err := os.Stat(filepath.Join(parent, "escape.conf")); !os.IsNotExist(err) {
		t.Errorf("越界文件被写出来了, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "snapshots", "intent-escape")); !os.IsNotExist(err) {
		t.Errorf("被拒的 intent 不该留下快照, stat err = %v", err)
	}
	if n := svc.pendingIntentCount(); n != 0 {
		t.Errorf("pending = %d, want 0(拒绝发生在进事务之前)", n)
	}
}

// 旧 spec 的单文件路径逐字不变:copy 后端、<intent_id>.snap、内容覆盖。
func TestApplyIntent_LegacyFileUnchanged(t *testing.T) {
	dataDir := t.TempDir()
	target := filepath.Join(t.TempDir(), "app.conf")
	if err := os.WriteFile(target, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	svc := newAgentService(&config.Config{DataDir: dataDir}, nil, snapshot.NewDetector(), nil,
		&rollback.CopyBackend{}, nil, nil)

	resp, err := svc.ApplyIntent(context.Background(), &agentv1.ApplyIntentRequest{
		IntentId:  "intent-legacy",
		Kind:      "file",
		Target:    target,
		SpecJson:  `{"content":"v2"}`,
		RiskClass: agentv1.RiskClass_RISK_CLASS_LOW,
	})
	if err != nil {
		t.Fatalf("ApplyIntent: %v", err)
	}
	if resp.Result != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS {
		t.Fatalf("Result = %v, want SUCCESS(%s)", resp.Result, resp.GetErrorMessage())
	}
	got, _ := os.ReadFile(target)
	if string(got) != "v2" {
		t.Errorf("内容 = %q, want v2", got)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "snapshots", "intent-legacy.snap")); err != nil {
		t.Errorf("单文件快照缺失: %v", err)
	}
}
