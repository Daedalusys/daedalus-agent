// Package tx owns the daedalus-agent local transaction state
// machine that gives ApplyIntent its pre-snapshot → apply →
// post-snapshot / rollback semantics.
//
// State machine (in pseudo-code):
//
//	pre-snapshot:   backend.CreateSnapshot(target → snap)
//	apply:          applyFn(ctx, intent)              [stub in T6]
//	if apply.ok:
//	    success:    result = SUCCESS, audit_seq++, log
//	else:
//	    rollback:   backend.Restore(snap → target)
//	    if rollback.ok:
//	        result = TRANSIENT_FAILURE (apply failed but
//	                 state was unwound; safe to retry)
//	    else:
//	        result = PERSISTENT_FAILURE (give up; needs
//	                 human — rollback machinery itself
//	                 broke)
//
// We deliberately separate the three failure modes
// (pre-snapshot fail, apply fail with rollback, apply fail
// without rollback) so the control plane can render a useful
// failure bucket to the human reviewer. A naive
// "ApplyIntent failed" message would lose the distinction
// between "rollback machinery is OK; retry" and "rollback
// machinery is broken; the host is in an undefined state".
//
// RF3 surface (plan §RF3 + spec §5.2):
//
//   The handler-level "snap_capability == NONE → refuse"
//   check lives in internal/rpc/apply_intent.go, not here.
//   Tx assumes the caller has already admitted the intent
//   (i.e. selected a non-nil backend). Defence in depth: even
//   if a future caller forgets the admission check, the tx's
//   pre-snapshot step on a nil backend will panic with a
//   clear nil-deref — surfaced loudly, not silently swallowed
//   as "best-effort".
//
// Audit / audit_seq:
//
//   T6 uses a process-local atomic.Uint64 counter as the
//   audit_seq source. T7 (issue in followup queue) replaces
//   this with the JSONL audit chain — see internal/rpc/
//   handlers_test.go for the placeholder stub.
//
// Apply stub:
//
//   T6 wires applyFn as a no-op (log + nil). T7+ replaces it
//   with the real package manager / filesystem executor. The
//   tx interface itself does not change between T6 and T7+.
//
package tx

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sync/atomic"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/rollback"
)

// ApplyFunc is the apply step the tx state machine invokes
// between pre-snapshot and post-snapshot. T6 stubs it as a
// no-op (logs the intent, returns nil). T7+ replaces it with
// the real package manager / filesystem executor (apt-get,
// yum, transactional update, etc.).
//
// The function MUST honour ctx.Done(). A long apply step
// without ctx observation can stall the gRPC server's
// graceful shutdown — see internal/daemon (30s
// ShutdownTimeout).
type ApplyFunc func(ctx context.Context, intent *agentv1.ApplyIntentRequest) error

// Tx is the per-intent transaction. Construct via New() with
// a non-nil rollback.Backend (the handler enforces the RF3
// admission check separately).
type Tx interface {
	// Run executes the pre-snapshot → apply → post-snapshot
	// state machine and returns the wire-level response.
	//
	// Tx.Run ALWAYS returns a non-nil (*ApplyIntentResponse,
	// nil) — even on tx failures. The result-code field
	// (SUCCESS / TRANSIENT_FAILURE / PERSISTENT_FAILURE) is
	// the tx-level outcome, NOT a gRPC transport error. Only
	// truly exceptional conditions (out of memory, nil
	// pointer on a nil backend, etc.) return a non-nil
	// error — the handler maps those to codes.Internal.
	Run(ctx context.Context, intent *agentv1.ApplyIntentRequest) (*agentv1.ApplyIntentResponse, error)
}

// Local is the production Tx implementation. Holds the
// rollback backend, the apply step, and the data dir under
// which snapshot files live. Safe for concurrent use across
// goroutines IF the backend is safe (BtrfsBackend /
// RsyncBackend are both safe per-call as long as the same
// backend is not invoked in parallel on the same path — the
// gRPC server processes one ApplyIntent per stream, so this
// is true in practice).
type Local struct {
	backend  rollback.Backend
	apply    ApplyFunc
	dataDir  string
	auditSeq *atomic.Uint64
}

