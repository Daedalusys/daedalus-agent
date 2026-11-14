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
	// OwnedPaths 是计划生成时刻包管理器声明的文件清单,写在计划里供人审。
	//
	// 执行期**不读它**:APPLY 请求里没有这个字段(它进不了 plan_hash),读它
	// 等于让请求方自己声明"这些路径归我"。删除路径的归属按 Handle 重查,
	// 见 VerifyOwnership。
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

// apt 版本不够(没有原生 undo)→ 退化计划:卸载 → 删残留 conffiles → 人工核对。
//
// 为什么两步而不是直接 purge:purge 是包管理器自己的一次销毁动作,人审时看不到
// 它到底会删哪些路径。拆成 remove + rm conffiles 之后,删除目标就在计划里
// 逐条列出,且每条都能被 dpkg 的文件清单证明。
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
	// 两条只读查询就是"凭什么删"的全部证据:包声明拥有的文件清单 + dpkg 记为
	// 配置文件的那些路径。任何一条查不到都整份计划失败 —— 只出一半证据的计划
	// 会让人在"看起来完整"的步骤上点同意。
	owned, err := dpkgOwnedPaths(pkg)
	if err != nil {
		return Plan{}, fmt.Errorf("%w: %v", ErrPlanUnsupported, err)
	}
	conffiles, err := dpkgConffiles(pkg)
	if err != nil {
		return Plan{}, fmt.Errorf("%w: %v", ErrPlanUnsupported, err)
	}

	removeCmd := "apt-get remove -y " + pkg
	if err := allowCommand(allower, removeCmd); err != nil {
		return Plan{}, err
	}
	steps := []Step{{
		Kind:        StepPackageRemove,
		Command:     removeCmd,
		Rationale:   "宿主 apt 不支持原生 undo:卸载 " + pkg + " 以撤销本次安装。注意:不会还原被它替换的旧版本",
		HumanReview: true,
		Handle:      handle,
	}}

	// 只删 conffiles:apt-get remove 已经删掉非配置文件,残留下来的正是它们;
	// 删别的就是删 dpkg 没声明拥有的东西。
	deletable, unprovable := provableDeletePaths(conffiles, owned)
	if len(deletable) > 0 {
		rmCmd := "rm -rf " + strings.Join(deletable, " ")
		if err := allowCommand(allower, rmCmd); err != nil {
			return Plan{}, err
		}
		steps = append(steps, Step{
			Kind:        StepFileDelete,
			Command:     rmCmd,
			Rationale:   "删除 " + pkg + " 的残留配置文件:apt-get remove 保留 dpkg 记为 conffile 的路径,回滚要把它们一起删干净",
			HumanReview: true,
			Handle:      handle,
			Paths:       deletable,
			OwnedPaths:  owned,
		})
	}

	manual := "人工核对 " + pkg + " 的残留副作用:postinst/preinst 可能建过用户、组、服务或生成过文件,这些不在 dpkg 的文件清单里,无法证明,仍需人工核对"
	if len(unprovable) > 0 {
		manual += ";另有 " + strconv.Itoa(len(unprovable)) + " 个配置文件路径无法证明归属或无法用 argv 安全表达,未自动删除: " + strings.Join(unprovable, ", ")
	}
	steps = append(steps, Step{
		Kind:        StepManual,
		Rationale:   manual,
		HumanReview: true,
		Handle:      handle,
	})

	return Plan{Handle: handle, Steps: steps}, nil
}

// dpkgOwnedPaths 查 dpkg 里该包**声明拥有**的完整路径清单。
//
// 这是"凭什么删"的唯一证明来源:-L 列的是包自己的文件清单,不是"磁盘上
// 恰好有这些路径"。查不到就报错,不用空清单冒充"这个包没有文件"。
func dpkgOwnedPaths(pkg string) ([]string, error) {
	out, err := runCommandOutput("dpkg-query", "-L", pkg)
	if err != nil {
		return nil, fmt.Errorf("dpkg-query -L %s: %w", pkg, err)
	}
	return parsePathList(string(out)), nil
}

// dpkgConffiles 查 dpkg 记为配置文件(conffile)的路径。
func dpkgConffiles(pkg string) ([]string, error) {
	out, err := runCommandOutput("dpkg-query", "-W", "-f=${Conffiles}\\n", pkg)
	if err != nil {
		return nil, fmt.Errorf("dpkg-query -W conffiles %s: %w", pkg, err)
	}
	return parseConffiles(string(out))
}

