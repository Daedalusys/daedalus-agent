// Package tx 拥有 daedalus-agent 的本地事务状态机,给 ApplyIntent 提供
// pre-snapshot → apply → post-snapshot / rollback 语义。
//
// 状态机(伪码):
//
//	point = rollback.PointFor(intent.Kind)
//	if point == file:
//	    pre-snapshot: backend.CreateSnapshot(intent.Target → <dataDir>/snapshots/<intent_id>.snap)
//	apply:            applyFn(ctx, intent)              [internal/exec]
//	if apply.ok:
//	    success:      result = SUCCESS, audit_seq++, log
//	else if point == file:
//	    rollback:     backend.Restore(snap → intent.Target)
//	    if rollback.ok:
//	        result = PERSISTENT_FAILURE,若执行器把该失败判为不可重试
//	                 (exec.ErrPersistent / ErrUnsupportedKind);
//	                 否则 TRANSIENT_FAILURE(已回滚,重试安全)
//	    else:
//	        result = PERSISTENT_FAILURE(回滚机制本身也坏了,需人工介入)
//	else:
//	    no rollback:  result 仍按可重试性分;文案写明 no rollback point
//
// 三种失败模式(快照失败、apply 失败但回滚成功、apply 失败且回滚失败)刻意
// 分开上报,控制面才能给出有用的失败分桶。一句笼统的 "ApplyIntent failed"
// 会丢掉「回滚机制还好,可以重试」与「回滚机制坏了,宿主状态未定义」的区别。
//
// 可重试与否来自执行器的错误哨兵(internal/exec):包不存在、kind 不认识
// 不该自动重试;包管理器锁被占、网络抖动可以。执行器判不出来时按不可重试
// 上报 —— 自动重试一个永远不会成功的操作,代价更大。
//
// 准入面:
//
//	handler 层的准入(高危且无回滚点 → 拒绝)在 internal/rpc/apply_intent.go,
//	不在这里。tx 假定调用方已受理该 intent。纵深防御:即便未来有调用方漏了
//	受理检查,这里对 nil backend 做 pre-snapshot 也会以一个清楚的 nil 解引用
//	炸出来,而不是被 "best-effort" 悄悄吞掉。
//
// 审计 / audit_seq:
//
//	审计序号取自进程内的 atomic.Uint64 计数器;事件落盘走 internal/audit
//	(单一入口,由 sdkAudit.LogAudit 写哈希链)。
//
// 回滚能力边界:
//
//	只有 kind=file 有回滚点,快照源就是被改文件本体。package / service 改的
//	是宿主状态(包库 / 单元文件),文件级副本对它们没有还原能力,因此不快照、
//	不还原,也不说 "rolled back"。
package tx

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
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
// a non-nil rollback.Backend (the handler enforces the
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

	// pathsFn 解析"这次 intent 要给哪些路径留副本"。nil 表示按默认处理:
	// 单文件 intent 快照 target 本体。目录级 intent 必须注入 —— 否则没有
	// 快照点可用,宁可不写也不能"写完再想办法还原"。
	pathsFn func(*agentv1.ApplyIntentRequest) ([]string, error)
}

// Option 是 Local 的可选装配项。
type Option func(*Local)

// WithSnapshotPaths 注入快照路径的解析函数。
//
// 为什么是函数而不是路径列表:路径由 spec 决定,spec 在 tx 之外解析。handler
// 解析完把结果交给 tx,tx 不必知道 spec 的形状。
func WithSnapshotPaths(fn func(*agentv1.ApplyIntentRequest) ([]string, error)) Option {
	return func(l *Local) {
		l.pathsFn = fn
	}
}

// postStateRecorder 能把写后状态记进快照清单,供还原前的冲突检测使用。
//
// 它是可选能力而不是 PathSetBackend 的一部分:不是每个集合后端都做得出
// 冲突检测(没有 manifest 就没有 post 可写)。
type postStateRecorder interface {
	RecordPostState(snapshotDir string, paths []string) error
}

// New constructs a Local transaction. backend 在 intent 没有回滚点
// (PointNone)时可以是 nil —— 那种 intent 根本不建快照;一旦有回滚点却拿到
// nil 后端,Run 会 fail-closed 报错,而不是等到 CreateSnapshot 上空指针。
//
// auditSeq may be nil — in that case the tx uses a private
// counter. Production wires a shared *atomic.Uint64 so audit
// sequence numbers are unique across all RPCs.
//
// audit may be nil — the tx then skips audit-event emission
// (no error). Production wires the per-process *audit.Local
// shared with report_status.go; tests pass nil to avoid
// the chain file.
func New(backend rollback.Backend, apply ApplyFunc, dataDir string, auditSeq *atomic.Uint64, auditLog *audit.Local, opts ...Option) *Local {
	if auditSeq == nil {
		auditSeq = new(atomic.Uint64)
	}
	l := &Local{
		backend:  backend,
		apply:    apply,
		dataDir:  dataDir,
		auditSeq: auditSeq,
		audit:    auditLog,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(l)
		}
	}
	return l
}

