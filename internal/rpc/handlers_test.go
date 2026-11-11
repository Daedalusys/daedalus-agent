// Tests for the five RPC handlers (Register / Heartbeat /
// ApplyIntent / SnapshotRequest / ReportStatus) wired into the
// AgentService in T5.
//
// We exercise the handlers via real grpc.NewClient + gRPC invocations
// rather than calling the handler functions directly. Reasons:
//
//  1. The unit under test is the wire-protocol behaviour: response
//     shape, error codes surfaced to the gRPC client, and the
//     interplay with mTLS (RF4).
//  2. Calling handlers directly would force us to hand-build the
//     *agentService struct each time; gRPC gives us one point of
//     wiring (NewServer) and covers the registration path too.
//
// RF3 (ApplyIntent refuses on snapshot_capability == NONE) and
// RF4 (cert expiry surfaces a clear error to the gRPC client) are
// covered as dedicated tests below.
package rpc_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/audit"
	"github.com/Daedalusys/daedalus-agent/internal/cert"
	"github.com/Daedalusys/daedalus-agent/internal/config"
	"github.com/Daedalusys/daedalus-agent/internal/rollback"
	"github.com/Daedalusys/daedalus-agent/internal/rpc"
	"github.com/Daedalusys/daedalus-agent/internal/snapshot"
	"github.com/Daedalusys/daedalus-agent/internal/tx"
)

// cannedTx 是只回一条固定响应的事务 fake。准入相关的用例不关心事务内部
// 怎么跑,只关心 handler 有没有把 intent 送进事务 —— 用真的 exec.Apply
// 会去跑测试机上的包管理器。
type cannedTx struct {
	onRun func(context.Context, *agentv1.ApplyIntentRequest) (*agentv1.ApplyIntentResponse, error)
}

func (c cannedTx) Run(ctx context.Context, req *agentv1.ApplyIntentRequest) (*agentv1.ApplyIntentResponse, error) {
	return c.onRun(ctx, req)
}

// successTxFactory 返回一个恒 SUCCESS 的事务,并把收到的 intent 记进 got。
func successTxFactory(got **agentv1.ApplyIntentRequest) rpc.TxFactory {
	return func(rollback.Backend, string, *atomic.Uint64, *audit.Local) tx.Tx {
		return cannedTx{onRun: func(_ context.Context, req *agentv1.ApplyIntentRequest) (*agentv1.ApplyIntentResponse, error) {
			if got != nil {
				*got = req
			}
			return &agentv1.ApplyIntentResponse{
				Result: agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS,
				TxId:   "tx-" + req.IntentId,
			}, nil
		}}
	}
}

// testRig bundles a live *rpc.Server with a *grpc.ClientConn
// already wired up to it. Tests defer rig.Cleanup() and use
// rig.Client to invoke RPCs. Backend is the rigMockBackend
// pre-wired into the server (tests can mutate its createErr /
// restoreErr to exercise failure paths).
type testRig struct {
	Server  *rpc.Server
	Conn    *grpc.ClientConn
	Client  agentv1.AgentServiceClient
	Backend *rigMockBackend
	Cleanup func()
}

// rigMockBackend is a hermetic rollback.Backend used by the
// T6 handler-level tests. It satisfies rollback.Backend via
// in-memory state and returns the configured createErr /
// restoreErr. Defaults to nil errors (happy path).
type rigMockBackend struct {
	mu                        sync.Mutex
	createErr                 error
	restoreErr                error
	createCalls, restoreCalls int
	lastSrc, lastSnap         string
	lastSnapPath, lastTarget  string
}

func (m *rigMockBackend) CreateSnapshot(src, snap string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalls++
	m.lastSrc = src
	m.lastSnap = snap
	return m.createErr
}

func (m *rigMockBackend) Restore(snap, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restoreCalls++
	m.lastSnapPath = snap
	m.lastTarget = target
	return m.restoreErr
}

