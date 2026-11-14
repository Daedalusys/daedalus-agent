package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Daedalusys/daedalus-sdk/pkgquery"
	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// package.set 的 kind 字符串。上线的是**资源 kind**(objectmodel.Kind
// 的 "package"),不是 core 风险判定表内部用的适配器名 "package.set":
// core 的调度器发的是 string(Intent.Kind),适配器名不上 wire。
const kindPackage = "package"

// package.set 的 desired_state 冻结三值。表外值一律拒绝(见
// packageDesiredState),与 core 侧 daedalus-tx 的 packageSetVerbs 同集。
const (
	statePresent = "present"
	stateAbsent  = "absent"
	stateLatest  = "latest"
)

// hostManager 描述宿主的包管理器。
type hostManager struct {
	name string // "apt" | "dnf"
	bin  string // /usr/bin/apt-get | /usr/bin/dnf
}

// detectHostManager 按宿主既有的包管理器选路:apt-get 优先,其次 dnf,
// 都没有则不可重试地拒绝 —— 不猜、不在 Ubuntu 上模拟 dnf(计划
// Global Constraints:不旁路宿主)。
func detectHostManager() (hostManager, error) {
	if fileExists("/usr/bin/apt-get") {
		return hostManager{name: "apt", bin: "/usr/bin/apt-get"}, nil
	}
	if fileExists("/usr/bin/dnf") {
		return hostManager{name: "dnf", bin: "/usr/bin/dnf"}, nil
	}
	return hostManager{}, fmt.Errorf("%w: no host package manager found (need /usr/bin/apt-get or /usr/bin/dnf)", ErrPersistent)
}

// packageSpec 是 package.set 的 SpecJson schema。Version 是控制面 spec 里
// 已有的键(常带版本诉求),显式声明以免被 DisallowUnknownFields 连坐拒掉。
//
// 口径 C:Version 不是安装指令 —— 它不换源、不降级、不改 apt/dnf 的安装
// 参数;它只是"我要的那个版本"的声明,apply 之后拿实际版本对一下(见
// verifyPackageVersion)。
type packageSpec struct {
	DesiredState string `json:"desired_state"`
	Version      string `json:"version"`
}

// parsePackageSpec 解析并校验 spec_json,同时给出 desired_state 与声明版本。
//
// 未声明 desired_state 时按 present 处理:v1 的冻结三值里 present 是 package
// 意图的自然默认("把这个包装上"),而控制面 Plan 层的 Intent.Target 已带包名、
// Spec 可只给 version。显式写了表外值则拒绝 —— 那才是"无法判定"。
//
// 未知键一律拒:拼错的 desired_stat 若被忽略,会静默落进上面的 present
// 默认 —— 一个本意"卸载"的意图被翻成"安装",且没有任何报错。这是全部分支
// 里唯一朝更危险方向默认的一条,必须堵死。
func parsePackageSpec(specJSON string) (packageSpec, error) {
	raw := strings.TrimSpace(specJSON)
	if raw == "" || raw == "null" {
		return packageSpec{DesiredState: statePresent}, nil
	}
	var spec packageSpec
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return packageSpec{}, fmt.Errorf("%w: invalid package.set spec_json: %v", ErrPersistent, err)
	}
	switch spec.DesiredState {
	case "":
		spec.DesiredState = statePresent
	case statePresent, stateAbsent, stateLatest:
	default:
		return packageSpec{}, fmt.Errorf("%w: unknown desired_state %q (v1 supports present|absent|latest)", ErrPersistent, spec.DesiredState)
	}
	return spec, nil
}

// sanitizePackageName 是包名门的唯一出口。字符集走 SDK 的
// pkgquery.IsValidPackageName(单一事实源,绝不内联白名单 regex 副本),
// 再补三条 post-check:字符类允许 '.',光靠它会放过 "a..b" 路径回溯与
// ".hidden" 隐藏文件语义;'/' 与 '\' 显式钉住路径分隔符;'*' 在 set 域
// 会变成批量变更(dnf/apt 的 glob 展开),违背 v1 单段精确语义。
// 错误串与 core 侧 daedalus-tx 的 sanitizePackageName 逐字一致。
func sanitizePackageName(name string) error {
	if !pkgquery.IsValidPackageName(name) {
		return fmt.Errorf("invalid package name: %q", name)
	}
	if strings.Contains(name, "..") || strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("invalid package name: %q", name)
	}
	if strings.Contains(name, "*") {
		return fmt.Errorf("invalid package name: %q", name)
	}
	return nil
}

