// Package tx 拥有 daedalus-agent 的本地事务状态机,给 ApplyIntent 提供
// pre-snapshot → apply → post-snapshot / rollback 语义。
//
// 状态机(伪码):
//
//	pre-snapshot:   backend.CreateSnapshot(target → snap)
//	apply:          applyFn(ctx, intent)              [internal/exec]
//	if apply.ok:
//	    success:    result = SUCCESS, audit_seq++, log
//	else:
//	    rollback:   backend.Restore(snap → target)
//	    if rollback.ok:
//	        result = PERSISTENT_FAILURE,若执行器把该失败判为不可重试
//	                 (exec.ErrPersistent / ErrUnsupportedKind);
//	                 否则 TRANSIENT_FAILURE(已回滚,重试安全)
//	    else:
//	        result = PERSISTENT_FAILURE(回滚机制本身也坏了,需人工介入)
//
// 三种失败模式(快照失败、apply 失败但回滚成功、apply 失败且回滚失败)刻意
// 分开上报,控制面才能给出有用的失败分桶。一句笼统的 "ApplyIntent failed"
// 会丢掉「回滚机制还好,可以重试」与「回滚机制坏了,宿主状态未定义」的区别。
//
// 可重试与否来自执行器的错误哨兵(internal/exec):包不存在、kind 不认识
// 不该自动重试;包管理器锁被占、网络抖动可以。执行器判不出来时按不可重试
// 上报 —— 自动重试一个永远不会成功的操作,代价更大。
//
// RF3 面(plan §RF3 + spec §5.2):
//
//	handler 层的「snap_capability == NONE → 拒绝」在
//	internal/rpc/apply_intent.go,不在这里。tx 假定调用方已受理该 intent
//	(即已选中非 nil backend)。纵深防御:即便未来有调用方漏了受理检查,
//	这里对 nil backend 做 pre-snapshot 也会以一个清楚的 nil 解引用炸出来,
//	而不是被 "best-effort" 悄悄吞掉。
//
// 审计 / audit_seq:
//
//	审计序号取自进程内的 atomic.Uint64 计数器;事件落盘走 internal/audit
//	(单一入口,由 sdkAudit.LogAudit 写哈希链)。
//
// 回滚能力边界:
//
//	文件级快照只捕获 <dataDir>/intent-target/<target> 这个暂存路径。当前
//	wire 上只有资源 kind(package/service),它们改的是宿主状态(包库 /
//	单元文件),文件级快照对它们没有还原能力 —— agent 的回滚本就是
//	best-effort(产品叙事 "best-effort rollback, not bootc");"精确到
//	kind 的快照源选择"是另一个待决的设计缺口。
package tx

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/audit"
	"github.com/Daedalusys/daedalus-agent/internal/exec"
	"github.com/Daedalusys/daedalus-agent/internal/rollback"
)

// ApplyFunc 是事务状态机在 pre-snapshot 与 post-snapshot 之间调用的 apply
// 步骤。生产实现是 internal/exec 的 Apply:按 intent.Kind 分派到各执行器。
//
// 实现**必须**尊重 ctx.Done():不观察 ctx 的长 apply 会卡住 gRPC 服务的
// 优雅退出 —— 见 internal/daemon 的 30s ShutdownTimeout。
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
	audit    *audit.Local
}

