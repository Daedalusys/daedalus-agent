// Package exec 是 daedalus-agent 的 intent 执行器:把控制面下发的
// ApplyIntent 真正落到宿主上。
//
// 定位:internal/tx 的事务状态机在 pre-snapshot 与回滚之间要调一个
// apply 步骤;本包的 Apply 就是那个步骤的生产实现(替换早先只打日志
// 却回报 SUCCESS 的 applyStub)。分派按 intent.Kind 查表,命中则交给
// 对应执行器,未命中一律拒绝。
//
// 三条硬约束(计划 Global Constraints):
//
//   - fail-closed:无法判定的 kind 不得猜。查不到执行器即返回
//     ErrUnsupportedKind,绝不回 SUCCESS。
//   - 尊重 ctx.Done():执行器里所有外部命令都经 runCmd(ctx, ...) 起,
//     ctx 取消即中止;半成品不得当成功。
//   - 不旁路宿主:package.set 调宿主包管理器(apt-get / dnf),
//     file.set 走宿主文件系统,service.set 走 systemctl。
//
// 错误分类决定控制面行为:ErrTransient 会被自动重试(上限 3 次),
// ErrPersistent 不会。因此"包名不存在"必须 Persistent,"锁被占/网络
// 抖动"才是 Transient。判定不清楚时按 Persistent(不可重试)处理。
package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// 错误哨兵。执行器用 %w 包装底层原因,调用方(tx / rpc)用 errors.Is 判定
// 可重试性;tx 会据此选择 TRANSIENT_FAILURE / PERSISTENT_FAILURE。
var (
	// ErrTransient 表示失败可能是暂时的(包管理器锁被占、网络抖动),
	// 控制面可以自动重试。
	ErrTransient = errors.New("exec: transient failure")
	// ErrPersistent 表示失败不是重试能解决的(包/单元不存在、依赖冲突、
	// 权限不足),控制面不得自动重试。
	ErrPersistent = errors.New("exec: persistent failure")
	// ErrUnsupportedKind 表示该 kind 没有注册执行器。fail-closed:
	// 不猜、不静默成功。
	ErrUnsupportedKind = errors.New("exec: unsupported intent kind")
)

// Executor 是单一 kind 的执行器。实现必须尊重 ctx.Done(),并且幂等:
// 同一 intent 重复投递(控制面无重试上限的边界)不得产生副作用叠加。
type Executor interface {
	Apply(ctx context.Context, intent *agentv1.ApplyIntentRequest) error
}

// HandleProvider 是可选接口:执行器实现它,即可在 apply 之后回传回滚句柄。
//
// 可选而非必备:file 的回滚点是内建 copy,不产句柄;没实现的 kind 一律
// 空句柄,控制面对高危变更据此拒绝(fail-closed)。
//
// 实现可能在 Apply 期间就把素材攒好了(service 的前置状态只能在 apply 前
// 读),ApplyWithHandle 用同一个 ctx 把素材递过来。
type HandleProvider interface {
	RollbackHandle(ctx context.Context, intent *agentv1.ApplyIntentRequest) (string, error)
}

// ApplyFunc 与 tx.ApplyFunc 同签名,便于直接作为 tx.New 的 apply 步骤传入。
type ApplyFunc func(ctx context.Context, intent *agentv1.ApplyIntentRequest) error

// registry 保存 kind → 执行器的映射。写入只发生在启动期的 registerAll
// (经 registerOnce)与测试的显式 Register;读取发生在每次 Apply。用
// RWMutex 是因为 gRPC 服务端会并发处理多个 ApplyIntent。
var (
	registryMu sync.RWMutex
	registry   = map[string]Executor{}
	// registerOnce 保证 registerAll 只跑一次。生产上 Apply 是唯一入口,
	// 首次调用即完成注册;这样调用方不需要记得先做初始化。
	registerOnce sync.Once
)