// packageExecutor 实现 package.set。无状态,可复用。
type packageExecutor struct{}

// Apply 把 package.set 落到宿主包管理器上。
//
// 幂等:先观测当前是否已装,已到位即 no-op。这样同一 intent 重复投递
// (控制面无重试上限的边界)不会叠加副作用。
//
// 版本声明(口径 C)在 apply 之后核对,no-op 路径同样核对:同一 intent
// 重复投递因此结论恒定 —— 不会第一次报版本不符、第二次因为"已装"就翻成
// 成功。
func (packageExecutor) Apply(ctx context.Context, intent *agentv1.ApplyIntentRequest) error {
	name := intent.Target
	if err := sanitizePackageName(name); err != nil {
		return fmt.Errorf("%w: %v", ErrPersistent, err)
	}
	spec, err := parsePackageSpec(intent.SpecJson)
	if err != nil {
		return err
	}
	mgr, err := detectHostManager()
	if err != nil {
		return err
	}

	installed, err := pkgInstalled(ctx, mgr, name)
	if err != nil {
		return err
	}

	var verb string
	switch spec.DesiredState {
	case statePresent:
		if !installed {
			verb = "install"
		}
	case stateLatest:
		// 已装则升级,未装则装到最新(apt 的 install 本身就取最新;
		// dnf 的 upgrade 对未装包会报错,故分流)。
		if installed {
			verb = "upgrade"
		} else {
			verb = "install"
		}
	case stateAbsent:
		if !installed {
			return nil
		}
		verb = "remove"
	default:
		// 防御性兜底:parsePackageSpec 已拒过表外值,这里不可达。
		return fmt.Errorf("%w: unknown desired_state %q", ErrPersistent, spec.DesiredState)
	}

	if verb != "" {
		if err := runPkgVerb(ctx, mgr, verb, name); err != nil {
			return err
		}
		if col := collectorFrom(ctx); col != nil {
			col.markChanged()
		}
	}

	// apply 已经落盘(或本次无需变更):此时才谈得上"实际版本是多少"。
	// 校验失败不回滚、不降级 —— 回滚由人按控制面提示执行。
	return verifyPackageVersion(ctx, mgr, name, spec.DesiredState, spec.Version)
}

// RollbackHandle 找本次 apply 在包管理器里留下的事务 id。
//
// 只在**本次真的改了宿主**时才找:no-op 的 apply 没有事务,给句柄等于让
// 控制面日后去回滚一次从未发生的变更(典型后果是把本来就在跑的包卸掉)。
func (packageExecutor) RollbackHandle(ctx context.Context, intent *agentv1.ApplyIntentRequest) (string, error) {
	col := collectorFrom(ctx)
	if col == nil || !col.wasChanged() {
		return "", nil
	}
	mgr, err := detectHostManager()
	if err != nil {
		return "", err
	}
	switch mgr.name {
	case "dnf":
		return captureDnfHandle(ctx, intent.Target, col.since())
	case "apt":
		return captureAptHandle(ctx, intent.Target, col.since())
	}
	return "", fmt.Errorf("%w: unsupported host manager %q", ErrPersistent, mgr.name)
}

// pkgInstalled 观测包是否已装。apt 用 dpkg-query(退出码 1 表示未装),
// dnf 用 rpm -q。观测失败(非"未装")如实上抛,绝不谎报现状。
//
// 已知取舍:dpkg 下"已卸载但留有配置文件"(deinstall ok config-files)不算
// 已装 —— desired_state=absent 因此不会去清理残留配置。这是有意的:clean
// 会删掉运维可能仍要保留的配置,超出"卸载"的语义。
func pkgInstalled(ctx context.Context, mgr hostManager, name string) (bool, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, fmt.Errorf("exec: package.set %q cancelled: %w", name, ctxErr)
	}
	switch mgr.name {
	case "apt":
		out, err := runCmd(ctx, "dpkg-query", "-W", "-f=${Status}", name)
		if err != nil {
			// dpkg-query 对未安装的包以非零退出并提示 no packages found。
			if strings.Contains(out, "no packages found") || strings.Contains(out, "is not installed") {
				return false, nil
			}
			return false, classifyManagerError("dpkg-query", out, err)
		}
		return strings.Contains(out, "install ok installed"), nil
	case "dnf":
		out, err := runCmd(ctx, "rpm", "-q", name)
		if err != nil {
			if strings.Contains(out, "is not installed") || strings.Contains(out, "not installed") {
				return false, nil
			}
			return false, classifyManagerError("rpm", out, err)
		}
		return strings.TrimSpace(out) != "", nil
	}
	return false, fmt.Errorf("%w: unsupported host manager %q", ErrPersistent, mgr.name)
}

