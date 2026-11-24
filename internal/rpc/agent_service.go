// agent_service.go owns the agentService struct and the shared
// helpers behind its RPC handlers.
//
// Each RPC handler lives in its own file (register.go,
// heartbeat.go, apply_intent.go, snapshot_request.go,
// report_status.go, rollback_intent.go, tool_call.go) — this
// file is the struct composition + shared helpers + the
// per-handler wiring.
//
// We embed agentv1.UnimplementedAgentServiceServer so that a
// proto change compiles without touching this file. 注意:内嵌桩
// 会让新增 RPC **编译照常通过**并在运行时静默返回
// codes.Unimplemented —— 新方法必须自己落实现并加测试,不能指望
// 编译器提醒。
package rpc

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/audit"
	"github.com/Daedalusys/daedalus-agent/internal/cert"
	"github.com/Daedalusys/daedalus-agent/internal/config"
	"github.com/Daedalusys/daedalus-agent/internal/exec"
	"github.com/Daedalusys/daedalus-agent/internal/rollback"
	"github.com/Daedalusys/daedalus-agent/internal/snapshot"
	"github.com/Daedalusys/daedalus-agent/internal/toolcall"
	"github.com/Daedalusys/daedalus-agent/internal/tx"
)

// heartbeatIntervalSeconds is the interval the agent advertises
// in its Register response. The control plane can override this
// (the proto's HeartbeatIntervalSeconds field is editable), but
// until it does, this constant is the answer.
//
// 30s is chosen to balance:
//   - Low heartbeat rate: less control-plane-side load, less
//     network chatter.
//   - High heartbeat rate: faster dead-node detection.
//
// 30s is a standard compromise for fleet agents; we can revisit
// if the control plane flags false-positive dead nodes.
const heartbeatIntervalSeconds = 30

// agentService 是 agentv1.AgentServiceServer 的生产实现。每条 RPC 都有
// 自己的 handler 文件;内嵌桩只负责让 proto 增长时编译不破,不提供任何
// 真实行为。
//
// 并发:agentService 的可变状态(nodeID、pending)由 mu 保护。nodeID 在
// Register 时写一次、之后只读;pending 在每次 ApplyIntent 前后成对变更。
// 其余依赖(cfg / certInfo / snapshot)都在构造期设定、之后不再改;
// snapshot.Detector 自己的缓存另有互斥保护。
type agentService struct {
	agentv1.UnimplementedAgentServiceServer

	mu sync.Mutex // 保护 nodeID(首次 Register 时写入一次)与 pending

	// cfg is the daemon's runtime config (agent_id, cert
	// paths, etc.). The Register handler reads agent_id and
	// PolicyPath from here: PolicyPath is read from disk to
	// compute the response's policy_hash. The cert paths in cfg
	// are consumed by main() to construct the TLS config.
	cfg *config.Config

	// certInfo carries the cert's NotAfter + SHA-256
	// fingerprint for the Register response. nil is acceptable
	// for tests that don't care about the cert metadata; the
	// Register handler emits CertValidUntilUnix=0 and an empty
	// fingerprint in that case (the proto permits both).
	certInfo *cert.Info

	// snapshot is the agent's snapshot-capability probe.
	// Register reads it; ApplyIntent consults it to pick the
	// rollback backend. SnapshotRequest no longer uses it (it
	// carries no target path, so it refuses unconditionally).
	snapshot *snapshot.Detector

	// auditSeq is the per-process monotonic audit sequence
	// counter. ApplyIntent (via the tx state machine) bumps it
	// on each tx outcome (success / transient / persistent).
	// nil is permitted only in tests that don't reach
	// ApplyIntent; the constructor seeds a private counter
	// otherwise.
	auditSeq *atomic.Uint64

	// audit is the per-process hash-chained audit log (T7).
	// ReportStatus streams events from it; tx.Local writes
	// success / failure events to it on every ApplyIntent
	// outcome. nil is permitted for tests that don't
	// exercise ReportStatus / ApplyIntent; the
	// report_status handler then returns an empty response
	// (the documented "no audit yet" wire shape).
	audit *audit.Local

	// backend is an optional override for the per-RPC backend
	// selection. If non-nil, it is used as-is by ApplyIntent
	// (skipping SelectBackend). Production leaves this nil;
	// tests inject a mock backend so the handler exercises the
	// wired-up state machine without touching the host.
	backend rollback.Backend

	// txFactory 非 nil 时用来替代 tx.New 构造每次 ApplyIntent 的事务;
	// nil 表示走生产默认(tx.New + internal/exec 的真实执行器)。
	txFactory TxFactory

	// rollbackTokens 签发 / 校验回滚的 approve token。nil 表示回滚通道未
	// 装配(dataDir 不可用时),RollbackIntent 一律 REFUSED。
	rollbackTokens *rollback.TokenManager

	// rollbackRunner 执行回滚步骤。接口化是为了让 PLAN 用例能断言"一条
	// 命令都没执行"。
	rollbackRunner RollbackRunner

	// nodeID is the control-plane-assigned identifier for
	// this agent. It is empty until the first successful
	// Register, then stable for the daemon's lifetime. The
	// Heartbeat handler rejects requests whose node_id does
	// not match — preventing a stale client from driving an
	// old agent registration.
	nodeID string

	// toolCaller 是节点侧只读工具调用面。nil 表示未配置:ListTools
	// 返回空工具集,CallTool 一律 FailedPrecondition。
	toolCaller *toolcall.Caller

	// pending 按 intent_id 记"已受理但未完成"的 ApplyIntent 次数。
	// Heartbeat 把它的总和回报成 pending_intents,让控制面看到真实的
	// 在途工作量,而不是硬编码的 0。由 mu 保护。
	//
	// 用引用计数而不是集合:控制面把同一 intent_id 并发投递两次时,先结束
	// 的那次不能把仍在跑的那次一起抹掉(否则计数会少报)。
	pending map[string]int

	// startedAt is the wall-clock the service was constructed.
	// It is informational only in T5 (a future health-check
	// RPC could surface uptime); not yet surfaced.
	startedAt time.Time
}

