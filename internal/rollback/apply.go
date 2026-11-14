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
		if err := validateStep(s); err != nil {
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

// validateStep 校验一条步骤能不能执行。
func validateStep(s Step) error {
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
		// rm 只归 FILE_DELETE 步,且路径必须 ⊆ 计划里声明的包拥有清单。
		if s.Kind != StepFileDelete {
			return fmt.Errorf("%w: rm step must be kind FILE_DELETE", ErrCommandNotAllowed)
		}
		for _, p := range argv[2:] {
			if !pathOwned(s.OwnedPaths, p) {
				return fmt.Errorf("%w: %q", ErrPathNotOwned, p)
			}
		}
	}
	return nil
}

// pathOwned 报告 p 是否落在拥有清单里(或其目录之下)。
func pathOwned(owned []string, p string) bool {
	for _, o := range owned {
		if o == "" {
			continue
		}
		if p == o {
			return true
		}
		if strings.HasPrefix(p, strings.TrimSuffix(o, "/")+"/") {
			return true
		}
	}
	return false
}