// pkgInstalledVersion 取已装包的实际版本。
//
// 命令形状刻意用查询工具的原生形态(与 pkgInstalled 同一个 runCmd 缝):
//
//   - apt:dpkg-query -W -f=${Version} <pkg> —— 只要版本字段本身,
//     不要整段 Status,免得多一层解析;
//   - dnf:rpm -q --qf %{VERSION}-%{RELEASE} <pkg> —— 与 dpkg 的
//     "上游版本-修订号"形状对齐,便于同一条截断规则处理。
//
// 查询失败一律 ErrPersistent:查不到实际版本时既不能说相符,也不能说
// 不符 —— 唯一诚实的做法是把"我不知道"报成不可重试的失败,让人来看。
// 包在 apply 之后突然"未安装"同属此类(并发卸载的竞态),同样如实上抛。
func pkgInstalledVersion(ctx context.Context, mgr hostManager, name string) (string, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", fmt.Errorf("exec: package.set %q cancelled: %w", name, ctxErr)
	}

	var out string
	var err error
	switch mgr.name {
	case "apt":
		out, err = runCmd(ctx, "dpkg-query", "-W", "-f=${Version}", name)
	case "dnf":
		out, err = runCmd(ctx, "rpm", "-q", "--qf", "%{VERSION}-%{RELEASE}", name)
	default:
		return "", fmt.Errorf("%w: unsupported host manager %q", ErrPersistent, mgr.name)
	}
	if err != nil {
		low := strings.ToLower(out)
		if strings.Contains(low, "no packages found") || strings.Contains(low, "is not installed") || strings.Contains(low, "not installed") {
			return "", fmt.Errorf("%w: version check for %q: package is not installed after apply", ErrPersistent, name)
		}
		return "", fmt.Errorf("%w: version check for %q: cannot determine installed version: %s (%v)", ErrPersistent, name, firstLine(out), err)
	}

	version := strings.TrimSpace(out)
	if version == "" {
		return "", fmt.Errorf("%w: version check for %q: cannot determine installed version: empty output", ErrPersistent, name)
	}
	return version, nil
}

// versionTrunk 把包管理器报出的实际版本裁成可比的"主干"。
//
// 为什么这样裁:
//
//   - 先去掉 epoch(第一个 ':' 之前的部分)。epoch 是包管理器内部的排序权重
//     (典型如 1:2.4.0),不是版本语义的一部分;把 1:2.4.0 当成"版本号"
//     去和 2.4.0 比,只会得到假的不符。
//   - 再在第一个 '-' / '+' / '~' 处截断。其后的部分是发行版/构建方的
//     修订信息:apt 的 -2ubuntu7.19、-9+deb12u10,rpm 的 -1.el9;它们
//     跨发行版不可比,拼进比较里会让"同一上游版本"在 Debian 与 Ubuntu
//     上得出不同结论。
//   - 截断后逐字相等才算相符:不做 >/>= 的"够新即可"判断 —— 口径 C 只
//     回答"装的是不是声明的那个版本",替用户判断"够不够新"会再次把
//     装错版本报成成功。也不做跨发行版归一:归一规则一旦猜错,结论就反了。
func versionTrunk(actual string) string {
	if i := strings.IndexByte(actual, ':'); i >= 0 {
		actual = actual[i+1:]
	}
	if i := strings.IndexAny(actual, "-+~"); i >= 0 {
		actual = actual[:i]
	}
	return strings.TrimSpace(actual)
}

