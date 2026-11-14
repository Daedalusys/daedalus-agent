package rollback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Daedalusys/daedalus-sdk/pkgquery"

	"github.com/Daedalusys/daedalus-agent/internal/exec"
)

// 计划生成失败的原因。分哨兵是为了让控制面把"这个句柄没救"与"命令不在
// 白名单"分开展示 —— 前者要人重新装一遍,后者要人改允许集。
var (
	// ErrPlanUnsupported 表示这个句柄无法生成可执行计划(fail-closed,不猜)。
	ErrPlanUnsupported = errors.New("rollback: no executable plan for this handle")
	// ErrCommandNotAllowed 表示计划里的命令不在允许集内。
	ErrCommandNotAllowed = errors.New("rollback: command not allowed")
)

// StepKind 是回滚步骤的类型。
type StepKind int

const (
	// StepUnspecified 是零值,不该出现在可执行计划里。
	StepUnspecified StepKind = iota
	// StepNativeUndo 包管理器原生撤销(整事务反向执行)。
	StepNativeUndo
	// StepPackageRemove 卸载包(保留配置文件)。
	StepPackageRemove
	// StepFileDelete 删除包拥有的文件;永远需要人审。
	StepFileDelete
	// StepManual 只给人看的说明步,不带命令。
	StepManual
)

// Step 是一个回滚步骤。
type Step struct {
	Kind        StepKind
	Command     string
	Rationale   string
	HumanReview bool
	// Handle 是本步归属的回滚句柄。Run 按它加并发锁(同一次回滚同时只允许
	// 一个执行者),所以它随步骤走而不是只在 Plan 上。
	Handle string
	// Paths 是本步命令会触及的路径(StepFileDelete 用)。
	Paths []string
	// OwnedPaths 是计划生成时刻包管理器声明的文件清单。删除路径必须 ⊆ 它:
	// 句柄是包管理器给的,不该有能力删掉包以外的文件。
	OwnedPaths []string
}

// Plan 是一次回滚的不可变描述。
type Plan struct {
	Handle string
	Steps  []Step
	// AutoOK 表示控制面可以在无人值守时直接 APPLY(只有包管理器原生撤销
	// 才给这个待遇)。
	AutoOK bool
}

