package main

import (
	"bytes"
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"google.golang.org/grpc"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/config"
)

func TestCollectRegisterRequest_FillsIdentityFields(t *testing.T) {
	dir := t.TempDir()
	certPath := writeTestCert(t) // 写入 dir 下的 leaf.pem
	cfg := &config.Config{
		AgentID:  "agent-7",
		DataDir:  filepath.Join(dir, "data"),
		CertPath: certPath,
	}
	req, err := collectRegisterRequest(cfg)
	if err != nil {
		t.Fatalf("collectRegisterRequest: %v", err)
	}
	if req.GetAgentId() != "agent-7" {
		t.Fatalf("agent_id = %q", req.GetAgentId())
	}
	if req.GetAgentVersion() != version {
		t.Fatalf("agent_version = %q, want %q(ldflags 注入的二进制版本)", req.GetAgentVersion(), version)
	}
	if len(req.GetCertFingerprint()) != 64 {
		t.Fatalf("cert_fingerprint 长度 = %d, want 64(sha256 hex)", len(req.GetCertFingerprint()))
	}
	if req.GetLastAuditSeq() != 0 {
		t.Fatalf("last_audit_seq = %d, want 0(空 DataDir)", req.GetLastAuditSeq())
	}
	if req.GetOsInfo().GetArch() != runtime.GOARCH {
		t.Fatalf("arch = %q, want %q", req.GetOsInfo().GetArch(), runtime.GOARCH)
	}
}

func TestCollectRegisterRequest_RejectsMissingCert(t *testing.T) {
	cfg := &config.Config{AgentID: "agent-7", DataDir: t.TempDir()}
	if _, err := collectRegisterRequest(cfg); err == nil {
		t.Fatal("CertPath 为空时必须报错(不能假装有 fingerprint)")
	}
}

func TestRegisterCmd_PrintsResponseJSON(t *testing.T) {
	cfgPath := writeTestConfig(t, t.TempDir()) // 见下:写最小可校验 YAML
	var captured *agentv1.RegisterRequest
	client := &fakeAgentClient{registerFn: func(_ context.Context, in *agentv1.RegisterRequest, _ ...grpc.CallOption) (*agentv1.RegisterResponse, error) {
		captured = in
		return &agentv1.RegisterResponse{
			NodeId:             "node-1",
			SnapshotCapability: agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME,
		}, nil
	}}
	installStubDial(t, client) // 记录 dialClient 收到的 *config.Config 到返回的 dialRecorder

	cmd := newRegisterCmd(&cfgPath)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("register 失败: %v", err)
	}
	if captured == nil || captured.GetAgentId() == "" {
		t.Fatal("RegisterRequest 未被下发或 agent_id 为空")
	}
	if !strings.Contains(out.String(), `"node_id": "node-1"`) {
		t.Fatalf("stdout 未输出 snake_case 的 node_id:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME") {
		t.Fatalf("stdout 未输出 snapshot_capability 枚举名:\n%s", out.String())
	}
}

func TestRegisterCmd_PeerWithoutAgentService(t *testing.T) {
	cfgPath := writeTestConfig(t, t.TempDir())
	client := &fakeAgentClient{} // 五个方法全返回 codes.Unimplemented
	installStubDial(t, client)

	cmd := newRegisterCmd(&cfgPath)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("对端没有 AgentService 时必须报错")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:19443") {
		t.Fatalf("错误必须点名对端地址, got %v", err)
	}
	if !strings.Contains(err.Error(), "--addr") {
		t.Fatalf("错误必须提示用 --addr 指向正确端点, got %v", err)
	}
}

func TestRegisterCmd_AddrFlagOverridesConfig(t *testing.T) {
	cfgPath := writeTestConfig(t, t.TempDir()) // 配置里是 127.0.0.1:19443
	rec := installStubDial(t, &fakeAgentClient{registerFn: func(context.Context, *agentv1.RegisterRequest, ...grpc.CallOption) (*agentv1.RegisterResponse, error) {
		return &agentv1.RegisterResponse{NodeId: "node-1"}, nil
	}})

	cmd := newRegisterCmd(&cfgPath)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--addr", "https://127.0.0.1:29443"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("register 失败: %v", err)
	}
	if rec.cfg == nil || rec.cfg.ControlPlaneAddr != "127.0.0.1:29443" {
		t.Fatalf("dial 收到的地址 = %+v, want %q(--addr 覆盖 + https 去 scheme)", rec.cfg, "127.0.0.1:29443")
	}
}
