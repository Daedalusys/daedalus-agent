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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/cert"
	"github.com/Daedalusys/daedalus-agent/internal/config"
	"github.com/Daedalusys/daedalus-agent/internal/rpc"
	"github.com/Daedalusys/daedalus-agent/internal/snapshot"
)

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

	detector := snapshot.NewDetector()
	detector.SetCapability(cap)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	srv, err := rpc.NewServer(listener.Addr().String(), tlsCfg, rpc.ServiceDeps{
		Cfg:      cfg,
		CertInfo: info,
		Snapshot: detector,
		Backend:  mockBackend,
	})
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

// TestApplyIntent_OnNoneCapability_Refused — RF3.
//
// This is the most important test in the file. The agent MUST
// refuse ApplyIntent when its snapshot_capability is NONE: any
// commit without a rollback path violates the spec's "no silent
// partial apply" rule (spec §6 + plan §RF3). The response must:
//
//   - Result == APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE.
//   - ErrorMessage is non-empty.
//   - ErrorMessage contains "no snapshot backend" or
//     "ApplyIntent refused" so the control plane's failure bucket
//     can surface it.
//   - ErrorMessage does NOT contain the bare word "best-effort"
//     without the rest of the explanation (the human-reviewer
//     misreading concern raised in the spec).
func TestApplyIntent_OnNoneCapability_Refused(t *testing.T) {
	rig := startTestServerWithSnapshot(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// First Register so the agent has a node_id.
	if _, err := rig.Client.Register(ctx, &agentv1.RegisterRequest{AgentId: "agent-test-1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	resp, err := rig.Client.ApplyIntent(ctx, &agentv1.ApplyIntentRequest{
		IntentId:  "intent-001",
		Kind:      "package.set",
		Target:    "nginx",
		SpecJson:  `{"name":"nginx","version":"1.18"}`,
		RiskClass: agentv1.RiskClass_RISK_CLASS_LOW,
	})
	if err != nil {
		t.Fatalf("ApplyIntent: %v", err)
	}
	if resp.Result != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE {
		t.Errorf("Result = %v, want REFUSED_SNAPSHOT_CAPABILITY_NONE", resp.Result)
	}
	if resp.ErrorMessage == "" {
		t.Errorf("ErrorMessage empty; want explanation of the refusal")
	}
	if !strings.Contains(resp.ErrorMessage, "snapshot backend") &&
		!strings.Contains(resp.ErrorMessage, "ApplyIntent refused") {
		t.Errorf("ErrorMessage %q must mention 'snapshot backend' or 'ApplyIntent refused' (RF3 clarity)", resp.ErrorMessage)
	}
	// Bare "best-effort" alone is forbidden — the full
	// explanation must accompany it.
	if strings.TrimSpace(resp.ErrorMessage) == "best-effort" {
		t.Errorf("ErrorMessage is the bare string 'best-effort'; that misreads to reviewers as 'we will handle it' (RF3)")
	}
}

// TestApplyIntent_OnBtrfsCapability_StubSucceeds verifies that the
// non-NONE happy path returns APPLY_INTENT_RESULT_SUCCESS with a
// non-empty tx_id. The real pre-snapshot → apply → post-snapshot
// → rollback state machine is T6; T5 stubs success and writes a
// clear comment marking the boundary.
func TestApplyIntent_OnBtrfsCapability_StubSucceeds(t *testing.T) {
	rig := startTestServerWithSnapshot(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := rig.Client.Register(ctx, &agentv1.RegisterRequest{AgentId: "agent-test-1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	resp, err := rig.Client.ApplyIntent(ctx, &agentv1.ApplyIntentRequest{
		IntentId:  "intent-001",
		Kind:      "package.set",
		Target:    "nginx",
		SpecJson:  `{"name":"nginx"}`,
		RiskClass: agentv1.RiskClass_RISK_CLASS_LOW,
	})
	if err != nil {
		t.Fatalf("ApplyIntent: %v", err)
	}
	if resp.Result != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS {
		t.Errorf("Result = %v, want SUCCESS", resp.Result)
	}
	if resp.TxId == "" {
		t.Errorf("TxId empty; want non-empty (stub)")
	}
}

// TestSnapshotRequest_OnBtrfsCapability_StubReturnsID verifies the
// SnapshotRequest happy path returns a non-empty snapshot_id and a
// backend_used that matches the agent's capability. Real btrfs /
// rsync snapshot primitives are T6.
func TestSnapshotRequest_OnBtrfsCapability_StubReturnsID(t *testing.T) {
	rig := startTestServerWithSnapshot(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := rig.Client.Register(ctx, &agentv1.RegisterRequest{AgentId: "agent-test-1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	resp, err := rig.Client.SnapshotRequest(ctx, &agentv1.SnapshotRequestRequest{Reason: "test"})
	if err != nil {
		t.Fatalf("SnapshotRequest: %v", err)
	}
	if resp.SnapshotId == "" {
		t.Errorf("SnapshotId empty; want non-empty (stub)")
	}
	if resp.BackendUsed != agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME {
		t.Errorf("BackendUsed = %v, want BTRFS_SUBVOLUME", resp.BackendUsed)
	}
	if resp.CreatedAtUnix == 0 {
		t.Errorf("CreatedAtUnix = 0; want positive Unix seconds")
	}
}

// TestSnapshotRequest_OnNoneCapability_FailedPrecondition verifies
// that an agent with no snapshot backend rejects SnapshotRequest
// with a clear gRPC FailedPrecondition. We surface this via a
// status code (rather than a stub SnapshotResponse) because the
// control plane treats "no backend" as an environment failure, not
// a successful empty operation.
func TestSnapshotRequest_OnNoneCapability_FailedPrecondition(t *testing.T) {
	rig := startTestServerWithSnapshot(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := rig.Client.Register(ctx, &agentv1.RegisterRequest{AgentId: "agent-test-1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, err := rig.Client.SnapshotRequest(ctx, &agentv1.SnapshotRequestRequest{Reason: "test"})
	if err == nil {
		t.Fatal("SnapshotRequest on NONE capability: nil err; want FailedPrecondition")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", st.Code())
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