// resolveSnapshotPaths 取这次 intent 要留副本的路径。目录级 intent 没有
// 注入解析函数时不猜默认值:整个 target 目录不是"一组点名子路径"。
func (t *Local) resolveSnapshotPaths(intent *agentv1.ApplyIntentRequest) ([]string, error) {
	if t.pathsFn == nil {
		return nil, errors.New("no snapshot path resolver configured for a directory rollback point")
	}
	paths, err := t.pathsFn(intent)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, errors.New("snapshot path resolver returned no paths")
	}
	return paths, nil
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

	// 1. 按 kind + spec 形状取回滚点。表外 kind 一律没有回滚点 ——
	// 「不认识」不等于「有还原手段」;同是 file,entries 形态是一组路径的
	// 目录回滚点,旧形态仍是单文件。
	point := rollback.PointForIntent(intent)

	// 有回滚点就必须有后端:装配错了当场失败,别等到 CreateSnapshot 上空指针。
	// 没有回滚点时 backend 允许为 nil(handler 不给 PointNone 选后端)。
	if point != rollback.PointNone && t.backend == nil {
		return nil, fmt.Errorf("tx: rollback point %v needs a backend, got nil", point)
	}

	// 目录级回滚点必须由"能留一组路径"的后端承接。单路径后端拿到目录只会
	// 在快照时失败,那没问题;但类型不对时连失败都表达不清楚,当场拒绝。
	var pathSet rollback.PathSetBackend
	if point == rollback.PointTree {
		var ok bool
		pathSet, ok = t.backend.(rollback.PathSetBackend)
		if !ok {
			return nil, fmt.Errorf("tx: rollback point %v needs a path-set backend, got %T", point, t.backend)
		}
	}

	// 2. 快照路径按 intent_id 固定:同一 intent 重投时要能认出上一轮的
	// 副本,而不是每次落一份新的。Base 挡住 intent_id 里的路径分隔符 ——
	// 它来自控制面,不该有机会把写点带出 dataDir。
	//
	// 单文件快照是一个 .snap 文件;目录快照是一个目录(manifest.json +
	// objects/),两者的生命周期与 GC 口径不同,不能共用路径。
	snapshotPath := filepath.Join(t.dataDir, "snapshots", filepath.Base(intent.IntentId)+".snap")
	if point == rollback.PointTree {
		snapshotPath = filepath.Join(t.dataDir, "snapshots", filepath.Base(intent.IntentId))
	}

	// 目录级 intent 必须先知道"点名了哪些路径"才谈得上快照。拿不到就
	// fail-closed:宁可整条 intent 不写,也不能把整个目录当一个文件拍。
	var snapshotPaths []string
	if point == rollback.PointTree {
		paths, perr := t.resolveSnapshotPaths(intent)
		if perr != nil {
			rbSeq := t.auditSeq.Add(1)
			log.Printf("agent: tx cannot resolve snapshot paths: intent_id=%q err=%v audit_seq=%d",
				intent.IntentId, perr, rbSeq)
			return &agentv1.ApplyIntentResponse{
				Result:       agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_PERSISTENT_FAILURE,
				ErrorMessage: fmt.Sprintf("directory rollback needs snapshot paths: %v", perr),
				AuditSeq:     rbSeq,
			}, nil
		}
		snapshotPaths = paths
	}

	seq := t.auditSeq.Add(1)

	// 3. Pre-snapshot。只有存在回滚点的 kind 才拍:package / service 改的
	// 是宿主状态,文件级副本对它们没有还原能力,拍了只会给"rolled back"
	// 这句谎话提供口径。
	var sourcePath string
	if point == rollback.PointFile || point == rollback.PointTree {
		var snapErr error
		if point == rollback.PointFile {
			sourcePath = intent.Target
			snapErr = t.backend.CreateSnapshot(sourcePath, snapshotPath)
		} else {
			snapErr = pathSet.CreateSnapshotSet(snapshotPaths, snapshotPath)
		}
		// 失败意味着宿主还没被改动 —— 报 TRANSIENT 让控制面重试(典型是
		// 快照卷 ENOSPC)。
		if snapErr != nil {
			log.Printf("agent: tx pre-snapshot failed: intent_id=%q target=%q err=%v audit_seq=%d",
				intent.IntentId, intent.Target, snapErr, seq)
			// 快照失败也要留痕:虽然没改宿主,但"事务被尝试过、快照坏了"
			// 本身要有可追溯的记录。
			if t.audit != nil {
				if _, aerr := t.audit.Log(audit.Event{
					Identity: intent.IntentId,
					Tool:     "apply_intent.presnapshot_failed",
					Args: map[string]string{
						"target": intent.Target,
						"kind":   intent.Kind,
					},
					Outcome: fmt.Sprintf("presnapshot_failed: %v", snapErr),
				}); aerr != nil {
					log.Printf("agent: audit log (presnapshot_failed) failed: %v", aerr)
				}
			}
			return &agentv1.ApplyIntentResponse{
				Result:       agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_TRANSIENT_FAILURE,
				ErrorMessage: fmt.Sprintf("pre-snapshot failed: %v", snapErr),
				AuditSeq:     seq,
			}, nil
		}
	}

	// 4. Apply。真实执行器(internal/exec)在这里改机器。失败时先看错误
	// 是可重试还是不可重试,再决定要不要还原、以及上报什么结果码 ——
	// 控制面据此决定要不要自动重试(TRANSIENT 最多 3 次;PERSISTENT 不重试)。
	if err := t.apply(ctx, intent); err != nil {
		cancelled := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		persistent := errors.Is(err, exec.ErrPersistent) || errors.Is(err, exec.ErrUnsupportedKind)

		// 4a. 没有回滚点(PointNone,以及本期无 producer 的 PointVolume)。
		// 不快照也就不还原,文案必须如实说明 —— 这句 "no rollback point"
		// 是控制面区分"已还原可重试"与"宿主状态未知"的唯一依据。
		if point != rollback.PointFile && point != rollback.PointTree {
			rbSeq := t.auditSeq.Add(1)
			log.Printf("agent: tx apply failed, no rollback point: intent_id=%q kind=%q apply_err=%v audit_seq=%d",
				intent.IntentId, intent.Kind, err, rbSeq)
			if t.audit != nil {
				tool := "apply_intent.failure"
				outcome := fmt.Sprintf("apply_failed: %v (no rollback point for kind=%s)", err, intent.Kind)
				if cancelled {
					tool = "apply_intent.cancelled"
					outcome = fmt.Sprintf("cancelled: %v (no rollback point for kind=%s)", err, intent.Kind)
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
			result := agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_TRANSIENT_FAILURE
			if persistent {
				result = agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_PERSISTENT_FAILURE
			}
			return &agentv1.ApplyIntentResponse{
				Result:       result,
				ErrorMessage: fmt.Sprintf("apply failed, no rollback point for kind=%s: %v", intent.Kind, err),
				AuditSeq:     rbSeq,
			}, nil
		}

		// 4b. 有回滚点:先把副本拷回去。目录级 intent 还原的是一组路径,
		// 单文件 intent 还原的是那一个文件。
		var rbErr error
		restoredDesc := sourcePath
		if point == rollback.PointTree {
			rbErr = pathSet.RestoreSet(snapshotPath, snapshotPaths)
			restoredDesc = strings.Join(snapshotPaths, ",")
		} else {
			rbErr = t.backend.Restore(snapshotPath, sourcePath)
		}
		if rbErr != nil {
			// 还原也失败。这是 PERSISTENT_FAILURE 的场合 —— 宿主可能
			// 处于未定义状态,必须大喊出来。
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
		// 还原成功。结果码仍取决于这次失败值不值得重试:不可重试的失败
		// 即便还原成功也必须报 PERSISTENT,否则控制面会重试一个永远不会
		// 成功的操作。
		rbSeq := t.auditSeq.Add(1)
		log.Printf("agent: tx apply failed, rolled back: intent_id=%q path=%q apply_err=%v audit_seq=%d",
			intent.IntentId, restoredDesc, err, rbSeq)
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
		result := agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_TRANSIENT_FAILURE
		if persistent {
			result = agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_PERSISTENT_FAILURE
		}
		return &agentv1.ApplyIntentResponse{
			Result:       result,
			ErrorMessage: fmt.Sprintf("apply failed, rolled back %s: %v", restoredDesc, err),
			AuditSeq:     rbSeq,
		}, nil
	}

	// 4. Success. Build the response with a deterministic
	// tx_id derived from intent_id (so retry / re-apply of
	// the same intent yields the same tx_id — useful for
	// the audit chain in T7).
	//
	// 目录级 intent 顺手记下写后状态:还原前的冲突检测靠它判断"这条路径
	// 在 apply 之后有没有被别人动过"。记不上只记日志 —— 这次 apply 已经
	// 发生,不能因为记状态失败就把它报成失败。
	if point == rollback.PointTree {
		if recorder, ok := t.backend.(postStateRecorder); ok {
			if perr := recorder.RecordPostState(snapshotPath, snapshotPaths); perr != nil {
				log.Printf("agent: tx post state recording failed: intent_id=%q err=%v", intent.IntentId, perr)
			}
		}
	}
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
