package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/config"
)

func TestApplyCmd_UnknownRiskClassIsUsageError(t *testing.T) {
	cfgPath := writeTestConfig(t, t.TempDir())
	mocked := false
	installStubDialFunc(t, func(*config.Config) (agentv1.AgentServiceClient, func() error, error) {
		mocked = true
		return &fakeAgentClient{}, func() error { return nil }, nil
	})

	cmd := newApplyCmd(&cfgPath)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--intent-id", "i-1", "--kind", "package", "--risk-class", "medium"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("未知 risk-class 必须是用法错误")
	}
	if !strings.Contains(err.Error(), "risk-class") {
		t.Fatalf("错误必须点名 --risk-class, got %v", err)
	}
	if mocked {
		t.Fatal("用法错误不得发起建连/RPC")
	}
}

func TestApplyCmd_RequiresIntentIDAndKind(t *testing.T) {
	cfgPath := writeTestConfig(t, t.TempDir())
	for _, args := range [][]string{
		{"--kind", "package"},
		{"--intent-id", "i-1"},
	} {
		cmd := newApplyCmd(&cfgPath)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("args=%v 必须报缺参数", args)
		}
	}
}

func TestApplyCmd_BuildsRequest(t *testing.T) {
	cfgPath := writeTestConfig(t, t.TempDir())
	specPath := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(specPath, []byte(`{"name":"nginx","state":"present"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var captured *agentv1.ApplyIntentRequest
	installStubDial(t, &fakeAgentClient{applyFn: func(_ context.Context, in *agentv1.ApplyIntentRequest, _ ...grpc.CallOption) (*agentv1.ApplyIntentResponse, error) {
		captured = in
		return &agentv1.ApplyIntentResponse{Result: agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS, TxId: "tx-9"}, nil
	}})

	cmd := newApplyCmd(&cfgPath)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--intent-id", "i-1", "--kind", "package", "--target", "nginx",
		"--spec-file", specPath, "--risk-class", "low"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	if captured.GetIntentId() != "i-1" || captured.GetKind() != "package" ||
		captured.GetTarget() != "nginx" || captured.GetSpecJson() != `{"name":"nginx","state":"present"}` {
		t.Fatalf("请求字段不符: %+v", captured)
	}
	if captured.GetRiskClass() != agentv1.RiskClass_RISK_CLASS_LOW {
		t.Fatalf("risk_class = %v, want LOW", captured.GetRiskClass())
	}
	if !strings.Contains(out.String(), `"tx_id": "tx-9"`) {
		t.Fatalf("stdout 未输出 tx_id:\n%s", out.String())
	}
}

func TestApplyCmd_RefusalExitsZero(t *testing.T) {
	cfgPath := writeTestConfig(t, t.TempDir())
	installStubDial(t, &fakeAgentClient{applyFn: func(context.Context, *agentv1.ApplyIntentRequest, ...grpc.CallOption) (*agentv1.ApplyIntentResponse, error) {
		return &agentv1.ApplyIntentResponse{
			Result:       agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE,
			ErrorMessage: "本机无 snapshot backend",
		}, nil
	}})

	cmd := newApplyCmd(&cfgPath)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--intent-id", "i-1", "--kind", "package", "--target", "nginx"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("拒绝是业务结果,必须退出 0; got %v", err)
	}
	if !strings.Contains(out.String(), "REFUSED_SNAPSHOT_CAPABILITY_NONE") {
		t.Fatalf("stdout 必须打印拒绝原因:\n%s", out.String())
	}
}

func TestApplyCmd_TransportErrorIsNonZero(t *testing.T) {
	cfgPath := writeTestConfig(t, t.TempDir())
	installStubDial(t, &fakeAgentClient{applyFn: func(context.Context, *agentv1.ApplyIntentRequest, ...grpc.CallOption) (*agentv1.ApplyIntentResponse, error) {
		return nil, status.Error(codes.Unavailable, "connection refused")
	}})

	cmd := newApplyCmd(&cfgPath)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--intent-id", "i-1", "--kind", "package"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("传输错误必须非零退出")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:19443") {
		t.Fatalf("错误必须点名地址, got %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("传输错误时不得打印响应:\n%s", out.String())
	}
}
