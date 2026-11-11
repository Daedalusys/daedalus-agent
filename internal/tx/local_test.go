// Tests for the local ApplyIntent transaction state machine.
//
// The state machine wired in internal/tx/local.go has four
// observable paths:
//
//  1. Success: pre-snapshot → apply (nil) → return SUCCESS.
//  2. Pre-snapshot failure: backend.CreateSnapshot errors; tx
//     returns TRANSIENT_FAILURE without calling apply.
//  3. Apply failure + rollback success: apply errors, rollback
//     succeeds; tx returns TRANSIENT_FAILURE.
//  4. Apply failure + rollback failure: apply errors, rollback
//     also errors; tx returns PERSISTENT_FAILURE and logs the
//     dual failure loudly.
//
// The tests cover each path with a recording mock backend and
// a recording apply spy, so an assertion failure pinpoints the
// exact (call-count, error) tuple the tx surfaced.
//
// T7 (this task) also adds: the new `auditLog *audit.Local`
// parameter on tx.New. All existing paths are exercised with
// `auditLog=nil` so the test does not have to set up a chain
// file; a dedicated test (TestLocal_AuditEmitsOnSuccess /
// Failure / RollbackFailed) drives a real audit.Local via
// t.TempDir() and asserts the audit chain received the
// expected event.
package tx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	sdkAudit "github.com/Daedalusys/daedalus-sdk/audit"
	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/audit"
	"github.com/Daedalusys/daedalus-agent/internal/exec"
)

// txMockBackend is a minimal Backend for tx-level tests. Each
// method records invocation count and can be made to error to
// drive the failure paths. We keep this in the test file (not
// internal/rollback's test helpers) because the contract being
// exercised here is the tx-Backend interface, not any specific
// backend's behaviour.
type txMockBackend struct {
	mu sync.Mutex

	createCalls              int
	restoreCalls             int
	createErr                error
	restoreErr               error
	lastSrc, lastSnap        string
	lastSnapPath, lastTarget string
}

func (m *txMockBackend) CreateSnapshot(src, snap string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalls++
	m.lastSrc = src
	m.lastSnap = snap
	return m.createErr
}

func (m *txMockBackend) Restore(snap, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restoreCalls++
	m.lastSnapPath = snap
	m.lastTarget = target
	return m.restoreErr
}

// txSpyApply records how the apply step was invoked. `err`
// controls whether the apply succeeds or fails.
type txSpyApply struct {
	mu         sync.Mutex
	err        error
	calls      int
	lastIntent *agentv1.ApplyIntentRequest
}

func (s *txSpyApply) Fn(ctx context.Context, intent *agentv1.ApplyIntentRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastIntent = intent
	return s.err
}

// sampleIntent is the canonical happy-path intent used by the
// success / apply-error tests. The choice of Target="nginx"
// and IntentId="intent-001" is arbitrary but stable so log
// assertions can match on string literals.
//
// package 没有回滚点:这些用例走的是"不快照、不还原"的分支。
func sampleIntent() *agentv1.ApplyIntentRequest {
	return &agentv1.ApplyIntentRequest{
		IntentId:  "intent-001",
		Kind:      "package.set",
		Target:    "nginx",
		SpecJson:  `{"name":"nginx","version":"1.18"}`,
		RiskClass: agentv1.RiskClass_RISK_CLASS_LOW,
	}
}