// Hash 是计划的稳定身份(token 绑定它,admin 改了命令就得重新 PLAN)。
//
// 只覆盖**上了 wire 的字段**(句柄 + 每步的 kind/命令/说明/人审标记):
// APPLY 请求里只有这些,agent 要靠它们从请求本身重算出同一个哈希。内部
// 字段(OwnedPaths 等)不参与 —— 它们不是控制面能改的东西,而且不进
// 请求就无法在 APPLY 时重算。
//
// 字段按长度前缀编码:命令/说明里出现分隔符也不会让两份不同计划撞出同一
// 个哈希。
func (p Plan) Hash() string {
	h := sha256.New()
	writeHashField(h, "handle", p.Handle)
	for _, s := range p.Steps {
		writeHashField(h, "kind", strconv.Itoa(int(s.Kind)))
		writeHashField(h, "cmd", s.Command)
		writeHashField(h, "why", s.Rationale)
		writeHashField(h, "review", strconv.FormatBool(s.HumanReview))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeHashField 按 "key:长度:值" 写一个字段,消除拼接歧义。
func writeHashField(w io.Writer, key, val string) {
	fmt.Fprintf(w, "%s:%d:%s\n", key, len(val), val)
}

// CommandAllower 判定一条命令是否在允许集内。
type CommandAllower interface {
	Allow(cmd string) bool
}

// BuildPlan 按句柄生成回滚计划。
//
// 三条硬边界:
//   - 认不出的句柄 / 拿不到包管理器事实 → 报错(fail-closed,不猜);
//   - 每条命令都要过 allower,白名单为空也报错;
//   - 计划生成阶段**不执行任何命令**,只读查询(dnf/apt 的历史、版本)。
//
// ctx 目前不外传:包内的命令缝(runCommandOutput)是既有形状,不带 ctx。
func BuildPlan(ctx context.Context, handle string, allower CommandAllower) (Plan, error) {
	kind, id, err := exec.ParseHandle(handle)
	if err != nil {
		return Plan{}, fmt.Errorf("%w: %v", ErrPlanUnsupported, err)
	}
	switch kind {
	case exec.HandleDnfHistory:
		return buildDnfPlan(handle, id, allower)
	case exec.HandleAptHistory:
		return buildAptPlan(ctx, handle, id, allower)
	case exec.HandleService:
		return buildServicePlan(handle, id, allower)
	}
	return Plan{}, fmt.Errorf("%w: unknown handle kind %q", ErrPlanUnsupported, kind)
}

// buildDnfPlan:dnf 有原生事务撤销,单步、自动可执行。
//
// 已知取舍:这里不再复核该事务是否仍是"最新相关事务"。dnf 在 undo 时会自己
// 报冲突,而冲突会被 APPLY 的退出码检查判成 FAILED —— 不会静默成功。
func buildDnfPlan(handle, id string, allower CommandAllower) (Plan, error) {
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return Plan{}, fmt.Errorf("%w: dnf transaction id %q is not a number", ErrPlanUnsupported, id)
	}
	cmd := "dnf history undo " + id
	if err := allowCommand(allower, cmd); err != nil {
		return Plan{}, err
	}
	return Plan{
		Handle: handle,
		Steps: []Step{{
			Kind:      StepNativeUndo,
			Command:   cmd,
			Rationale: "dnf 原生撤销事务 " + id + ":按事务把本次变更整体反向执行;失败即冲突,不重试",
			Handle:    handle,
		}},
		AutoOK: true,
	}, nil
}

// buildAptPlan:apt ≥ 3.2 走原生撤销;更老的 apt 退化成"卸载 + 人工核对"。
func buildAptPlan(ctx context.Context, handle, id string, allower CommandAllower) (Plan, error) {
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return Plan{}, fmt.Errorf("%w: apt history id %q is not a number", ErrPlanUnsupported, id)
	}
	native, err := aptSupportsUndo()
	if err != nil {
		return Plan{}, err
	}
	if native {
		cmd := aptUndoCommand(id)
		if err := allowCommand(allower, cmd); err != nil {
			return Plan{}, err
		}
		return Plan{
			Handle: handle,
			Steps: []Step{{
				Kind:      StepNativeUndo,
				Command:   cmd,
				Rationale: "apt 原生撤销历史条目 " + id + ";失败即冲突,不重试",
				Handle:    handle,
			}},
			AutoOK: true,
		}, nil
	}

	// 老 apt 没有原生 undo:只能卸载,而且卸载不还原旧版本。这条路径必须人审。
	pkg, err := aptHistoryPackage(id)
	if err != nil {
		return Plan{}, err
	}
	if err := validatePackageName(pkg); err != nil {
		return Plan{}, err
	}
	removeCmd := "apt-get remove -y " + pkg
	if err := allowCommand(allower, removeCmd); err != nil {
		return Plan{}, err
	}
	return Plan{
		Handle: handle,
		Steps: []Step{
			{
				Kind:        StepPackageRemove,
				Command:     removeCmd,
				Rationale:   "宿主 apt 不支持原生 undo:卸载 " + pkg + " 以撤销本次安装。注意:不会还原被它替换的旧版本",
				HumanReview: true,
				Handle:      handle,
			},
			{
				Kind:        StepManual,
				Rationale:   "人工核对 " + pkg + " 的残留:purge 会删配置文件,postinst 可能改过用户/服务/生成文件;这些都不自动处理",
				HumanReview: true,
				Handle:      handle,
			},
		},
	}, nil
}

// buildServicePlan:service 的"回滚"就是把单元恢复到 apply 之前的状态。
//
// 两个方向都给:恢复前置状态对没动过的维度是幂等的(start 已 active 的单元
// 是 no-op),因此不会误改我们没碰过的东西。
func buildServicePlan(handle, id string, allower CommandAllower) (Plan, error) {
	st, err := exec.DecodeServiceState(id)
	if err != nil {
		return Plan{}, fmt.Errorf("%w: %v", ErrPlanUnsupported, err)
	}
	if !exec.ValidUnitName(st.Unit) {
		return Plan{}, fmt.Errorf("%w: invalid systemd unit name %q", ErrPlanUnsupported, st.Unit)
	}

	activeCmd := "systemctl stop " + st.Unit
	if st.ActiveState == "active" {
		activeCmd = "systemctl start " + st.Unit
	}
	fileCmd := "systemctl disable " + st.Unit
	if st.UnitFileState == "enabled" {
		fileCmd = "systemctl enable " + st.Unit
	}
	for _, cmd := range []string{activeCmd, fileCmd} {
		if err := allowCommand(allower, cmd); err != nil {
			return Plan{}, err
		}
	}
	return Plan{
		Handle: handle,
		Steps: []Step{
			{
				Kind:      StepNativeUndo,
				Command:   activeCmd,
				Rationale: "把 " + st.Unit + " 的运行状态恢复成 apply 之前的 " + orUnknown(st.ActiveState),
				Handle:    handle,
			},
			{
				Kind:      StepNativeUndo,
				Command:   fileCmd,
				Rationale: "把 " + st.Unit + " 的开机自启状态恢复成 apply 之前的 " + orUnknown(st.UnitFileState),
				Handle:    handle,
			},
		},
	}, nil
}