// New constructs a Local transaction. The backend MUST be
// non-nil (callers enforce RF3 admission upstream).
//
// auditSeq may be nil — in that case the tx uses a private
// counter. Production wires a shared *atomic.Uint64 so audit
// sequence numbers are unique across all RPCs.
//
// audit may be nil — the tx then skips audit-event emission
// (no error). Production wires the per-process *audit.Local
// shared with report_status.go; tests pass nil to avoid
// the chain file.
func New(backend rollback.Backend, apply ApplyFunc, dataDir string, auditSeq *atomic.Uint64, auditLog *audit.Local) *Local {
	if auditSeq == nil {
		auditSeq = new(atomic.Uint64)
	}
	return &Local{
		backend:  backend,
		apply:    apply,
		dataDir:  dataDir,
		auditSeq: auditSeq,
		audit:    auditLog,
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
	//
	// 暂存目标名走 filepath.Base:intent.Target 由控制面提供,含 ".." 时
	// 会让 Join 的结果爬出 dataDir —— 而那一步之后是 MkdirAll,等于路径穿越。
	sourcePath := filepath.Join(t.dataDir, "intent-target", filepath.Base(intent.Target))
	snapshotPath := filepath.Join(t.dataDir, "snapshots", intent.IntentId+".snap")

	// 2. Pre-snapshot.
	//
	// 先把暂存目标建出来:文件级快照后端(btrfs / rsync)对不存在的源路径
	// 直接失败,而资源 kind(package/service)的 intent 动的是宿主状态,
	// 没有谁会预先 materialize 这个目录。不建出来,事务会在 pre-snapshot
	// 就 dead-end,真正的执行器根本没机会跑。
	//
	// 注意这**不**等于"回滚万事大吉":文件级快照只捕获这个暂存路径,对
	// package 安装 / service 启停这类宿主状态变更并无还原能力 —— agent 的
	// 回滚本就是 best-effort(产品叙事 "best-effort rollback, not bootc"),
	// 精确到 kind 的快照源选择是另一个待决的设计缺口。
	if mkErr := os.MkdirAll(sourcePath, 0o755); mkErr != nil {
		seq := t.auditSeq.Add(1)
		log.Printf("agent: tx staged-target mkdir failed: intent_id=%q target=%q err=%v audit_seq=%d",
			intent.IntentId, intent.Target, mkErr, seq)
		return &agentv1.ApplyIntentResponse{
			Result:       agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_TRANSIENT_FAILURE,
			ErrorMessage: fmt.Sprintf("staged target unavailable: %v", mkErr),
			AuditSeq:     seq,
		}, nil
	}

	// A failure here means we never modified the host — return
	// TRANSIENT_FAILURE so the control plane retries (e.g.
	// transient ENOSPC on the snapshot volume).
	seq := t.auditSeq.Add(1)
	if err := t.backend.CreateSnapshot(sourcePath, snapshotPath); err != nil {
		log.Printf("agent: tx pre-snapshot failed: intent_id=%q target=%q err=%v audit_seq=%d",
			intent.IntentId, intent.Target, err, seq)
		// Audit the pre-snapshot failure too: without
		// the pre-snapshot we did NOT modify the host,
		// but we still want a tamper-evident record of
		// "tx was attempted, pre-snapshot broke".
		if t.audit != nil {
			if _, aerr := t.audit.Log(audit.Event{
				Identity: intent.IntentId,
				Tool:     "apply_intent.presnapshot_failed",
				Args: map[string]string{
					"target": intent.Target,
					"kind":   intent.Kind,
				},
				Outcome: fmt.Sprintf("presnapshot_failed: %v", err),
			}); aerr != nil {
				log.Printf("agent: audit log (presnapshot_failed) failed: %v", aerr)
			}
		}
		return &agentv1.ApplyIntentResponse{
			Result:       agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_TRANSIENT_FAILURE,
			ErrorMessage: fmt.Sprintf("pre-snapshot failed: %v", err),
			AuditSeq:     seq,
		}, nil
	}

	// 3. Apply. 真实执行器(internal/exec)在这里改机器。失败时先看
	// 错误是可重试还是不可重试,再决定回滚后的上报结果 —— 控制面据此
	// 决定要不要自动重试(TRANSIENT 最多 3 次;PERSISTENT 不重试)。
	if err := t.apply(ctx, intent); err != nil {
		cancelled := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		persistent := errors.Is(err, exec.ErrPersistent) || errors.Is(err, exec.ErrUnsupportedKind)

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
			if t.audit != nil {
				if _, aerr := t.audit.Log(audit.Event{
					Identity: intent.IntentId,
					Tool:     "apply_intent.rollback_failed",
					Args: map[string]string{
						"target": intent.Target,
						"kind":   intent.Kind,
					},
					Outcome: fmt.Sprintf("apply_failed: %v; rollback_failed: %v", err, rbErr),
				}); aerr != nil {
					log.Printf("agent: audit log (rollback_failed) failed: %v", aerr)
				}
			}
			return &agentv1.ApplyIntentResponse{
				Result:       agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_PERSISTENT_FAILURE,
				ErrorMessage: fmt.Sprintf("apply failed AND rollback failed (host may be in undefined state): apply=%v rollback=%v", err, rbErr),
				AuditSeq:     rbSeq,
			}, nil
		}
		// apply 失败但回滚成功。上报结果取决于这次失败值不值得重试:
		// 不可重试的失败即便回滚成功也必须报 PERSISTENT,否则控制面会
		// 自动重试一个永远不会成功的操作。
		rbSeq := t.auditSeq.Add(1)
		log.Printf("agent: tx apply failed, rolled back: intent_id=%q apply_err=%v audit_seq=%d",
			intent.IntentId, err, rbSeq)
		if t.audit != nil {
			tool := "apply_intent.failure"
			outcome := fmt.Sprintf("apply_failed: %v (rolled back)", err)
			if cancelled {
				// 控制面超时取消必须留下可区分的记录,不能与普通
				// 失败混为一谈。
				tool = "apply_intent.cancelled"
				outcome = fmt.Sprintf("cancelled: %v (rolled back)", err)
			}
			if _, aerr := t.audit.Log(audit.Event{
				Identity: intent.IntentId,
				Tool:     tool,
				Args: map[string]string{
					"target": intent.Target,
					"kind":   intent.Kind,
				},
				Outcome: outcome,
			}); aerr != nil {
				log.Printf("agent: audit log (failure) failed: %v", aerr)
			}
		}
		if persistent {
			return &agentv1.ApplyIntentResponse{
				Result:       agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_PERSISTENT_FAILURE,
				ErrorMessage: fmt.Sprintf("apply failed with a non-retryable error, rolled back successfully: %v", err),
				AuditSeq:     rbSeq,
			}, nil
		}
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
	if t.audit != nil {
		if _, aerr := t.audit.Log(audit.Event{
			Identity: intent.IntentId,
			Tool:     "apply_intent.success",
			Args: map[string]string{
				"target": intent.Target,
				"kind":   intent.Kind,
				"tx_id":  txID,
			},
			Outcome: "ok",
		}); aerr != nil {
			log.Printf("agent: audit log (success) failed: %v", aerr)
		}
	}
	return &agentv1.ApplyIntentResponse{
		Result:   agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS,
		TxId:     txID,
		AuditSeq: seq,
	}, nil
}
