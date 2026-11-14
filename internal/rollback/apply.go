package rollback

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Daedalusys/daedalus-sdk/pkgquery"

	"github.com/Daedalusys/daedalus-agent/internal/exec"
)

// 执行阶段的拒绝原因。分哨兵是为了让控制面把"要人审"与"命令本身不许"
// 分开呈现给审查者。
var (
	// ErrRollbackInFlight 表示同一个句柄已经有一个回滚在跑。
	ErrRollbackInFlight = errors.New("rollback: another rollback is already running for this handle")
	// ErrDestructiveNeedsReview 表示销毁性步骤没标人审。
	ErrDestructiveNeedsReview = errors.New("rollback: destructive step must be marked human-review")
	// ErrPathNotOwned 表示步骤要动的路径不归这个包所有。
	ErrPathNotOwned = errors.New("rollback: step touches a path the package does not own")
	// ErrOwnershipUnverifiable 表示这个删除步没法证明路径归属(句柄不指向
	// 可查询的包、或包管理器清单查不到)。拿不到证明就不删。
	ErrOwnershipUnverifiable = errors.New("rollback: cannot prove the step's paths are package-owned")
	// ErrStepFailed 表示某一步执行失败(已执行的步骤不自动回退)。
	ErrStepFailed = errors.New("rollback: step failed")
)

// commandWhitelist 是生产命令白名单:只认回滚语法里的那几条命令形状。
//
// 白名单按 argv 判而不是按字符串前缀判:前缀匹配会把
// "dnf history undo 42; rm -rf /" 这种拼接串放进来。
type commandWhitelist struct{}

// NewCommandAllower 返回生产命令白名单。计划生成与步骤执行用的是同一份
// 形状判定 —— 两边各写一套迟早会漂移。
func NewCommandAllower() CommandAllower { return commandWhitelist{} }

// defaultCommandAllower 是 StepRunner 执行时用的白名单。
var defaultCommandAllower CommandAllower = commandWhitelist{}

func (commandWhitelist) Allow(cmd string) bool {
	argv := strings.Fields(cmd)
	if len(argv) == 0 {
		return false
	}
	switch argv[0] {
	case "dnf":
		// dnf history undo <id>
		return len(argv) == 4 && argv[1] == "history" && argv[2] == "undo" && isDecimal(argv[3])
	case "apt":
		// apt history-undo <id>
		return len(argv) == 3 && argv[1] == "history-undo" && isDecimal(argv[2])
	case "apt-get":
		// apt-get remove|purge -y <pkg>...
		if len(argv) < 3 || (argv[1] != "remove" && argv[1] != "purge") {
			return false
		}
		for _, a := range argv[2:] {
			if a == "-y" || a == "--yes" {
				continue
			}
			if strings.HasPrefix(a, "-") || !pkgquery.IsValidPackageName(a) {
				return false
			}
		}
		return true
	case "systemctl":
		if len(argv) != 3 || !exec.ValidUnitName(argv[2]) {
			return false
		}
		switch argv[1] {
		case "start", "stop", "enable", "disable":
			return true
		}
		return false
	case "rm":
		// 只认 rm -rf <绝对路径>...:相对路径相对的是 agent 的 CWD,不受控。
		if len(argv) < 3 || argv[1] != "-rf" {
			return false
		}
		for _, p := range argv[2:] {
			if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, " \t") {
				return false
			}
		}
		return true
	}
	return false
}

// isDecimal 报告 s 是否是十进制非负整数(事务 id / 历史 id 的形状)。
func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// StepRunner 执行一份回滚计划。
//
// 零值可用;sync.Map 不能复制,所以对外只给 *StepRunner。
type StepRunner struct {
	// inflight 按 handle 加并发锁:同一个句柄同时只允许一个 APPLY。
	// 第二个直接拒(而不是排队)—— 排队意味着控制面以为两件事都在进行。
	inflight sync.Map
}

// NewStepRunner 构造一个执行器。
func NewStepRunner() *StepRunner { return &StepRunner{} }

// Run 校验并执行步骤:先整体校验,再逐条执行;失败即停。
//
// 为什么先整体校验:一份混进"未标人审的 rm -rf"的计划不该先跑完前两步再
// 被拒 —— 那会把宿主留在"回滚了一半"的状态。
//
// MANUAL 步不执行:它是给人看的清单项(配置残留、postinst 副作用),agent
// 不替人做,也不假装做了。
func (r *StepRunner) Run(ctx context.Context, steps []Step) error {
	if len(steps) == 0 {
		return fmt.Errorf("%w: empty plan", ErrPlanUnsupported)
	}
	handle := steps[0].Handle
	if handle == "" {
		return fmt.Errorf("%w: step carries no rollback handle", ErrPlanUnsupported)
	}
	for i, s := range steps {
		// 一份计划只能属于一个句柄:混着句柄意味着并发锁锁不住整份计划。
		if s.Handle != handle {
			return fmt.Errorf("%w: step %d belongs to another handle", ErrPlanUnsupported, i)
		}
		if err := ValidateStep(s); err != nil {
			return fmt.Errorf("rollback: step %d: %w", i, err)
		}
	}

	if _, loaded := r.inflight.LoadOrStore(handle, struct{}{}); loaded {
		return fmt.Errorf("%w: %q", ErrRollbackInFlight, handle)
	}
	defer r.inflight.Delete(handle)

	for i, s := range steps {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("rollback: cancelled before step %d: %w", i, err)
		}
		if s.Kind == StepManual {
			continue
		}
		argv := strings.Fields(s.Command)
		if err := runCommand(argv[0], argv[1:]...); err != nil {
			return fmt.Errorf("%w: step %d (%s): %v", ErrStepFailed, i, s.Command, err)
		}
	}
	return nil
}