// fileIntent 是"有回滚点"的 intent:目标是一个真实存在的普通文件,
// 这样快照源就是被改路径本体,而不是 dataDir 下的暂存目录。
func fileIntent(t *testing.T, id string) *agentv1.ApplyIntentRequest {
	t.Helper()
	target := filepath.Join(t.TempDir(), "app.conf")
	if err := os.WriteFile(target, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &agentv1.ApplyIntentRequest{
		IntentId:  id,
		Kind:      "file",
		Target:    target,
		SpecJson:  `{"content":"v2"}`,
		RiskClass: agentv1.RiskClass_RISK_CLASS_LOW,
	}
}

// TestLocal_Run_Success covers the happy path: pre-snapshot is
// invoked, apply succeeds, tx returns SUCCESS with the expected
// tx_id (deterministic from intent_id) and a non-zero audit_seq.
// Rollback MUST NOT be called — verify the spy counter.
func TestLocal_Run_Success(t *testing.T) {
	backend := &txMockBackend{}
	apply := &txSpyApply{}
	audit := new(atomic.Uint64)
	tx := New(backend, apply.Fn, t.TempDir(), audit, nil)

	resp, err := tx.Run(context.Background(), fileIntent(t, "intent-001"))
	if err != nil {
		t.Fatalf("Run: unexpected err: %v", err)
	}
	if resp.Result != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS {
		t.Errorf("Result = %v, want SUCCESS", resp.Result)
	}
	if resp.TxId != "tx-intent-001" {
		t.Errorf("TxId = %q, want %q (derived from intent_id)", resp.TxId, "tx-intent-001")
	}
	if resp.AuditSeq == 0 {
		t.Errorf("AuditSeq = 0; want > 0 (tx should bump it)")
	}
	if backend.createCalls != 1 {
		t.Errorf("CreateSnapshot calls = %d, want 1", backend.createCalls)
	}
	if backend.restoreCalls != 0 {
		t.Errorf("Restore calls = %d, want 0 (no rollback on success path)", backend.restoreCalls)
	}
	if apply.calls != 1 {
		t.Errorf("apply calls = %d, want 1", apply.calls)
	}
}

// TestLocal_Run_PreSnapshotError_AppliesNotCalled covers the
// path where backend.CreateSnapshot errors. The tx returns
// TRANSIENT_FAILURE (the snapshot failure may be transient —
// retryable) without ever invoking the apply step. This guards
// a critical RF3 invariant: a tx that cannot even take a
// snapshot MUST NOT proceed to apply (a silent apply with no
// rollback path would lose data on host failure).
func TestLocal_Run_PreSnapshotError_AppliesNotCalled(t *testing.T) {
	backend := &txMockBackend{createErr: errors.New("simulated no snapshot backend")}
	apply := &txSpyApply{}
	audit := new(atomic.Uint64)
	tx := New(backend, apply.Fn, t.TempDir(), audit, nil)

	resp, err := tx.Run(context.Background(), fileIntent(t, "intent-001"))
	if err != nil {
		t.Fatalf("Run: unexpected err: %v", err)
	}
	if resp.Result != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_TRANSIENT_FAILURE {
		t.Errorf("Result = %v, want TRANSIENT_FAILURE", resp.Result)
	}
	if !strings.Contains(resp.ErrorMessage, "pre-snapshot failed") {
		t.Errorf("ErrorMessage must explain pre-snapshot failure; got %q", resp.ErrorMessage)
	}
	if !strings.Contains(resp.ErrorMessage, "simulated no snapshot backend") {
		t.Errorf("ErrorMessage must carry the underlying error for triage; got %q", resp.ErrorMessage)
	}
	// CRITICAL: apply must NOT have been called. A silent
	// apply after a snapshot failure would violate RF3.
	if apply.calls != 0 {
		t.Errorf("apply was called %d times AFTER snapshot failure; expected 0 (RF3 violation)", apply.calls)
	}
	if backend.restoreCalls != 0 {
		t.Errorf("Restore was called %d times; expected 0 (no rollback when no snapshot existed)", backend.restoreCalls)
	}
}

// TestLocal_Run_ApplyError_RollbackSucceeds covers the path
// where apply errors but the rollback machinery unwinds the
// state. Tx returns TRANSIENT_FAILURE — the failure was
// contained, a retry is safe.
func TestLocal_Run_ApplyError_RollbackSucceeds(t *testing.T) {
	backend := &txMockBackend{}
	apply := &txSpyApply{err: errors.New("apply failed: package not signed")}
	audit := new(atomic.Uint64)
	tx := New(backend, apply.Fn, t.TempDir(), audit, nil)

	resp, err := tx.Run(context.Background(), fileIntent(t, "intent-001"))
	if err != nil {
		t.Fatalf("Run: unexpected err: %v", err)
	}
	if resp.Result != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_TRANSIENT_FAILURE {
		t.Errorf("Result = %v, want TRANSIENT_FAILURE (apply failed but rollback succeeded)", resp.Result)
	}
	if !strings.Contains(resp.ErrorMessage, "rolled back") {
		t.Errorf("ErrorMessage must mention rollback status; got %q", resp.ErrorMessage)
	}
	if !strings.Contains(resp.ErrorMessage, "apply failed: package not signed") {
		t.Errorf("ErrorMessage must carry apply error; got %q", resp.ErrorMessage)
	}
	if backend.createCalls != 1 {
		t.Errorf("CreateSnapshot calls = %d, want 1", backend.createCalls)
	}
	if backend.restoreCalls != 1 {
		t.Errorf("Restore calls = %d, want 1 (rollback triggered by apply failure)", backend.restoreCalls)
	}
}

// TestLocal_Run_ApplyError_RollbackFails_PersistentFailure covers
// the worst case: apply errors AND rollback errors. The host
// may be in an undefined state. Tx returns PERSISTENT_FAILURE
// and the error message names BOTH failures for triage.
func TestLocal_Run_ApplyError_RollbackFails_PersistentFailure(t *testing.T) {
	backend := &txMockBackend{
		restoreErr: errors.New("rollback ENOSPC"),
	}
	apply := &txSpyApply{err: errors.New("apply failed: checksum mismatch")}
	audit := new(atomic.Uint64)
	tx := New(backend, apply.Fn, t.TempDir(), audit, nil)

	resp, err := tx.Run(context.Background(), fileIntent(t, "intent-001"))
	if err != nil {
		t.Fatalf("Run: unexpected err: %v", err)
	}
	if resp.Result != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_PERSISTENT_FAILURE {
		t.Errorf("Result = %v, want PERSISTENT_FAILURE (apply + rollback both failed)", resp.Result)
	}
	if !strings.Contains(resp.ErrorMessage, "apply failed") {
		t.Errorf("ErrorMessage must reference apply error; got %q", resp.ErrorMessage)
	}
	if !strings.Contains(resp.ErrorMessage, "rollback") {
		t.Errorf("ErrorMessage must reference rollback failure; got %q", resp.ErrorMessage)
	}
	if !strings.Contains(resp.ErrorMessage, "undefined state") {
		t.Errorf("ErrorMessage must warn that the host may be in an undefined state; got %q", resp.ErrorMessage)
	}
}

// TestLocal_Run_NilIntent_Errors covers the defence-in-depth
// nil-intent check inside tx.Run. The handler already validates
// this (returning codes.InvalidArgument), but a future caller
// bypassing the handler should still see a clean error.
func TestLocal_Run_NilIntent_Errors(t *testing.T) {
	backend := &txMockBackend{}
	apply := &txSpyApply{}
	tx := New(backend, apply.Fn, t.TempDir(), nil, nil)

	_, err := tx.Run(context.Background(), nil)
	if err == nil {
		t.Fatal("Run(nil): nil err; want error")
	}
	if !strings.Contains(err.Error(), "nil intent") {
		t.Errorf("err must be self-describing; got %q", err.Error())
	}
	if apply.calls != 0 || backend.createCalls != 0 {
		t.Errorf("nil intent must short-circuit before any backend / apply call")
	}
}

// TestLocal_Run_AuditSeqIncrementing verifies the audit_seq
// semantics: each tx invocation bumps the counter by 1
// (success), 2 on the dual-failure path (one for the apply
// failure, one for the rollback failure).
func TestLocal_Run_AuditSeqIncrementing(t *testing.T) {
	backend := &txMockBackend{}
	apply := &txSpyApply{}
	audit := new(atomic.Uint64)
	tx := New(backend, apply.Fn, t.TempDir(), audit, nil)

	r1, _ := tx.Run(context.Background(), sampleIntent())
	if r1.AuditSeq != 1 {
		t.Errorf("first audit_seq = %d, want 1", r1.AuditSeq)
	}
	r2, _ := tx.Run(context.Background(), sampleIntent())
	if r2.AuditSeq != 2 {
		t.Errorf("second audit_seq = %d, want 2", r2.AuditSeq)
	}
}

// TestLocal_NilAuditSeq_UsesPrivateCounter covers the audit
// fallback: passing nil for auditSeq should not panic (the
// constructor creates a private counter in that case).
func TestLocal_NilAuditSeq_UsesPrivateCounter(t *testing.T) {
	backend := &txMockBackend{}
	apply := &txSpyApply{}
	tx := New(backend, apply.Fn, t.TempDir(), nil, nil)

	resp, err := tx.Run(context.Background(), sampleIntent())
	if err != nil {
		t.Fatalf("Run with nil auditSeq: %v", err)
	}
	if resp.AuditSeq == 0 {
		t.Errorf("AuditSeq = 0; constructor should have allocated a private counter")
	}
}

// TestLocal_PathsBuildFromDataDir verifies the snapshot-path convention
// used by Run: <dataDir>/snapshots/<intent_id>.snap。快照源不再由 tx
// 拼——它就是 intent.Target 本体,由 file 用例单独断。
func TestLocal_PathsBuildFromDataDir(t *testing.T) {
	backend := &txMockBackend{}
	apply := &txSpyApply{}
	dir := t.TempDir()
	tx := New(backend, apply.Fn, dir, nil, nil)

	if _, err := tx.Run(context.Background(), fileIntent(t, "intent-001")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantSnap := filepath.Join(dir, "snapshots", "intent-001.snap")
	if backend.lastSnap != wantSnap {
		t.Errorf("snapshot path = %q, want %q", backend.lastSnap, wantSnap)
	}
}

// 无回滚点的 kind 不得拍快照,也不得在失败后假装还原。错误文案必须
// 说明真实原因,且不能出现 "rolled back"。
func TestLocal_Run_NonFileKind_NoSnapshotNoRestore(t *testing.T) {
	backend := &txMockBackend{}
	apply := &txSpyApply{err: errors.New("boom")}
	tx := New(backend, apply.Fn, t.TempDir(), nil, nil)

	resp, err := tx.Run(context.Background(), &agentv1.ApplyIntentRequest{
		IntentId: "i1", Kind: "package", Target: "nginx",
	})
	if err != nil {
		t.Fatal(err)
	}
	if backend.createCalls != 0 || backend.restoreCalls != 0 {
		t.Errorf("无回滚点的 kind 不应拍快照/还原: create=%d restore=%d", backend.createCalls, backend.restoreCalls)
	}
	if !strings.Contains(resp.ErrorMessage, "no rollback point") {
		t.Errorf("错误文案 = %q, want 含 no rollback point", resp.ErrorMessage)
	}
	if strings.Contains(resp.ErrorMessage, "rolled back") {
		t.Errorf("错误文案不应出现 rolled back: %q", resp.ErrorMessage)
	}
}

// file kind 的快照源必须是被改路径本体,而不是 dataDir 下的暂存目录;
// apply 失败后要把这份副本还原回去,并在文案里点名路径。
func TestLocal_Run_FileKind_SnapshotsTargetAndRestores(t *testing.T) {
	backend := &txMockBackend{}
	apply := &txSpyApply{err: errors.New("boom")}
	tx := New(backend, apply.Fn, t.TempDir(), nil, nil)

	intent := fileIntent(t, "i2")
	resp, err := tx.Run(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if backend.createCalls != 1 {
		t.Errorf("file kind 应拍一次快照, got %d", backend.createCalls)
	}
	if backend.lastSrc != intent.Target {
		t.Errorf("快照源 = %q, want %q(被改路径本体)", backend.lastSrc, intent.Target)
	}
	if backend.restoreCalls != 1 {
		t.Errorf("apply 失败应还原一次, got %d", backend.restoreCalls)
	}
	if !strings.Contains(resp.ErrorMessage, "rolled back") {
		t.Errorf("错误文案 = %q, want 含 rolled back", resp.ErrorMessage)
	}
	if !strings.Contains(resp.ErrorMessage, intent.Target) {
		t.Errorf("错误文案 = %q, want 含被还原路径 %q", resp.ErrorMessage, intent.Target)
	}
}

// TestLocal_AuditEmitsOnSuccess covers T7's primary contract:
// a successful ApplyIntent MUST write a hash-chained audit
// event with tool="apply_intent.success", outcome="ok", and
// args {target, kind, tx_id}. The chain lives in a t.TempDir()
// so the test is self-contained.
//
// RED: the test was written before the tx had audit plumbing;
// the first run logged "audit is nil" assertions and the GREEN
// wiring is what we're verifying here.
func TestLocal_AuditEmitsOnSuccess(t *testing.T) {
	dir := t.TempDir()
	auditLog, err := audit.New(dir)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	backend := &txMockBackend{}
	apply := &txSpyApply{}
	tx := New(backend, apply.Fn, dir, new(atomic.Uint64), auditLog)

	resp, err := tx.Run(context.Background(), sampleIntent())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Result != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS {
		t.Fatalf("Result = %v, want SUCCESS", resp.Result)
	}

	// Drain the chain: Since(0) returns all events.
	events, final, err := auditLog.Since(0)
	if err != nil {
		t.Fatalf("audit.Since: %v", err)
	}
	if final != 1 {
		t.Fatalf("audit final seq = %d, want 1 (one success event)", final)
	}
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Tool != "apply_intent.success" {
		t.Errorf("audit tool = %q, want apply_intent.success", ev.Tool)
	}
	if ev.Outcome != "ok" {
		t.Errorf("audit outcome = %q, want ok", ev.Outcome)
	}
	if ev.Identity != "intent-001" {
		t.Errorf("audit identity = %q, want intent-001", ev.Identity)
	}
	if ev.Args["target"] != "nginx" {
		t.Errorf("audit args[target] = %q, want nginx", ev.Args["target"])
	}
	if ev.Args["kind"] != "package.set" {
		t.Errorf("audit args[kind] = %q, want package.set", ev.Args["kind"])
	}
	if ev.Args["tx_id"] != "tx-intent-001" {
		t.Errorf("audit args[tx_id] = %q, want tx-intent-001", ev.Args["tx_id"])
	}
}

// TestLocal_AuditEmitsOnRollback covers the "apply failed,
// rollback succeeded" path. The audit event should have
// tool="apply_intent.failure" with outcome=... and args
// {target, kind}.
func TestLocal_AuditEmitsOnRollback(t *testing.T) {
	dir := t.TempDir()
	auditLog, err := audit.New(dir)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	backend := &txMockBackend{}
	apply := &txSpyApply{err: errors.New("apply failed: package not signed")}
	tx := New(backend, apply.Fn, dir, new(atomic.Uint64), auditLog)

	intent := fileIntent(t, "intent-001")
	resp, err := tx.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Result != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_TRANSIENT_FAILURE {
		t.Fatalf("Result = %v, want TRANSIENT_FAILURE", resp.Result)
	}
	events, _, err := auditLog.Since(0)
	if err != nil {
		t.Fatalf("audit.Since: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Tool != "apply_intent.failure" {
		t.Errorf("audit tool = %q, want apply_intent.failure", ev.Tool)
	}
	if ev.Identity != "intent-001" {
		t.Errorf("audit identity = %q, want intent-001", ev.Identity)
	}
	if ev.Args["target"] != intent.Target {
		t.Errorf("audit args[target] = %q, want %q", ev.Args["target"], intent.Target)
	}
	if !strings.Contains(ev.Outcome, "apply_failed:") {
		t.Errorf("audit outcome must reference apply failure; got %q", ev.Outcome)
	}
	if !strings.Contains(ev.Outcome, "rolled back") {
		t.Errorf("audit outcome must mention rollback recovery; got %q", ev.Outcome)
	}
}

// TestLocal_AuditEmitsOnRollbackFailed covers the worst case:
// apply + rollback both fail. The audit event should have
// tool="apply_intent.rollback_failed" with the dual-failure
// outcome. This is the event the control plane uses to
// surface PERSISTENT_FAILURE to the human.
func TestLocal_AuditEmitsOnRollbackFailed(t *testing.T) {
	dir := t.TempDir()
	auditLog, err := audit.New(dir)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	backend := &txMockBackend{
		restoreErr: errors.New("rollback ENOSPC"),
	}
	apply := &txSpyApply{err: errors.New("apply failed: checksum mismatch")}
	tx := New(backend, apply.Fn, dir, new(atomic.Uint64), auditLog)

	resp, err := tx.Run(context.Background(), fileIntent(t, "intent-001"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Result != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_PERSISTENT_FAILURE {
		t.Fatalf("Result = %v, want PERSISTENT_FAILURE", resp.Result)
	}
	events, _, err := auditLog.Since(0)
	if err != nil {
		t.Fatalf("audit.Since: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Tool != "apply_intent.rollback_failed" {
		t.Errorf("audit tool = %q, want apply_intent.rollback_failed", ev.Tool)
	}
	if ev.Identity != "intent-001" {
		t.Errorf("audit identity = %q, want intent-001", ev.Identity)
	}
	if !strings.Contains(ev.Outcome, "apply_failed:") {
		t.Errorf("audit outcome must reference apply failure; got %q", ev.Outcome)
	}
	if !strings.Contains(ev.Outcome, "rollback_failed:") {
		t.Errorf("audit outcome must reference rollback failure; got %q", ev.Outcome)
	}
}

// TestLocal_NilAuditSkipsEmission covers the case where the
// caller passes audit=nil (test fixture, or a deployment that
// has not yet set up a chain dir). The tx must not panic and
// must still return the correct response shape.
func TestLocal_NilAuditSkipsEmission(t *testing.T) {
	backend := &txMockBackend{}
	apply := &txSpyApply{}
	tx := New(backend, apply.Fn, t.TempDir(), new(atomic.Uint64), nil)

	resp, err := tx.Run(context.Background(), sampleIntent())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Result != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS {
		t.Errorf("Result = %v, want SUCCESS", resp.Result)
	}
}

// --- apply 结果进本地哈希链(Task 5) ----------------------------------------

// txTestKind 是 tx 测试自用的 intent kind。注册一个可配置错误的执行器后,
// 这些测试经**真实的 exec.Apply 分派**驱动 tx,而不是绕过分派层用 spy ——
// 这样"执行器返回 error ⇒ 非 SUCCESS 且进审计链"这条链路是被端到端覆盖的。
const txTestKind = "tx.test.kind"

// txTestApplyErr 控制 txTestExecutor 的返回值。测试内设置,t.Cleanup 还原;
// 本包测试不并行,故包级变量够用。
var txTestApplyErr error

type txTestExecutor struct{}

func (txTestExecutor) Apply(_ context.Context, _ *agentv1.ApplyIntentRequest) error {
	return txTestApplyErr
}

var txTestRegOnce sync.Once

// execApplyForTest 返回真实的 exec.Apply 分派函数(首次调用时注册测试执行器)。
func execApplyForTest() ApplyFunc {
	txTestRegOnce.Do(func() { exec.Register(txTestKind, txTestExecutor{}) })
	return exec.Apply
}

func execIntent() *agentv1.ApplyIntentRequest {
	return &agentv1.ApplyIntentRequest{IntentId: "intent-exec-1", Kind: txTestKind, Target: "nginx"}
}

func setApplyErr(t *testing.T, err error) {
	t.Helper()
	prev := txTestApplyErr
	txTestApplyErr = err
	t.Cleanup(func() { txTestApplyErr = prev })
}

// apply 成功后链上恰好多一条成功条目。
func TestLocal_ApplyWritesAuditEntry(t *testing.T) {
	dir := t.TempDir()
	auditLog, err := audit.New(dir)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	setApplyErr(t, nil)

	tx := New(&txMockBackend{}, execApplyForTest(), dir, new(atomic.Uint64), auditLog)
	resp, err := tx.Run(context.Background(), execIntent())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Result != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS {
		t.Fatalf("Result = %v, want SUCCESS", resp.Result)
	}

	events, final, err := auditLog.Since(0)
	if err != nil {
		t.Fatalf("audit.Since: %v", err)
	}
	if final != 1 || len(events) != 1 {
		t.Fatalf("chain entries = %d (final=%d), got %d, want 1", len(events), final, len(events))
	}
	if events[0].Tool != "apply_intent.success" {
		t.Errorf("tool = %q, got %q, want apply_intent.success", events[0].Tool, events[0].Tool)
	}
	if events[0].Outcome != "ok" {
		t.Errorf("outcome = %q, got %q, want ok", events[0].Outcome, events[0].Outcome)
	}
}

// 执行器报错时链上落一条 failure 条目(而不是静默丢弃)。
func TestLocal_ApplyFailureWritesFailureEntry(t *testing.T) {
	dir := t.TempDir()
	auditLog, err := audit.New(dir)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	setApplyErr(t, errors.New("executor exploded"))

	tx := New(&txMockBackend{}, execApplyForTest(), dir, new(atomic.Uint64), auditLog)
	resp, err := tx.Run(context.Background(), execIntent())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Result == agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS {
		t.Fatalf("Result = SUCCESS despite executor error (Review Focus #1)")
	}

	events, _, err := auditLog.Since(0)
	if err != nil {
		t.Fatalf("audit.Since: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("chain entries = %d, got %d, want 1", len(events), len(events))
	}
	if events[0].Tool != "apply_intent.failure" {
		t.Errorf("tool = %q, got %q, want apply_intent.failure", events[0].Tool, events[0].Tool)
	}
	if !strings.Contains(events[0].Outcome, "executor exploded") {
		t.Errorf("outcome = %q must carry the executor error", events[0].Outcome)
	}
}

// apply 之后链仍然是完整可验的哈希链(SDK 是权威校验器)。
func TestLocal_ChainStaysIntactAfterApply(t *testing.T) {
	dir := t.TempDir()
	auditLog, err := audit.New(dir)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	setApplyErr(t, nil)

	tx := New(&txMockBackend{}, execApplyForTest(), dir, new(atomic.Uint64), auditLog)
	if _, err := tx.Run(context.Background(), execIntent()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	n, err := sdkAudit.Verify(auditLog.ChainPath())
	if err != nil {
		t.Fatalf("hash chain is not intact: %v", err)
	}
	if n != 1 {
		t.Errorf("verified entries = %d, got %d, want 1", n, n)
	}
}

// 不可重试的执行器失败必须映射成 PERSISTENT_FAILURE,否则控制面会自动重试
// 一个永远不会成功的操作(Review Focus #2)。txTestKind 没有回滚点,所以
// 这里同时断言"不假装还原"。
func TestLocal_PersistentApplyErrorYieldsPersistentFailure(t *testing.T) {
	backend := &txMockBackend{}
	setApplyErr(t, fmt.Errorf("%w: package nope does not exist", exec.ErrPersistent))

	tx := New(backend, execApplyForTest(), t.TempDir(), new(atomic.Uint64), nil)
	resp, err := tx.Run(context.Background(), execIntent())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Result != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_PERSISTENT_FAILURE {
		t.Fatalf("Result = %v, got %v, want PERSISTENT_FAILURE", resp.Result, resp.Result)
	}
	if backend.restoreCalls != 0 {
		t.Errorf("Restore calls = %d, got %d, want 0 (kind 无回滚点)", backend.restoreCalls, backend.restoreCalls)
	}
}

// 有回滚点时,不可重试的失败仍然先还原再报 PERSISTENT —— 结果码与
// "已还原"是两件事,不能因为不重试就跳过还原。
func TestLocal_FileKindPersistentErrorStillRollsBack(t *testing.T) {
	backend := &txMockBackend{}
	apply := &txSpyApply{err: fmt.Errorf("%w: package nope does not exist", exec.ErrPersistent)}

	tx := New(backend, apply.Fn, t.TempDir(), new(atomic.Uint64), nil)
	resp, err := tx.Run(context.Background(), fileIntent(t, "intent-001"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Result != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_PERSISTENT_FAILURE {
		t.Fatalf("Result = %v, got %v, want PERSISTENT_FAILURE", resp.Result, resp.Result)
	}
	if backend.restoreCalls != 1 {
		t.Errorf("Restore calls = %d, got %d, want 1 (rollback still runs)", backend.restoreCalls, backend.restoreCalls)
	}
	if !strings.Contains(resp.ErrorMessage, "rolled back") {
		t.Errorf("ErrorMessage = %q, want 含 rolled back", resp.ErrorMessage)
	}
}

// 可重试的执行器失败仍是 TRANSIENT_FAILURE(回滚成功、重试安全)。
func TestLocal_TransientApplyErrorYieldsTransientFailure(t *testing.T) {
	backend := &txMockBackend{}
	setApplyErr(t, fmt.Errorf("%w: dpkg lock held", exec.ErrTransient))

	tx := New(backend, execApplyForTest(), t.TempDir(), new(atomic.Uint64), nil)
	resp, err := tx.Run(context.Background(), execIntent())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Result != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_TRANSIENT_FAILURE {
		t.Fatalf("Result = %v, got %v, want TRANSIENT_FAILURE", resp.Result, resp.Result)
	}
}

// ctx 取消不得被静默丢弃:链上要留一条明确记录(Review Focus #4)。
func TestLocal_CancelledApplyWritesCancelledEntry(t *testing.T) {
	dir := t.TempDir()
	auditLog, err := audit.New(dir)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	setApplyErr(t, fmt.Errorf("exec: package.set cancelled: %w", context.Canceled))

	tx := New(&txMockBackend{}, execApplyForTest(), dir, new(atomic.Uint64), auditLog)
	resp, err := tx.Run(context.Background(), execIntent())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Result == agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS {
		t.Fatalf("Result = SUCCESS despite cancellation")
	}

	events, _, err := auditLog.Since(0)
	if err != nil {
		t.Fatalf("audit.Since: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("chain entries = %d, got %d, want 1", len(events), len(events))
	}
	if events[0].Tool != "apply_intent.cancelled" {
		t.Errorf("tool = %q, got %q, want apply_intent.cancelled", events[0].Tool, events[0].Tool)
	}
	if !strings.Contains(events[0].Outcome, "cancelled") {
		t.Errorf("outcome = %q must mark the cancellation", events[0].Outcome)
	}
}

// 事务必须按 kind 取回滚点:package 没有回滚点,不得再往 dataDir 下建
// intent-target 暂存目录(那个目录概念已删除)。
func TestLocal_Run_NonFileKindDoesNotCreateStagingDir(t *testing.T) {
	dir := t.TempDir()
	tx := New(&txMockBackend{}, (&txSpyApply{}).Fn, dir, nil, nil)

	if _, err := tx.Run(context.Background(), sampleIntent()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "intent-target")); !os.IsNotExist(err) {
		t.Errorf("intent-target 目录不应再被创建, stat err = %v", err)
	}
}
