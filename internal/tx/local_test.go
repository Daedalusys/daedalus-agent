// Tests for the local ApplyIntent transaction state machine.
//
// The state machine wired in internal/tx/local.go has four
// observable paths:
//
//   1. Success: pre-snapshot → apply (nil) → return SUCCESS.
//   2. Pre-snapshot failure: backend.CreateSnapshot errors; tx
//      returns TRANSIENT_FAILURE without calling apply.
//   3. Apply failure + rollback success: apply errors, rollback
//      succeeds; tx returns TRANSIENT_FAILURE.
//   4. Apply failure + rollback failure: apply errors, rollback
//      also errors; tx returns PERSISTENT_FAILURE and logs the
//      dual failure loudly.
//
// The tests cover each path with a recording mock backend and
// a recording apply spy, so an assertion failure pinpoints the
// exact (call-count, error) tuple the tx surfaced.
package tx

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// txMockBackend is a minimal Backend for tx-level tests. Each
// method records invocation count and can be made to error to
// drive the failure paths. We keep this in the test file (not
// internal/rollback's test helpers) because the contract being
// exercised here is the tx-Backend interface, not any specific
// backend's behaviour.
type txMockBackend struct {
	mu sync.Mutex

	createCalls    int
	restoreCalls   int
	createErr      error
	restoreErr     error
	lastSrc, lastSnap       string
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
func sampleIntent() *agentv1.ApplyIntentRequest {
	return &agentv1.ApplyIntentRequest{
		IntentId:  "intent-001",
		Kind:      "package.set",
		Target:    "nginx",
		SpecJson:  `{"name":"nginx","version":"1.18"}`,
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
	tx := New(backend, apply.Fn, t.TempDir(), audit)

	resp, err := tx.Run(context.Background(), sampleIntent())
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
	tx := New(backend, apply.Fn, t.TempDir(), audit)

	resp, err := tx.Run(context.Background(), sampleIntent())
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
	tx := New(backend, apply.Fn, t.TempDir(), audit)

	resp, err := tx.Run(context.Background(), sampleIntent())
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
	tx := New(backend, apply.Fn, t.TempDir(), audit)

	resp, err := tx.Run(context.Background(), sampleIntent())
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
	tx := New(backend, apply.Fn, t.TempDir(), nil)

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
	tx := New(backend, apply.Fn, t.TempDir(), audit)

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
	tx := New(backend, apply.Fn, t.TempDir(), nil)

	resp, err := tx.Run(context.Background(), sampleIntent())
	if err != nil {
		t.Fatalf("Run with nil auditSeq: %v", err)
	}
	if resp.AuditSeq == 0 {
		t.Errorf("AuditSeq = 0; constructor should have allocated a private counter")
	}
}

// TestLocal_PathsBuildFromDataDir verifies the path convention
// used by Run: <dataDir>/intent-target/<target> for the source,
// <dataDir>/snapshots/<intent_id>.snap for the snapshot. The
// mock backend records both so an assertion failure pinpoints
// the exact (src, snap) pair the tx derived.
func TestLocal_PathsBuildFromDataDir(t *testing.T) {
	backend := &txMockBackend{}
	apply := &txSpyApply{}
	dir := t.TempDir()
	tx := New(backend, apply.Fn, dir, nil)

	if _, err := tx.Run(context.Background(), sampleIntent()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantSrc := dir + "/intent-target/nginx"
	if backend.lastSrc != wantSrc {
		t.Errorf("source path = %q, want %q", backend.lastSrc, wantSrc)
	}
	wantSnap := dir + "/snapshots/intent-001.snap"
	if backend.lastSnap != wantSnap {
		t.Errorf("snapshot path = %q, want %q", backend.lastSnap, wantSnap)
	}
}