// startTestServerWithSnapshot stands up a fully-wired agentServer
// with a preset snapshot capability, then dials it as a real gRPC
// client. The PKI is fresh per test (60h TTL, well outside the
// loader's 24h rotation window).
//
// The rig pre-wires a hermetic rigMockBackend as the rollback
// backend (injected via ServiceDeps.Backend) so the handler
// exercises the wired-up state machine without requiring real
// btrfs / rsync on the test runner. The mock defaults to
// success — tests that need failure paths reach into
// rig.Backend to substitute.
func startTestServerWithSnapshot(t *testing.T, cap agentv1.SnapshotCapability) *testRig {
	return startTestServerConfigured(t, cap, nil)
}

// startTestServerConfigured is startTestServerWithSnapshot plus a hook
// that mutates the agent's Config before the server is built. Tests that
// need a non-default Config field (e.g. PolicyPath) use this seam; every
// other test keeps calling the wrapper and stays untouched.
func startTestServerConfigured(t *testing.T, cap agentv1.SnapshotCapability, mutate func(*config.Config)) *testRig {
	return startTestServerFull(t, cap, mutate, nil)
}

// startTestServerWithTx 在标准 rig 上换掉 ApplyIntent 的事务构造,用来
// 只验证准入 / 分派,不落真实执行器。回滚点用例既不希望跑包管理器,也不
// 希望依赖 file 执行器是否已注册。
func startTestServerWithTx(t *testing.T, cap agentv1.SnapshotCapability, factory rpc.TxFactory) *testRig {
	return startTestServerFull(t, cap, nil, func(d *rpc.ServiceDeps) { d.TxFactory = factory })
}

// startTestServerFull is the shared rig builder: mutateCfg runs before the
// config is consumed, mutateDeps before the server is constructed.
func startTestServerFull(t *testing.T, cap agentv1.SnapshotCapability, mutateCfg func(*config.Config), mutateDeps func(*rpc.ServiceDeps)) *testRig {
	t.Helper()

	mockBackend := &rigMockBackend{}

	caCertPEM, caKeyPEM, serverCertPEM, serverKeyPEM := buildPKIAt(t,
		time.Now().Add(-1*time.Hour),
		time.Now().Add(60*time.Hour),
	)
	clientCertPEM, clientKeyPEM := signLeafWithCA(t, "client-test", []string{},
		60*time.Hour, caCertPEM, caKeyPEM)

	dir := t.TempDir()
	loader, err := cert.New(cert.Config{
		CAPath:     mustWrite(t, dir, "ca.crt", caCertPEM),
		CAKeyPath:  mustWrite(t, dir, "ca.key", caKeyPEM),
		CertPath:   mustWrite(t, dir, "server.crt", serverCertPEM),
		KeyPath:    mustWrite(t, dir, "server.key", serverKeyPEM),
		DNSNames:   []string{"localhost"},
		CommonName: "daedalus-agent-test",
	})
	if err != nil {
		t.Fatalf("cert.New: %v", err)
	}
	tlsCfg, err := loader.Load()
	if err != nil {
		t.Fatalf("loader.Load: %v", err)
	}

	info, err := cert.LoaderInfo(loader)
	if err != nil {
		t.Fatalf("cert.LoaderInfo: %v", err)
	}

	cfg := &config.Config{
		AgentID:          "agent-test-1",
		ControlPlaneAddr: "https://control.example.test:7443",
		DataDir:          dir,
		AuditDir:         dir,
		CertPath:         mustWrite(t, dir, "noop-c.crt", []byte("")),
		KeyPath:          mustWrite(t, dir, "noop-c.key", []byte("")),
		CAPath:           mustWrite(t, dir, "noop-ca.crt", []byte("")),
		CAKeyPath:        mustWrite(t, dir, "noop-ca.key", []byte("")),
	}
	if mutateCfg != nil {
		mutateCfg(cfg)
	}

	detector := snapshot.NewDetector()
	detector.SetCapability(cap)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	deps := rpc.ServiceDeps{
		Cfg:      cfg,
		CertInfo: info,
		Snapshot: detector,
		Backend:  mockBackend,
	}
	if mutateDeps != nil {
		mutateDeps(&deps)
	}
	srv, err := rpc.NewServer(listener.Addr().String(), tlsCfg, deps)
	if err != nil {
		t.Fatalf("rpc.NewServer: %v", err)
	}
	go func() { _ = srv.Serve(listener) }()

	clientCert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		t.Fatalf("parse client cert: %v", err)
	}
	clientCfg := &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      tlsCfg.ClientCAs,
		ServerName:   "localhost",
		MaxVersion:   tls.VersionTLS12,
	}
	// grpc.NewClient is lazy: the actual TLS handshake happens on
	// the first RPC. We invoke Register below to drive the
	// handshake before returning the rig.
	conn, err := grpc.NewClient(listener.Addr().String(),
		grpc.WithTransportCredentials(credentials.NewTLS(clientCfg)),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	// Drive the handshake eagerly so a startup TLS failure
	// surfaces at setup time, not on the first test RPC.
	client := agentv1.NewAgentServiceClient(conn)
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, _ = client.Register(probeCtx, &agentv1.RegisterRequest{AgentId: "agent-probe"})
	probeCancel()
	return &testRig{
		Server:  srv,
		Conn:    conn,
		Client:  client,
		Backend: mockBackend,
		Cleanup: func() {
			srv.Stop()
			_ = conn.Close()
		},
	}
}