// verifyPackageVersion 在 apply 之后核对实际版本(口径 C)。
//
// 口径 C 的边界:
//
//   - version 不是安装指令:不换源、不降级、不改 apt/dnf 的安装参数。
//   - version 为空/缺失 → 不校验,行为与历史完全一致。
//   - desired_state=absent → 不校验:包本来就不该留在宿主上,谈不上"装的是
//     哪个版本"。
//   - present/latest → 只要包确已安装就校验,包括本次 no-op 的情形。这样
//     同一 intent 重复投递结论恒定,不会第一次失败、第二次成功。
//   - 不符不回滚、不降级、不自动重试:报 ErrPersistent,由人按控制面提示
//     决定回滚或换源。
func verifyPackageVersion(ctx context.Context, mgr hostManager, name, state, declared string) error {
	declared = strings.TrimSpace(declared)
	if declared == "" || state == stateAbsent {
		return nil
	}

	actual, err := pkgInstalledVersion(ctx, mgr, name)
	if err != nil {
		return err
	}
	if versionTrunk(actual) == declared {
		return nil
	}
	return fmt.Errorf("%w: version mismatch: declared=%s actual=%s", ErrPersistent, declared, actual)
}

// runPkgVerb 发一条变更命令。argv 用宿主管理器的原生形态:
// apt → apt-get <install|remove> -y / --only-upgrade;dnf → dnf <install|remove|upgrade> -y。
func runPkgVerb(ctx context.Context, mgr hostManager, verb, name string) error {
	var argv []string
	switch mgr.name {
	case "apt":
		switch verb {
		case "install":
			argv = []string{"install", "-y", name}
		case "remove":
			argv = []string{"remove", "-y", name}
		case "upgrade":
			argv = []string{"install", "-y", "--only-upgrade", name}
		default:
			return fmt.Errorf("%w: unsupported apt verb %q", ErrPersistent, verb)
		}
	case "dnf":
		argv = []string{verb, "-y", name}
	default:
		return fmt.Errorf("%w: unsupported host manager %q", ErrPersistent, mgr.name)
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		// 命令还没起就被取消:半成品为零,直接上抛取消原因。
		return fmt.Errorf("exec: package.set %s %q cancelled: %w", verb, name, ctxErr)
	}
	out, err := runCmd(ctx, mgr.bin, argv...)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("exec: package.set %s %q cancelled: %w", verb, name, ctxErr)
		}
		return classifyManagerError(mgr.name, out, err)
	}
	return nil
}

// classifyManagerError 把包管理器输出归类成可重试性,决定控制面会不会
// 自动重试(TRANSIENT)还是就此停下(PERSISTENT)。
//
// 判据不明时按 PERSISTENT 处理:自动重试高危操作比"多留一个人工确认"
// 代价更大,故默认不可重试。
func classifyManagerError(source, output string, err error) error {
	low := strings.ToLower(output)

	// 锁被占:另一个包管理器进程正在跑。等一会儿就好 → 可重试。
	for _, p := range []string{
		"could not get lock",
		"unable to acquire the dpkg frontend lock",
		"is held by process",
		"failed to obtain the transaction lock",
		"another app is currently holding the yum lock",
		"waiting for process with pid",
	} {
		if strings.Contains(low, p) {
			return fmt.Errorf("%w: %s: lock held: %s", ErrTransient, source, firstLine(output))
		}
	}

	// 网络抖动:DNS / 连接层面失败 → 可重试。
	for _, p := range []string{
		"temporary failure resolving",
		"could not resolve",
		"connection timed out",
		"connection failed",
		"network is unreachable",
		"temporary failure in name resolution",
	} {
		if strings.Contains(low, p) {
			return fmt.Errorf("%w: %s: network: %s", ErrTransient, source, firstLine(output))
		}
	}

	// 包不存在 → 重试无益。
	for _, p := range []string{
		"unable to locate package",
		"has no installation candidate",
		"no match for argument",
		"unable to find a match",
		"no packages found",
	} {
		if strings.Contains(low, p) {
			return fmt.Errorf("%w: %s: package not found: %s", ErrPersistent, source, firstLine(output))
		}
	}

	// 依赖冲突 → 重试无益。
	for _, p := range []string{
		"unmet dependencies",
		"nothing provides",
		"conflicts with",
		"cannot install",
		"error: problem",
	} {
		if strings.Contains(low, p) {
			return fmt.Errorf("%w: %s: dependency conflict: %s", ErrPersistent, source, firstLine(output))
		}
	}

	return fmt.Errorf("%w: %s: unclassified failure: %s (%v)", ErrPersistent, source, firstLine(output), err)
}

// firstLine 取输出的第一行非空内容,避免把整段管理器日志塞进错误串。
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}