// Register 登记一个执行器。kind 为空或重复注册会 panic —— 这是编程错误
// (注册表是启动期构建的静态结构),早失败比运行到一个谁也不认识的 kind
// 更安全。测试也可用它注入 fake。
func Register(kind string, e Executor) {
	if kind == "" {
		panic("exec: Register with empty kind")
	}
	if e == nil {
		panic("exec: Register with nil executor for kind " + kind)
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[kind]; dup {
		panic("exec: duplicate executor registration for kind " + kind)
	}
	registry[kind] = e
}

// registerAll 登记所有生产执行器。在 init() 与显式函数之间选了**显式**:
// 注册顺序确定、可测、无 init 顺序陷阱。
//
// 键是**资源 kind**(objectmodel.Kind):控制面把 Intent.Kind 原样发上
// wire,`package.set` / `service.set` 只是 core 风险判定表内部的适配器名,
// 不会出现在这里。`package` / `service` / `file` 各对应一个执行器;未登记的
// kind 由 Apply 统一按 ErrUnsupportedKind 拒绝(fail-closed)。
//
// 各执行器文件各自往这里追加一行注册。
func registerAll() {
	Register(kindPackage, packageExecutor{})
	Register(kindService, serviceExecutor{})
	Register(kindFile, fileExecutor{})
}

// lookup 返回 kind 对应的执行器。第二个返回值为 false 表示未注册。
func lookup(kind string) (Executor, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	e, ok := registry[kind]
	return e, ok
}

// Apply 是 intent 的分派入口(只要结局,不要句柄)。句柄版见 ApplyWithHandle
// —— 生产路径(tx)用的是后者;这个包装留给只需要错误的地方与测试。
func Apply(ctx context.Context, intent *agentv1.ApplyIntentRequest) error {
	_, err := ApplyWithHandle(ctx, intent)
	return err
}

// ApplyWithHandle 是 Apply 的加长版:除了 apply 的结局,还回传可持久引用的
// 回滚句柄。
//
// 句柄捕获失败**不改变 apply 的结局**:变更已经落在宿主上了,把它报成失败
// 只会让控制面去重试一个已经生效的变更。失败时返回空句柄 + nil,由上层按
// "没有回滚锚点"处理(写链 + 高危拒绝)。
func ApplyWithHandle(ctx context.Context, intent *agentv1.ApplyIntentRequest) (string, error) {
	if intent == nil {
		return "", fmt.Errorf("%w: nil intent", ErrUnsupportedKind)
	}
	registerOnce.Do(registerAll)
	e, ok := lookup(intent.Kind)
	if !ok {
		// 不猜:控制面发了本 agent 不认识的 kind,重试也不会变好。
		return "", fmt.Errorf("%w: %q", ErrUnsupportedKind, intent.Kind)
	}

	// 收集器必须在 Apply 之前挂上:service 的前置状态只能在那时读到。
	ctx = withCollector(ctx, newHandleCollector())

	if err := e.Apply(ctx, intent); err != nil {
		return "", classifyExecError(err)
	}

	hp, ok := e.(HandleProvider)
	if !ok {
		return "", nil
	}
	handle, err := hp.RollbackHandle(ctx, intent)
	if err != nil {
		log.Printf("agent: exec: rollback handle unavailable: kind=%q target=%q err=%v",
			intent.Kind, intent.Target, err)
		return "", nil
	}
	return handle, nil
}

// classifyExecError 给执行器错误兜底分类。执行器契约:失败必须可分类,否则
// 控制面不知道要不要重试。漏裹哨兵的实现一律按不可重试处理 —— 这里在
// **分派边界**兜底,免得依赖上层(如 tx)的默认分流恰好选对方向。
func classifyExecError(err error) error {
	if classifyable(err) {
		return err
	}
	return fmt.Errorf("%w: executor returned an unclassified error: %v", ErrPersistent, err)
}

// HandleCapable 报告该 kind 是否可能产出回滚句柄。
//
// 控制面与审计据此区分「空句柄是异常」与「这个 kind 本来就没有句柄」。
func HandleCapable(kind string) bool {
	return kind == kindPackage || kind == kindService
}

// classifyable 报告 err 是否已经带上可重试性(或本身就是 ctx 取消,由上层
// 按取消语义处理)。
func classifyable(err error) bool {
	return errors.Is(err, ErrTransient) ||
		errors.Is(err, ErrPersistent) ||
		errors.Is(err, ErrUnsupportedKind) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// cmdRunner 是所有执行器调用外部命令的唯一注入缝。返回合并后的
// stdout+stderr 与退出错误:执行器的错误分类要看管理器输出里的字句,
// 故必须拿到输出。测试替换它,以免起真实进程或碰真实宿主。
type cmdRunner func(ctx context.Context, name string, args ...string) (string, error)

// runCmd 是 production 实现:exec.CommandContext 保证 ctx 取消时进程被
// 杀,命令因此天然尊重 ctx.Done()。
var runCmd cmdRunner = func(ctx context.Context, name string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, name, args...)
	var buf bytes.Buffer
	c.Stdout = &buf
	c.Stderr = &buf
	err := c.Run()
	return buf.String(), err
}

// fileExists 是宿主能力探测的注入缝(os.Stat 语义)。测试替换它来模造
// "宿主上有 apt-get / dnf / systemctl / snapper" 等条件。
var fileExists = func(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