// 编译期断言:*agentService 满足 agentv1.AgentServiceServer。
//
// 它**不**能发现 proto 新增方法:内嵌的 UnimplementedAgentServiceServer
// 已经满足新方法,编译照常通过,节点在运行时对未实现的方法静默回
// codes.Unimplemented。新增 RPC 必须自己补 handler 与测试 —— 这里只
// 兜「接口本身被改签名」这一类编译期错误。
var _ agentv1.AgentServiceServer = (*agentService)(nil)

// RollbackRunner 执行一份回滚计划。
//
// 接口化(而不是直接用 *rollback.StepRunner)是为了让测试能注入一个只
// 记账、不碰宿主的实现 —— PLAN 用例要断言"一步都没执行"。
type RollbackRunner interface {
	Run(ctx context.Context, steps []rollback.Step) error
}

// rollbackKeyFile 是 approve token 的 HMAC key 的落盘名(在 dataDir 下)。
const rollbackKeyFile = "rollback_token.key"

// loadOrCreateRollbackKey 读 <dataDir>/rollback_token.key;不存在就生成
// 32 字节随机并以 0600 落盘。
//
// 权限必须是 0600:这把 key 是回滚通道的根 —— 拿到它就能给自己签 approve
// token。
func loadOrCreateRollbackKey(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, rollbackKeyFile)
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(b) == 0 {
			return nil, fmt.Errorf("rpc: rollback token key %s is empty", path)
		}
		return b, nil
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("rpc: read rollback token key %s: %w", path, err)
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("rpc: generate rollback token key: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("rpc: mkdir %s: %w", dataDir, err)
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("rpc: write rollback token key %s: %w", path, err)
	}
	return key, nil
}

// initRollbackTokens 装配回滚通道的 token 管理器。
//
// dataDir 为空时只把通道标成未启用(handler 一律 REFUSED),不拒绝启动:生产
// 配置校验已经要求 data_dir,走到这里的空值只可能来自绕过校验的测试。key 文件
// 的读写失败仍然让启动失败 —— 那是真实的 I/O 故障,悄悄降级成"永远 REFUSED"
// 的回滚通道比启动报错更难被发现。
func (s *agentService) initRollbackTokens() error {
	if s.cfg == nil || s.cfg.DataDir == "" {
		return nil
	}
	key, err := loadOrCreateRollbackKey(s.cfg.DataDir)
	if err != nil {
		return err
	}
	s.rollbackTokens = rollback.NewTokenManager(key)
	return nil
}