// New constructs a Local transaction. The backend MUST be
// non-nil (callers enforce RF3 admission upstream).
//
// auditSeq may be nil — in that case the tx uses a private
// counter. Production wires a shared *atomic.Uint64 so audit
// sequence numbers are unique across all RPCs.
func New(backend rollback.Backend, apply ApplyFunc, dataDir string, auditSeq *atomic.Uint64) *Local {
	if auditSeq == nil {
		auditSeq = new(atomic.Uint64)
	}
	return &Local{
		backend:  backend,
		apply:    apply,
		dataDir:  dataDir,
		auditSeq: auditSeq,
	}
}

// Run executes the state machine. See the package comment
// for the semantics. The returned response's result field is
// the wire-level outcome; ErrorMessage carries a
// human-readable triage string; AuditSeq is the per-process
// monotonic sequence number (T7 replaces with the JSONL
// chain).
func (t *Local) Run(ctx context.Context, intent *agentv1.ApplyIntentRequest) (*agentv1.ApplyIntentResponse, error) {
	if intent == nil {
		// Defence in depth: handler already validates
		// this, but a tx caller bypassing the handler
		// (e.g. a future test) should still see a clean
		// error rather than a nil-deref.
		return nil, fmt.Errorf("tx: nil intent")
	}

	// 1. Build paths. We use filepath.Join (not path.Join)
	// because the agent runs on linux only — '/' is the
	// separator regardless of host OS. filepath.Join also
	// cleans redundant separators that an intent_id might
	// carry.
	sourcePath := filepath.Join(t.dataDir, "intent-target", intent.Target)
	snapshotPath := filepath.Join(t.dataDir, "snapshots", intent.IntentId+".snap")

	// 2. Pre-snapshot. A failure here means we never
	// modified the host — return TRANSIENT_FAILURE so the
	// control plane retries (e.g. transient ENOSPC on the
	// snapshot volume).
	seq := t.auditSeq.Add(1)
	if err := t.backend.CreateSnapshot(sourcePath, snapshotPath); err != nil {
		log.Printf("agent: tx pre-snapshot failed: intent_id=%q target=%q err=%v audit_seq=%d",
			intent.IntentId, intent.Target, err, seq)
		return &agentv1.ApplyIntentResponse{
			Result:       agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_TRANSIENT_FAILURE,
			ErrorMessage: fmt.Sprintf("pre-snapshot failed: %v", err),
			AuditSeq:     seq,
		}, nil
	}

	// 3. Apply. T6 stub always returns nil (success); T7+
	// replaces with real package / fs operations.
	if err := t.apply(ctx, intent); err != nil {
		// Apply failed. Attempt rollback.
		rbErr := t.backend.Restore(snapshotPath, sourcePath)
		if rbErr != nil {
			// Rollback ALSO failed. This is the
			// PERSISTENT_FAILURE case — host may be
			// in an undefined state. Audit it loudly
			// so the operator sees the dual failure.
			rbSeq := t.auditSeq.Add(1)
			log.Printf("agent: tx apply failed AND rollback failed: intent_id=%q apply_err=%v rollback_err=%v audit_seq=%d",
				intent.IntentId, err, rbErr, rbSeq)
			return &agentv1.ApplyIntentResponse{
				Result:       agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_PERSISTENT_FAILURE,
				ErrorMessage: fmt.Sprintf("apply failed AND rollback failed (host may be in undefined state): apply=%v rollback=%v", err, rbErr),
				AuditSeq:     rbSeq,
			}, nil
		}
		// Apply failed but rollback succeeded —
		// TRANSIENT_FAILURE (safe to retry).
		rbSeq := t.auditSeq.Add(1)
		log.Printf("agent: tx apply failed, rolled back: intent_id=%q apply_err=%v audit_seq=%d",
			intent.IntentId, err, rbSeq)
		return &agentv1.ApplyIntentResponse{
			Result:       agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_TRANSIENT_FAILURE,
			ErrorMessage: fmt.Sprintf("apply failed, rolled back successfully: %v", err),
			AuditSeq:     rbSeq,
		}, nil
	}

	// 4. Success. Build the response with a deterministic
	// tx_id derived from intent_id (so retry / re-apply of
	// the same intent yields the same tx_id — useful for
	// the audit chain in T7).
	txID := "tx-" + intent.IntentId
	return &agentv1.ApplyIntentResponse{
		Result:   agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS,
		TxId:     txID,
		AuditSeq: seq,
	}, nil
}
