package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/config"
)

// writeTestCert 生成一张自签叶子证书并返回 cert 路径(register 组装请求时要读它)。
func writeTestCert(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成测试私钥: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("生成测试 serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "agent-cli-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发测试证书: %v", err)
	}
	path := filepath.Join(t.TempDir(), "leaf.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("写测试证书: %v", err)
	}
	return path
}

// writeTestConfig 写一份最小可校验 YAML,并配上可读的证书,返回配置路径。
// 配置里的地址固定为 127.0.0.1:19443,供地址覆盖类断言引用。
func writeTestConfig(t *testing.T, dir string) string {
	t.Helper()
	certPath := writeTestCert(t)
	body := fmt.Sprintf(`agent_id: agent-test
control_plane_addr: 127.0.0.1:19443
cert_path: %s
data_dir: %s
audit_dir: %s
`, certPath, filepath.Join(dir, "data"), filepath.Join(dir, "audit"))
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写测试配置: %v", err)
	}
	return path
}

// fakeAgentClient 是 agentv1.AgentServiceClient 的桩:每个 RPC 一个函数字段,
// 未赋值的 RPC 返回 codes.Unimplemented。
type fakeAgentClient struct {
	registerFn  func(context.Context, *agentv1.RegisterRequest, ...grpc.CallOption) (*agentv1.RegisterResponse, error)
	applyFn     func(context.Context, *agentv1.ApplyIntentRequest, ...grpc.CallOption) (*agentv1.ApplyIntentResponse, error)
	reportFn    func(context.Context, *agentv1.ReportStatusRequest, ...grpc.CallOption) (*agentv1.ReportStatusResponse, error)
	heartbeatFn func(context.Context, *agentv1.HeartbeatRequest, ...grpc.CallOption) (*agentv1.HeartbeatResponse, error)
	snapshotFn  func(context.Context, *agentv1.SnapshotRequestRequest, ...grpc.CallOption) (*agentv1.SnapshotResponse, error)
	rollbackFn  func(context.Context, *agentv1.RollbackIntentRequest, ...grpc.CallOption) (*agentv1.RollbackIntentResponse, error)
}

func (f *fakeAgentClient) Register(ctx context.Context, in *agentv1.RegisterRequest, opts ...grpc.CallOption) (*agentv1.RegisterResponse, error) {
	if f.registerFn == nil {
		return nil, status.Error(codes.Unimplemented, "stub 未设置")
	}
	return f.registerFn(ctx, in, opts...)
}

func (f *fakeAgentClient) Heartbeat(ctx context.Context, in *agentv1.HeartbeatRequest, opts ...grpc.CallOption) (*agentv1.HeartbeatResponse, error) {
	if f.heartbeatFn == nil {
		return nil, status.Error(codes.Unimplemented, "stub 未设置")
	}
	return f.heartbeatFn(ctx, in, opts...)
}

func (f *fakeAgentClient) ApplyIntent(ctx context.Context, in *agentv1.ApplyIntentRequest, opts ...grpc.CallOption) (*agentv1.ApplyIntentResponse, error) {
	if f.applyFn == nil {
		return nil, status.Error(codes.Unimplemented, "stub 未设置")
	}
	return f.applyFn(ctx, in, opts...)
}

func (f *fakeAgentClient) SnapshotRequest(ctx context.Context, in *agentv1.SnapshotRequestRequest, opts ...grpc.CallOption) (*agentv1.SnapshotResponse, error) {
	if f.snapshotFn == nil {
		return nil, status.Error(codes.Unimplemented, "stub 未设置")
	}
	return f.snapshotFn(ctx, in, opts...)
}

func (f *fakeAgentClient) ReportStatus(ctx context.Context, in *agentv1.ReportStatusRequest, opts ...grpc.CallOption) (*agentv1.ReportStatusResponse, error) {
	if f.reportFn == nil {
		return nil, status.Error(codes.Unimplemented, "stub 未设置")
	}
	return f.reportFn(ctx, in, opts...)
}

func (f *fakeAgentClient) RollbackIntent(ctx context.Context, in *agentv1.RollbackIntentRequest, opts ...grpc.CallOption) (*agentv1.RollbackIntentResponse, error) {
	if f.rollbackFn == nil {
		return nil, status.Error(codes.Unimplemented, "stub 未设置")
	}
	return f.rollbackFn(ctx, in, opts...)
}

// dialRecorder 记录 dialClient 被调用时收到的配置与调用次数。
type dialRecorder struct {
	cfg    *config.Config
	called bool
}

func installStubDial(t *testing.T, client agentv1.AgentServiceClient) *dialRecorder {
	t.Helper()
	return installStubDialFunc(t, func(*config.Config) (agentv1.AgentServiceClient, func() error, error) {
		return client, func() error { return nil }, nil
	})
}

// installStubDialFunc 用于需要断言"压根没建连"的用例(用法错误必须在
// 任何网络行为之前被拒)。
func installStubDialFunc(t *testing.T, fn func(*config.Config) (agentv1.AgentServiceClient, func() error, error)) *dialRecorder {
	t.Helper()
	rec := &dialRecorder{}
	orig := dialClient
	dialClient = func(cfg *config.Config) (agentv1.AgentServiceClient, func() error, error) {
		rec.called = true
		rec.cfg = cfg
		return fn(cfg)
	}
	t.Cleanup(func() { dialClient = orig })
	return rec
}