// allowCommand 让每条命令都过白名单。allower 为 nil 视为装配错误:没有
// 白名单就执行命令,等于把"命令由控制面给定"这件事交给运气。
func allowCommand(allower CommandAllower, cmd string) error {
	if allower == nil {
		return fmt.Errorf("%w: no command allowlist configured", ErrCommandNotAllowed)
	}
	if !allower.Allow(cmd) {
		return fmt.Errorf("%w: %q", ErrCommandNotAllowed, cmd)
	}
	return nil
}

// aptUndoCommand 是 apt ≥ 3.2 原生撤销的命令形状。
//
// 收在一个函数里:apt 各版本的子命令命名并不统一,真机核实后只改这里,
// 计划生成与命令白名单都不用动。
func aptUndoCommand(id string) string {
	return "apt history-undo " + id
}

// aptSupportsUndo 报告宿主的 apt 是否自带历史撤销(≥ 3.2)。
//
// 判不出来就报错而不是"当作老版本走退化路径":退化路径会卸载包,猜错方向
// 的代价比拒绝一次回滚大。
func aptSupportsUndo() (bool, error) {
	out, err := runCommandOutput("apt-get", "--version")
	if err != nil {
		return false, fmt.Errorf("rollback: apt-get --version: %w", err)
	}
	major, minor, ok := parseAptVersion(string(out))
	if !ok {
		return false, fmt.Errorf("%w: cannot parse apt version from %q", ErrPlanUnsupported, firstNonEmptyLine(string(out)))
	}
	return major > 3 || (major == 3 && minor >= 2), nil
}

// aptHistoryPackage 从 apt 历史里反查这次变更动的是哪个包。
//
// 句柄只带历史 id(格式钉死在 apt-history:<id>),包名要在计划期查:老 apt
// 的退化计划必须点名卸载谁,而"猜一个包"等于删错东西。
func aptHistoryPackage(id string) (string, error) {
	out, err := runCommandOutput("apt", "history-info", id)
	if err != nil {
		return "", fmt.Errorf("rollback: apt history-info %s: %w", id, err)
	}
	return packageFromAptCommandline(string(out))
}

// aptVerbs 是 apt 历史命令行里的动词,不是包名。
var aptVerbs = map[string]struct{}{
	"install": {}, "remove": {}, "purge": {}, "upgrade": {}, "dist-upgrade": {},
	"full-upgrade": {}, "autoremove": {}, "reinstall": {}, "downgrade": {},
}

// packageFromAptCommandline 从 apt history-info 的 "Commandline:" 行取出
// 唯一的包名。多于一个包时报错:句柄没说是哪一个,猜不得。
func packageFromAptCommandline(out string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-"))
		rest, ok := strings.CutPrefix(t, "Commandline:")
		if !ok {
			continue
		}
		var pkgs []string
		for _, f := range strings.Fields(rest) {
			f = strings.TrimSuffix(f, ",")
			if f == "" || f == "apt" || f == "apt-get" || strings.HasPrefix(f, "-") {
				continue
			}
			if _, isVerb := aptVerbs[f]; isVerb {
				continue
			}
			name := f
			if i := strings.IndexAny(name, "=:"); i > 0 {
				name = name[:i]
			}
			if name == "" {
				continue
			}
			pkgs = append(pkgs, name)
		}
		switch len(pkgs) {
		case 1:
			return pkgs[0], nil
		case 0:
			continue
		default:
			return "", fmt.Errorf("%w: apt history entry touches %d packages; cannot pick one", ErrPlanUnsupported, len(pkgs))
		}
	}
	return "", fmt.Errorf("%w: apt history-info has no usable command line", ErrPlanUnsupported)
}

// parseAptVersion 从 "apt 2.4.11 (amd64)" 里取 major.minor。
func parseAptVersion(out string) (major, minor int, ok bool) {
	for _, line := range strings.Split(out, "\n") {
		for _, f := range strings.Fields(line) {
			parts := strings.SplitN(f, ".", 3)
			if len(parts) < 2 {
				continue
			}
			maj, err1 := strconv.Atoi(parts[0])
			min, err2 := strconv.Atoi(parts[1])
			if err1 == nil && err2 == nil {
				return maj, min, true
			}
		}
	}
	return 0, 0, false
}

// validatePackageName 挡住从宿主历史里读回来的怪名字:真正的防线是
// argv 执行(没有 shell),这里再拦一次前导 '-' 与非法字符,免得把命令行
// 选项当成包名传下去。
func validatePackageName(name string) error {
	if !pkgquery.IsValidPackageName(name) || strings.HasPrefix(name, "-") {
		return fmt.Errorf("%w: invalid package name %q", ErrPlanUnsupported, name)
	}
	return nil
}

// orUnknown 给空状态一个可读的占位,别在计划里留下 "恢复成 " 这种断句。
func orUnknown(s string) string {
	if s == "" {
		return "(未知)"
	}
	return s
}

// firstNonEmptyLine 取第一行非空内容,避免把整段输出塞进错误串。
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}