// newAgentService 用注入的依赖构造 agentService。main() 与测试的 rig
// 都走这里;所有可选依赖都用零值表达「未配置」。
//
// 参数语义:
//   - auditSeq 由事务在每次 ApplyIntent 结局时推进;nil 时分配私有计数器。
//   - backend 非 nil 时覆盖 ApplyIntent 的回滚后端选择(测试注入用)。
//   - txFactory 非 nil 时替代 tx.New 构造事务;生产传 nil 走真实执行器。
//   - auditLog 是进程级哈希链;nil 时 ReportStatus 返回空、事务不落链。
//   - toolCaller 是只读工具调用面;nil 表示节点没有远程面。
//
// startedAt 在构造时记下,uptime 从构造开始算(与实际监听只差 bind 时间)。
func newAgentService(cfg *config.Config, certInfo *cert.Info, det *snapshot.Detector, auditSeq *atomic.Uint64, backend rollback.Backend, txFactory TxFactory, auditLog *audit.Local, toolCaller *toolcall.Caller) *agentService {
	if auditSeq == nil {
		auditSeq = new(atomic.Uint64)
	}
	return &agentService{
		cfg:            cfg,
		certInfo:       certInfo,
		snapshot:       det,
		auditSeq:       auditSeq,
		backend:        backend,
		txFactory:      txFactory,
		audit:          auditLog,
		toolCaller:     toolCaller,
		rollbackRunner: rollback.NewStepRunner(),
		pending:        map[string]int{},
		startedAt:      time.Now().UTC(),
	}
}

// beginIntent / endIntent / pendingIntentCount 维护"已受理但未完成"的
// intent 计数。ApplyIntent 在事务前后成对调用,因此这个数反映的是**在途**
// 工作量,不是历史累计。
func (s *agentService) beginIntent(intentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		s.pending = map[string]int{}
	}
	s.pending[intentID]++
}

func (s *agentService) endIntent(intentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.pending[intentID]
	if !ok {
		return
	}
	if n <= 1 {
		delete(s.pending, intentID)
		return
	}
	s.pending[intentID] = n - 1
}

func (s *agentService) pendingIntentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, n := range s.pending {
		total += n
	}
	return total
}

// selectedBackend returns the rollback backend for the
// current RPC invocation. If a test-injected backend is set,
// it is returned as-is (the detector is bypassed). Otherwise
// the agent selects via rollback.SelectBackend based on the
// live detector capability.
//
// 回滚点形态由调用方传入(PointForIntent 已在 handler 里算过),宿主能力才
// 从 detector 取:PointFile / PointTree 恒走内建后端,能力只在 PointVolume
// 时参与选择。tree 后端要靠 cfg.DataDir 定位快照集合,所以走带数据目录的
// SelectBackendFor。
func (s *agentService) selectedBackend(point rollback.Point) (rollback.Backend, error) {
	if s.backend != nil {
		return s.backend, nil
	}
	return rollback.SelectBackendFor(point, s.snapshot.Capability(), s.cfg.DataDir)
}

// buildTx 构造每次 ApplyIntent 的事务:测试注入的 txFactory 优先,否则走
// 生产默认(tx.New + internal/exec 分派器)。传入的 backend 与 auditSeq
// 原样转发给事务。
//
// opts 只对生产默认路径生效:txFactory 是测试替身,替身要不要支持目录
// 快照由测试自己决定。
//
// 同时转发进程级哈希链审计日志(与 report_status.go 共用):
// tx.Local 在每次 ApplyIntent 结局往链上写 success / failure 事件。
func (s *agentService) buildTx(backend rollback.Backend, opts ...tx.Option) tx.Tx {
	if s.txFactory != nil {
		return s.txFactory(backend, s.cfg.DataDir, s.auditSeq, s.audit)
	}
	return tx.New(backend, exec.ApplyWithHandle, s.cfg.DataDir, s.auditSeq, s.audit, opts...)
}