// parsePathList 把 dpkg 的逐行清单变成路径切片。
func parsePathList(out string) []string {
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		paths = append(paths, unescapeDpkgPath(t))
	}
	return paths
}

// parseConffiles 解析 dpkg 的 ${Conffiles} 输出。
//
// 真机形状(dpkg 1.21,cat -A 核过)是:
//
//	<路径> <md5|newconffile> [obsolete|remove-on-upgrade]
//
// 前导空格是控制文件续行的格式;第二段是 .deb 里的 md5(该文件不在 .deb 里
// 时是字面量 newconffile),第三段才是 dpkg 的记账状态。把 md5 当状态词、
// 或只认两个旧状态词,都会让整条计划在真机上失败(第一版就是这样)。
//
// 路径里的空白等特殊字符 dpkg 已转义成 \ddd,所以按空白切词是安全的。
// 切不出这个形状的行一律报错:拿前缀当路径去 rm,删掉的可能是别的包的文件。
func parseConffiles(out string) ([]string, error) {
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		fields := strings.Fields(t)
		if len(fields) < 2 || len(fields) > 3 {
			return nil, fmt.Errorf("%w: conffile line %q is not <path> <md5> [flag]", ErrPlanUnsupported, t)
		}
		if !isConffileChecksum(fields[1]) {
			return nil, fmt.Errorf("%w: conffile line %q has no md5sum", ErrPlanUnsupported, t)
		}
		if len(fields) == 3 && !isConffileStatus(fields[2]) {
			return nil, fmt.Errorf("%w: conffile line %q has an unknown flag", ErrPlanUnsupported, t)
		}
		if p := unescapeDpkgPath(fields[0]); p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// isConffileChecksum 报告第二段是否是 dpkg 记的校验和:32 位小写十六进制,
// 或 "newconffile"(该文件不在 .deb 里,由维护者脚本生成)。
func isConffileChecksum(s string) bool {
	if s == "newconffile" {
		return true
	}
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isConffileStatus 报告 dpkg 记在 conffile 记录后的状态词。
func isConffileStatus(s string) bool {
	return s == "obsolete" || s == "remove-on-upgrade"
}

// unescapeDpkgPath 还原 dpkg 清单里 \ddd(三位八进制)形式的转义。
//
// dpkg 用这套编码把空白、反斜杠等写进逐行清单(\040 是空格,\134 是反斜杠);
// 不还原的话,删除路径与拥有清单会各自带一身转义,永远对不上,白名单与归属
// 校验都会误判。
func unescapeDpkgPath(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+3 < len(s) &&
			isOctDigit(s[i+1]) && isOctDigit(s[i+2]) && isOctDigit(s[i+3]) {
			v := int(s[i+1]-'0')*64 + int(s[i+2]-'0')*8 + int(s[i+3]-'0')
			if v <= 0xFF {
				b.WriteByte(byte(v))
				i += 4
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// isOctDigit 报告 c 是否是八进制数字(dpkg 的 \ddd 转义只认 0-7)。
func isOctDigit(c byte) bool { return c >= '0' && c <= '7' }

// provableDeletePaths 从 conffiles 里挑出"能证明归属、也能安全写进 rm argv"
// 的路径。
//
// 归属判定只有一种:路径必须**完全等于** dpkg 记为 conffile 的某条记录,并且
// 精确出现在 dpkg-query -L 的清单里 —— 两份 dpkg 记录互相印证。不做前缀
// 匹配,也不认目录:dpkg-query -L 本来就会列出 /etc、/usr 这些目录,按目录
// 前缀放行等于把"按包清理"变成"按任意目录清理"。
//
// 被挑掉的路径进第二个返回值:它们没法自动删,但必须让人看见,而不是静悄悄
// 留在磁盘上假装删过了。
func provableDeletePaths(conffiles, owned []string) (deletable, skipped []string) {
	ownedSet := make(map[string]struct{}, len(owned))
	for _, o := range owned {
		ownedSet[o] = struct{}{}
	}
	seen := make(map[string]struct{}, len(conffiles))
	for _, p := range conffiles {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		if _, ok := ownedSet[p]; !ok {
			skipped = append(skipped, p)
			continue
		}
		// rm 白名单只认绝对路径、且路径里不能有空白:表达不了的路径不能硬塞,
		// 否则整条删除步会被白名单拒掉。
		if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, " \t\n") {
			skipped = append(skipped, p)
			continue
		}
		deletable = append(deletable, p)
	}
	return deletable, skipped
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