// TestRegister_AssignsNodeIDAndCapability covers the happy path:
// Register receives a valid request, assigns a node_id, and reports
// the snapshot_capability from the detector.
//
// We force the detector to RSYNC_TARGET so the test is hermetic
// (host-agnostic — Detect() on the test runner could otherwise be
// either BTRFS or RSYNC depending on the host's btrfs support).
func TestRegister_AssignsNodeIDAndCapability(t *testing.T) {
	rig := startTestServerWithSnapshot(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := rig.Client.Register(ctx, &agentv1.RegisterRequest{
		AgentId:      "agent-test-1",
		AgentVersion: "0.1.0-test",
		OsInfo: &agentv1.OsInfo{
			Distro:   "Ubuntu",
			Version:  "22.04",
			Kernel:   "5.15.0",
			Arch:     "amd64",
			Hostname: "test-host",
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.NodeId == "" {
		t.Errorf("RegisterResponse.NodeId empty; want non-empty")
	}
	if got := resp.SnapshotCapability; got != agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET {
		t.Errorf("SnapshotCapability = %v, want RSYNC_TARGET", got)
	}
	if resp.CertValidUntilUnix == 0 {
		t.Errorf("CertValidUntilUnix = 0; want non-zero Unix seconds")
	}
	if resp.HeartbeatIntervalSeconds == 0 {
		t.Errorf("HeartbeatIntervalSeconds = 0; want > 0")
	}
	// node_id should be stable across Register calls.
	resp2, err := rig.Client.Register(ctx, &agentv1.RegisterRequest{AgentId: "agent-test-1"})
	if err != nil {
		t.Fatalf("Register (2nd): %v", err)
	}
	if resp.NodeId != resp2.NodeId {
		t.Errorf("NodeId changed between registers: %q vs %q", resp.NodeId, resp2.NodeId)
	}
}

// TestRegister_EmptyAgentID_ReturnsInvalidArgument covers the input
// validation contract: empty agent_id is a programmer error /
// wrong wire — return codes.InvalidArgument.
func TestRegister_EmptyAgentID_ReturnsInvalidArgument(t *testing.T) {
	rig := startTestServerWithSnapshot(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := rig.Client.Register(ctx, &agentv1.RegisterRequest{AgentId: ""})
	if err == nil {
		t.Fatal("Register with empty agent_id: nil err; want InvalidArgument")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("err is not a gRPC status: %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

// TestRegister_PolicyHashIsRealDigest pins the "configured + readable"
// arm: when cfg.PolicyPath points at a readable file, the response's
// policy_hash must be that file's sha256(hex) — not a placeholder.
func TestRegister_PolicyHashIsRealDigest(t *testing.T) {
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "policy.toml")
	content := []byte("schema = \"daedalus.policy/v0\"\nmode = \"enforce\"\n")
	if err := os.WriteFile(policyPath, content, 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])

	rig := startTestServerConfigured(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET,
		func(c *config.Config) { c.PolicyPath = policyPath })
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := rig.Client.Register(ctx, &agentv1.RegisterRequest{AgentId: "agent-test-1"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.PolicyHash != want {
		t.Errorf("PolicyHash = %q, want sha256 %q", resp.PolicyHash, want)
	}
}

// TestRegister_FailedPreconditionWhenPolicyMissing pins the
// "configured + unreadable" arm: a configured path whose file is gone
// is a deployment accident, not a dev shape, so Register must surface
// FailedPrecondition rather than silently falling back to "".
func TestRegister_FailedPreconditionWhenPolicyMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.toml")

	rig := startTestServerConfigured(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET,
		func(c *config.Config) { c.PolicyPath = missing })
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := rig.Client.Register(ctx, &agentv1.RegisterRequest{AgentId: "agent-test-1"})
	if err == nil {
		t.Fatal("Register with missing policy file: nil err; want FailedPrecondition")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", st.Code())
	}
}

// TestHeartbeat_RequiresRegisteredNodeID covers the post-Register
// contract: Heartbeat is rejected unless the caller has Registered.
// In a fresh server, an unknown node_id is rejected with
// FailedPrecondition (the agent has no record of that node yet).
func TestHeartbeat_RequiresRegisteredNodeID(t *testing.T) {
	rig := startTestServerWithSnapshot(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := rig.Client.Heartbeat(ctx, &agentv1.HeartbeatRequest{
		NodeId:        "node-never-registered",
		StatusSummary: "alive",
	})
	if err == nil {
		t.Fatal("Heartbeat before Register: nil err; want FailedPrecondition")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", st.Code())
	}
}

// TestHeartbeat_AfterRegister_Acks covers the happy path:
// Register then Heartbeat with the assigned node_id returns
// ack=true and a positive next_heartbeat_seconds.
func TestHeartbeat_AfterRegister_Acks(t *testing.T) {
	rig := startTestServerWithSnapshot(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	regResp, err := rig.Client.Register(ctx, &agentv1.RegisterRequest{AgentId: "agent-test-1"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	hbResp, err := rig.Client.Heartbeat(ctx, &agentv1.HeartbeatRequest{
		NodeId:        regResp.NodeId,
		StatusSummary: "alive",
	})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !hbResp.Ack {
		t.Errorf("HeartbeatResponse.Ack = false; want true")
	}
	if hbResp.NextHeartbeatSeconds == 0 {
		t.Errorf("NextHeartbeatSeconds = 0; want > 0")
	}
}

// TestApplyIntent_HighRiskWithoutRollbackPoint_Refused — 准入闸门.
//
// 高危 intent 打到没有回滚点的 kind(package / service)上 → 拒绝。拒绝
// 理由必须说"没有回滚点",而不是"宿主没有快照能力":后者回答的是另一个
// 问题,照旧用它会把"这台机器没装 btrfs"错当成"这次变更没法还原"。
//
// 这里刻意把宿主能力设成 NONE:准入不再看它。
func TestApplyIntent_HighRiskWithoutRollbackPoint_Refused(t *testing.T) {
	rig := startTestServerWithSnapshot(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := rig.Client.Register(ctx, &agentv1.RegisterRequest{AgentId: "agent-test-1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	resp, err := rig.Client.ApplyIntent(ctx, &agentv1.ApplyIntentRequest{
		IntentId:  "i1",
		Kind:      "package",
		Target:    "kernel-core",
		RiskClass: agentv1.RiskClass_RISK_CLASS_HIGH,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResult() != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE {
		t.Errorf("结果 = %v, want REFUSED", resp.GetResult())
	}
	if !strings.Contains(resp.GetErrorMessage(), "without rollback point") {
		t.Errorf("文案 = %q, want 含 without rollback point", resp.GetErrorMessage())
	}
}

// 低危 + 无回滚点 → 放行。低危变更没有还原手段也未必不能做:拒绝与否是
// 风险问题,不是能力问题。
func TestApplyIntent_LowRiskWithoutRollbackPoint_Allowed(t *testing.T) {
	var got *agentv1.ApplyIntentRequest
	rig := startTestServerWithTx(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE, successTxFactory(&got))
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := rig.Client.Register(ctx, &agentv1.RegisterRequest{AgentId: "agent-test-1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	resp, err := rig.Client.ApplyIntent(ctx, &agentv1.ApplyIntentRequest{
		IntentId:  "i2",
		Kind:      "package",
		Target:    "nginx",
		RiskClass: agentv1.RiskClass_RISK_CLASS_LOW,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResult() == agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE {
		t.Fatalf("低危 package 应放行, got REFUSED: %q", resp.GetErrorMessage())
	}
	if got == nil {
		t.Fatal("intent 没有进入事务")
	}
}

// 高危 + 有回滚点(file)→ 放行。file 是唯一有回滚点的 kind。
func TestApplyIntent_HighRiskWithRollbackPoint_Allowed(t *testing.T) {
	var got *agentv1.ApplyIntentRequest
	rig := startTestServerWithTx(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE, successTxFactory(&got))
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := rig.Client.Register(ctx, &agentv1.RegisterRequest{AgentId: "agent-test-1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	resp, err := rig.Client.ApplyIntent(ctx, &agentv1.ApplyIntentRequest{
		IntentId:  "i3",
		Kind:      "file",
		Target:    "/etc/app.conf",
		SpecJson:  `{"content":"hello"}`,
		RiskClass: agentv1.RiskClass_RISK_CLASS_HIGH,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResult() == agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE {
		t.Fatalf("高危 file 应放行, got REFUSED: %q", resp.GetErrorMessage())
	}
	if got == nil {
		t.Fatal("intent 没有进入事务")
	}
}

// 表外 kind → 拒绝(fail-closed)。不认识的 kind 既没有回滚点,也没有执行器,
// 放行等于让控制面以为 agent 会做点什么。
func TestApplyIntent_UnknownKind_Refused(t *testing.T) {
	rig := startTestServerWithSnapshot(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := rig.Client.Register(ctx, &agentv1.RegisterRequest{AgentId: "agent-test-1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	resp, err := rig.Client.ApplyIntent(ctx, &agentv1.ApplyIntentRequest{
		IntentId:  "i4",
		Kind:      "totally-unknown",
		Target:    "whatever",
		RiskClass: agentv1.RiskClass_RISK_CLASS_LOW,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResult() != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE {
		t.Errorf("结果 = %v, want REFUSED", resp.GetResult())
	}
	if !strings.Contains(resp.GetErrorMessage(), "unknown intent kind") {
		t.Errorf("文案 = %q, want 含 unknown intent kind", resp.GetErrorMessage())
	}
}

// SnapshotRequest 不再拍 intent-target 目录(该概念已删),而请求本身
// 只有 reason、没有目标路径 —— 无从决定拍谁,所以恒拒绝。结果不随宿主
// 能力变化:装了 btrfs 也一样 FailedPrecondition。
func TestSnapshotRequest_RefusedRegardlessOfCapability(t *testing.T) {
	for _, cap := range []agentv1.SnapshotCapability{
		agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE,
		agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME,
	} {
		t.Run(cap.String(), func(t *testing.T) {
			rig := startTestServerWithSnapshot(t, cap)
			defer rig.Cleanup()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := rig.Client.Register(ctx, &agentv1.RegisterRequest{AgentId: "agent-test-1"}); err != nil {
				t.Fatalf("Register: %v", err)
			}
			_, err := rig.Client.SnapshotRequest(ctx, &agentv1.SnapshotRequestRequest{Reason: "test"})
			if err == nil {
				t.Fatal("SnapshotRequest: nil err; want FailedPrecondition")
			}
			st, _ := status.FromError(err)
			if st.Code() != codes.FailedPrecondition {
				t.Errorf("code = %v, want FailedPrecondition", st.Code())
			}
		})
	}
}

// TestReportStatus_ReturnsEmptyEvents covers the T5 contract:
// ReportStatus returns an empty events list and final_seq=0.
// Real audit hash-chain replay is T7 (which will pull events from
// the agent's local JSONL audit log).
func TestReportStatus_ReturnsEmptyEvents(t *testing.T) {
	rig := startTestServerWithSnapshot(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := rig.Client.Register(ctx, &agentv1.RegisterRequest{AgentId: "agent-test-1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	resp, err := rig.Client.ReportStatus(ctx, &agentv1.ReportStatusRequest{
		SinceAuditSeq: 0,
		MaxEvents:     100,
	})
	if err != nil {
		t.Fatalf("ReportStatus: %v", err)
	}
	if len(resp.Events) != 0 {
		t.Errorf("events len = %d, want 0 (T5 stub; T7 fills from JSONL audit log)", len(resp.Events))
	}
	if resp.FinalSeq != 0 {
		t.Errorf("FinalSeq = %d, want 0", resp.FinalSeq)
	}
}

// TestRF4_HandlerLevel_ExpiredServerCertReturnsClearError — RF4
// handler-level guard.
//
// T3's wire-level test (TestServer_ExpiredCertReturnsClearError)
// uses raw tls.Dial to bypass gRPC's transport retry. T5's
// handler-level test exercises a real gRPC client against a
// server whose cert is already expired; the client must surface
// the TLS error rather than silently hanging or returning a
// generic transport error.
//
// We use grpc.WithContextDialer to perform the TLS handshake
// manually (gRPC's default transport retry hides the underlying
// x509 error behind context.DeadlineExceeded). The dialer
// returns the stdlib TLS error directly; gRPC's wrapper
// preserves it on Register().
//
// We expect a non-nil error whose message — walking the entire
// error chain — contains "expired".
func TestRF4_HandlerLevel_ExpiredServerCertReturnsClearError(t *testing.T) {
	caCertPEM, _, serverCertPEM, serverKeyPEM := buildPKIAt(t,
		time.Now().Add(-4*time.Second),
		time.Now().Add(-2*time.Second),
	)
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatalf("parse server cert: %v", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCertPEM) {
		t.Fatal("AppendCertsFromPEM failed for CA")
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   tls.VersionTLS12,
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	// Use a fully-built agentService even though no RPC will
	// actually run (the TLS handshake fails first).
	srv, err := rpc.NewServer(listener.Addr().String(), tlsCfg, rpc.ServiceDeps{
		Cfg:      &config.Config{AgentID: "rf4-test"},
		CertInfo: nil,
		Snapshot: snapshot.NewDetector(),
	})
	if err != nil {
		t.Fatalf("rpc.NewServer: %v", err)
	}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { srv.Stop() })

	clientCfg := &tls.Config{
		RootCAs:    caPool,
		ServerName: "localhost",
		MaxVersion: tls.VersionTLS12,
	}

	// Manual TLS dialer so the stdlib TLS error surfaces
	// directly. gRPC's default transport retries and wraps
	// errors in context.DeadlineExceeded, hiding the
	// x509 expiry message.
	tlsDialer := func(ctx context.Context, addr string) (net.Conn, error) {
		var d net.Dialer
		return tls.DialWithDialer(&d, "tcp", addr, clientCfg)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// grpc.NewClient (lazy) + WithContextDialer — the dialer's
	// error surfaces on the first RPC.
	conn, newClientErr := grpc.NewClient(listener.Addr().String(),
		grpc.WithContextDialer(tlsDialer),
		grpc.WithTransportCredentials(credentials.NewTLS(clientCfg)),
	)
	if newClientErr != nil {
		t.Fatalf("grpc.NewClient: %v", newClientErr)
	}
	defer func() { _ = conn.Close() }()

	client := agentv1.NewAgentServiceClient(conn)
	_, regErr := client.Register(ctx, &agentv1.RegisterRequest{AgentId: "rf4-tst"})

	if regErr == nil {
		t.Fatal("Register against expired-cert server: nil err; want handshake failure")
	}
	t.Logf("RF4 handler-level Register error: %v", regErr)

	if !errorChainContains(regErr, "expired") {
		t.Errorf("Register error chain does not contain 'expired': %v", regErr)
	}
}

// errorChainContains walks an error and its Unwrap() chain,
// returning true if any error message contains substr. We use
// this rather than strings.Contains(err.Error()) because gRPC
// wraps transport errors in multiple layers (status.Error,
// connection error, etc.) and the underlying TLS error may
// surface only deep in the chain.
func errorChainContains(err error, substr string) bool {
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		if strings.Contains(cur.Error(), substr) {
			return true
		}
	}
	return false
}