// ValidateStep 校验一条步骤能不能执行。
//
// 导出它是因为 RPC 层在 PLAN 阶段要校验 admin 改过的步骤:白名单、人审
// 标记、路径归属必须与执行阶段是同一套判定。
//
// 路径归属不看 s.OwnedPaths(它不进 APPLY 请求):rm 步一律按句柄重查包管理
// 器的文件清单,查不到就拒绝。
func ValidateStep(s Step) error {
	if s.Kind == StepManual {
		// MANUAL 只给文字:带命令就成了绕过白名单的后门。
		if s.Command != "" {
			return fmt.Errorf("%w: manual step carries a command", ErrCommandNotAllowed)
		}
		if !s.HumanReview {
			return fmt.Errorf("%w: manual step must be marked human-review", ErrDestructiveNeedsReview)
		}
		return nil
	}
	if s.Command == "" {
		return fmt.Errorf("%w: executable step has no command", ErrCommandNotAllowed)
	}
	if !defaultCommandAllower.Allow(s.Command) {
		return fmt.Errorf("%w: %q", ErrCommandNotAllowed, s.Command)
	}

	argv := strings.Fields(s.Command)
	destructive := argv[0] == "rm" || (argv[0] == "apt-get" && len(argv) > 1 && argv[1] == "purge")
	if destructive && !s.HumanReview {
		return fmt.Errorf("%w: %q", ErrDestructiveNeedsReview, s.Command)
	}
	if argv[0] == "rm" {
		// rm 只归 FILE_DELETE 步,且路径必须 ⊆ **按句柄重查**的包拥有清单:
		// 清单不上 wire,请求方没法声明"这些路径归我"。
		if s.Kind != StepFileDelete {
			return fmt.Errorf("%w: rm step must be kind FILE_DELETE", ErrCommandNotAllowed)
		}
		if err := verifyStepOwnership(s); err != nil {
			return err
		}
	}
	return nil
}

// VerifyOwnership 复核计划里所有删除步的路径归属。
//
// 导出给 RPC 层在消耗 token 之前先跑一遍:拿不到清单时,一份本来有效的
// token 不该被白白烧掉。真正的执行判定仍在 ValidateStep(它每步都会重查,
// 调用方绕不过去)。
func VerifyOwnership(steps []Step) error {
	for i, s := range steps {
		if err := verifyStepOwnership(s); err != nil {
			return fmt.Errorf("step %d: %w", i, err)
		}
	}
	return nil
}

// verifyStepOwnership 按句柄重查包的文件记录,复核 rm 要删的路径。
//
// 判据只有"完全相等":目标必须同时是 dpkg 记的 conffile、又精确出现在
// dpkg-query -L 的清单里。不做前缀匹配 —— 清单里有 /etc 目录,按前缀放行
// 会把 /etc/passwd 也算成"包拥有"。
//
// 为什么必须重查:APPLY 请求里的步骤只带 kind/命令/说明/人审标记,拥有清单
// 不上 wire(它进不了 plan_hash)。所以执行期的证明只能从**包管理器自己的
// 数据库**重新取 —— 句柄是包管理器给的事实,不是请求方能编辑的字段。
func verifyStepOwnership(s Step) error {
	argv := strings.Fields(s.Command)
	if len(argv) < 3 || argv[0] != "rm" {
		// 白名单会先拒掉非 "rm -rf <路径>..." 的形状;这里只处理有路径可查的。
		return nil
	}
	provable, err := provableConffilesForHandle(s.Handle)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOwnershipUnverifiable, err)
	}
	for _, p := range argv[2:] {
		if _, ok := provable[p]; !ok {
			return fmt.Errorf("%w: %q", ErrPathNotOwned, p)
		}
	}
	return nil
}

// provableConffilesForHandle 从句柄重查"能证明归属"的配置文件路径集合。
//
// 只有 apt 句柄有这条可证明路径(dpkg 的 conffile 记录 + 文件清单互相印证)。
// 别的句柄知道自己撤的是哪次变更,却说不出"那个包拥有哪些配置文件" ——
// 没有证明就不删,fail-closed。
func provableConffilesForHandle(handle string) (map[string]struct{}, error) {
	kind, id, err := exec.ParseHandle(handle)
	if err != nil {
		return nil, err
	}
	if kind != exec.HandleAptHistory {
		return nil, fmt.Errorf("handle %q has no provable file list", handle)
	}
	pkg, err := aptHistoryPackage(id)
	if err != nil {
		return nil, err
	}
	if err := validatePackageName(pkg); err != nil {
		return nil, err
	}
	owned, err := dpkgOwnedPaths(pkg)
	if err != nil {
		return nil, err
	}
	conffiles, err := dpkgConffiles(pkg)
	if err != nil {
		return nil, err
	}
	deletable, _ := provableDeletePaths(conffiles, owned)
	provable := make(map[string]struct{}, len(deletable))
	for _, p := range deletable {
		provable[p] = struct{}{}
	}
	return provable, nil
}
