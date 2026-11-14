package rollback

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Daedalusys/daedalus-agent/internal/exec"
)

// fakeCommands 回放命令的标准输出/错误,让计划生成不必真跑宿主命令。
type fakeCommands struct {
	out   map[string]string
	err   map[string]error
	calls []string
}

func newFakeCommands() *fakeCommands {
	return &fakeCommands{out: map[string]string{}, err: map[string]error{}}
}

func (f *fakeCommands) run(name string, args ...string) ([]byte, error) {
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.calls = append(f.calls, key)
	if err, ok := f.err[key]; ok {
		return []byte(f.out[key]), err
	}
	return []byte(f.out[key]), nil
}

// withFakeCommands 换掉命令执行缝。
func withFakeCommands(t *testing.T, f *fakeCommands) {
	t.Helper()
	prev := runCommandOutput
	t.Cleanup(func() { runCommandOutput = prev })
	runCommandOutput = f.run
}

// allowAll 是放行一切的命令白名单(测试用)。
type allowAll struct{}

func (allowAll) Allow(string) bool { return true }

// denyAll 拒绝一切的命令白名单(测试用)。
type denyAll struct{}

func (denyAll) Allow(string) bool { return false }

// dnf 句柄 → 单步原生撤销,自动可执行。
func TestPlan_DnfHistoryUndo(t *testing.T) {
	withFakeCommands(t, newFakeCommands())

	p, err := BuildPlan(context.Background(), "dnf-history:42", allowAll{})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p.Handle != "dnf-history:42" {
		t.Errorf("Handle = %q, want dnf-history:42", p.Handle)
	}
	if len(p.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(p.Steps))
	}
	step := p.Steps[0]
	if step.Kind != StepNativeUndo {
		t.Errorf("Kind = %v, want StepNativeUndo", step.Kind)
	}
	if step.Command != "dnf history undo 42" {
		t.Errorf("Command = %q, want %q", step.Command, "dnf history undo 42")
	}
	if step.HumanReview {
		t.Error("dnf 原生撤销不该要人审")
	}
	if !p.AutoOK {
		t.Error("dnf 原生撤销应当可自动执行")
	}
}

// 真机 dpkg 1.21 的 conffile 记录形状(经 cat -A 核过):
// 前导空格 + "<路径> <md5>[ obsolete| remove-on-upgrade]"。md5 是 .deb 里的
// 校验和,文件不在 .deb 里时是字面量 newconffile。
const (
	conffileMD5Nginx   = "89269e1298235f1b12b4c16e4065ad0d"
	conffileMD5Default = "ee35a240758f374832e809ae0ea4883a"
)

// 真机样本:Debian 13 / apt 3.0.4 装 nginx 时 /var/log/apt/history.log 的形状
// (与 debian13-historylog.txt 的 hello 条目同形)。apt 3.0.4 的
// `apt history-list` 是 exit 100,日志是唯一可用的锚点。
const aptLogNginx = `
Start-Date: 2026-11-14  11:28:45
Commandline: apt-get install -y nginx
Install: nginx:amd64 (1.24.0-1)
End-Date: 2026-11-14  11:28:45
`

// aptLogHandle 用样本日志里提到 pkg 的条目拼出句柄(与捕获侧同构)。
func aptLogHandle(logText, pkg string) string {
	for _, e := range exec.ParseAptHistoryLog(logText) {
		for _, p := range e.Packages {
			if p == pkg {
				return "apt-log:" + strconv.FormatInt(e.StartDate.Unix(), 10) + ":" + pkg
			}
		}
	}
	panic("sample log has no entry for " + pkg)
}

var aptNginxHandle = aptLogHandle(aptLogNginx, "nginx")

// withAptLog 把"读 apt 历史日志"的缝换成样本内容。
func withAptLog(t *testing.T, text string) {
	t.Helper()
	entries := exec.ParseAptHistoryLog(text)
	prev := readAptHistoryLog
	t.Cleanup(func() { readAptHistoryLog = prev })
	readAptHistoryLog = func() ([]exec.AptLogEntry, error) { return entries, nil }
}

// withAptLogError 模拟日志读不了(不存在 / 权限)。
func withAptLogError(t *testing.T, err error) {
	t.Helper()
	prev := readAptHistoryLog
	t.Cleanup(func() { readAptHistoryLog = prev })
	readAptHistoryLog = func() ([]exec.AptLogEntry, error) { return nil, err }
}

// aptFallbackFake 伪造一台"老 apt(无原生 undo)+ nginx 已装"的宿主。
//
// dpkg-query -L 的输出**包含目录**(/.、/etc、/usr...),这是真机形状,也是
// 归属校验不能按目录前缀放行的原因。
func aptFallbackFake() *fakeCommands {
	fc := newFakeCommands()
	fc.out["apt-get --version"] = "apt 3.0.4 (amd64)\n"
	fc.out["dpkg-query -L nginx"] = strings.Join([]string{
		"/.",
		"/etc",
		"/etc/nginx",
		"/etc/nginx/nginx.conf",
		"/etc/nginx/sites-available",
		"/etc/nginx/sites-available/default",
		"/usr",
		"/usr/sbin",
		"/usr/sbin/nginx",
	}, "\n") + "\n"
	fc.out["dpkg-query -W -f=${Conffiles}\\n nginx"] = strings.Join([]string{
		" /etc/nginx/nginx.conf " + conffileMD5Nginx,
		" /etc/nginx/sites-available/default " + conffileMD5Default + " obsolete",
	}, "\n") + "\n"
	return fc
}

// apt 版本不够(没有原生 undo)→ 退化计划:卸载 → 删残留 conffiles → 人工核对。
//
// 删除步不是"顺手删点什么":它的路径来自 dpkg 的 conffile 记录,且必须落在
// dpkg-query -L 声明的清单内 —— 计划自己就把"凭什么删这些"写在了一起。
func TestPlan_AptFallbackNeedsReview(t *testing.T) {
	fc := aptFallbackFake()
	withFakeCommands(t, fc)
	withAptLog(t, aptLogNginx)

	p, err := BuildPlan(context.Background(), aptNginxHandle, allowAll{})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	// apt<3.2 没有 history 子命令:退化路径不得去调它(调了必失败)。
	for _, c := range fc.calls {
		if strings.Contains(c, "history-info") {
			t.Errorf("apt<3.2 的退化路径不该调 %q", c)
		}
	}
	if p.AutoOK {
		t.Error("apt 退化路径不该自动执行")
	}
	wantKinds := []StepKind{StepPackageRemove, StepFileDelete, StepManual}
	if len(p.Steps) != len(wantKinds) {
		t.Fatalf("steps = %d, want %d(卸载 → 删 conffiles → 人工核对):%+v",
			len(p.Steps), len(wantKinds), p.Steps)
	}
	for i, want := range wantKinds {
		if p.Steps[i].Kind != want {
			t.Fatalf("step %d Kind = %v, want %v", i, p.Steps[i].Kind, want)
		}
	}

	remove := p.Steps[0]
	if remove.Command != "apt-get remove -y nginx" {
		t.Errorf("remove.Command = %q, want %q", remove.Command, "apt-get remove -y nginx")
	}
	if !remove.HumanReview {
		t.Error("apt 退化路径必须人审")
	}

	// 只删 conffiles:apt-get remove 已经删掉非配置文件,残留的正是它们。
	del := p.Steps[1]
	wantPaths := []string{"/etc/nginx/nginx.conf", "/etc/nginx/sites-available/default"}
	if !slices.Equal(del.Paths, wantPaths) {
		t.Errorf("delete.Paths = %v, want %v", del.Paths, wantPaths)
	}
	if want := "rm -rf /etc/nginx/nginx.conf /etc/nginx/sites-available/default"; del.Command != want {
		t.Errorf("delete.Command = %q, want %q", del.Command, want)
	}
	if !del.HumanReview {
		t.Error("rm -rf 永远人审,没有例外")
	}
	// 计划里要内联 dpkg 声明的完整清单,而不是只有删除目标。
	for _, want := range []string{"/etc/nginx/nginx.conf", "/usr/sbin/nginx"} {
		if !slices.Contains(del.OwnedPaths, want) {
			t.Errorf("delete.OwnedPaths 缺 %q: %v", want, del.OwnedPaths)
		}
	}

	// postinst/preinst 的副作用不在 dpkg 清单里,只能给人看。
	manual := p.Steps[2]
	if manual.Command != "" {
		t.Errorf("MANUAL 步不该带命令, got %q", manual.Command)
	}
	if !manual.HumanReview {
		t.Error("MANUAL 步必须人审")
	}
	if !strings.Contains(manual.Rationale, "postinst") || !strings.Contains(manual.Rationale, "无法证明") {
		t.Errorf("MANUAL 说明 = %q, want 点明 postinst 副作用无法证明", manual.Rationale)
	}
}

// 计划里的每一步都要能过执行期的校验:两边用的是同一份 dpkg 清单。
func TestPlan_AptFallbackStepsPassValidation(t *testing.T) {
	withFakeCommands(t, aptFallbackFake())
	withAptLog(t, aptLogNginx)

	p, err := BuildPlan(context.Background(), aptNginxHandle, allowAll{})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	for i, s := range p.Steps {
		if err := ValidateStep(s); err != nil {
			t.Fatalf("ValidateStep(step %d %v) = %v, want nil", i, s.Kind, err)
		}
	}
}

// dpkg 把清单里的特殊字符写成 \ddd(八进制):解析时必须还原,否则删除路径
// 与拥有清单对不上。
func TestPlan_AptFallbackUnescapesConffiles(t *testing.T) {
	fc := aptFallbackFake()
	// \134 是反斜杠;它没有空白,rm 的 argv 形状仍能表达。
	fc.out["dpkg-query -W -f=${Conffiles}\\n nginx"] = " /etc/nginx/a\\134b.conf " + conffileMD5Nginx + "\n"
	fc.out["dpkg-query -L nginx"] = "/etc/nginx/a\\134b.conf\n"
	withFakeCommands(t, fc)
	withAptLog(t, aptLogNginx)

	p, err := BuildPlan(context.Background(), aptNginxHandle, allowAll{})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(p.Steps) != 3 {
		t.Fatalf("steps = %d, want 3:%+v", len(p.Steps), p.Steps)
	}
	del := p.Steps[1]
	want := "/etc/nginx/a\\b.conf"
	if !slices.Equal(del.Paths, []string{want}) {
		t.Errorf("delete.Paths = %v, want [%q]", del.Paths, want)
	}
	if del.Command != "rm -rf "+want {
		t.Errorf("delete.Command = %q, want %q", del.Command, "rm -rf "+want)
	}
	if !slices.Contains(del.OwnedPaths, want) {
		t.Errorf("delete.OwnedPaths = %v, want 含还原后的 %q", del.OwnedPaths, want)
	}
}

// \040 是空格:路径本身解析得出来,但 rm 的 argv 形状表达不了带空白的路径
// (白名单要求路径里没有空白)。这种情况留给人,不塞进删除步。
func TestPlan_AptFallbackSkipsUnsafeConffilePath(t *testing.T) {
	fc := aptFallbackFake()
	fc.out["dpkg-query -W -f=${Conffiles}\\n nginx"] = " /etc/nginx/a\\040b.conf " + conffileMD5Nginx + "\n"
	fc.out["dpkg-query -L nginx"] = "/etc/nginx/a\\040b.conf\n"
	withFakeCommands(t, fc)
	withAptLog(t, aptLogNginx)

	p, err := BuildPlan(context.Background(), aptNginxHandle, allowAll{})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	wantKinds := []StepKind{StepPackageRemove, StepManual}
	if len(p.Steps) != len(wantKinds) {
		t.Fatalf("steps = %d, want %d(没有可安全删除的 conffile):%+v", len(p.Steps), len(wantKinds), p.Steps)
	}
	for i, want := range wantKinds {
		if p.Steps[i].Kind != want {
			t.Fatalf("step %d Kind = %v, want %v", i, p.Steps[i].Kind, want)
		}
	}
	if !strings.Contains(p.Steps[1].Rationale, "/etc/nginx/a b.conf") {
		t.Errorf("MANUAL 说明 = %q, want 点名没能自动删除的路径", p.Steps[1].Rationale)
	}
}

// 空 conffiles 是正常情况(包没有配置文件):只卸载,不产生删除步。
func TestPlan_AptFallbackEmptyConffiles(t *testing.T) {
	fc := aptFallbackFake()
	fc.out["dpkg-query -W -f=${Conffiles}\\n nginx"] = "\n"
	withFakeCommands(t, fc)
	withAptLog(t, aptLogNginx)

	p, err := BuildPlan(context.Background(), aptNginxHandle, allowAll{})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(p.Steps) != 2 {
		t.Fatalf("steps = %d, want 2(卸载 + 人工核对):%+v", len(p.Steps), p.Steps)
	}
	for _, s := range p.Steps {
		if s.Kind == StepFileDelete {
			t.Fatalf("没有 conffiles 时不该生成删除步: %+v", s)
		}
	}
}

// conffile 记录与 dpkg-query -L 对不上(清单里没有这个路径) → 不删,点名给人。
func TestPlan_AptFallbackSkipsConffileMissingFromFileList(t *testing.T) {
	fc := aptFallbackFake()
	fc.out["dpkg-query -W -f=${Conffiles}\\n nginx"] =
		" /etc/nginx/ghost.conf " + conffileMD5Nginx + "\n"
	fc.out["dpkg-query -L nginx"] = "/etc/nginx\n/usr/sbin/nginx\n"
	withFakeCommands(t, fc)
	withAptLog(t, aptLogNginx)

	p, err := BuildPlan(context.Background(), aptNginxHandle, allowAll{})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(p.Steps) != 2 {
		t.Fatalf("steps = %d, want 2(两份 dpkg 记录对不上时不出删除步):%+v", len(p.Steps), p.Steps)
	}
	if !strings.Contains(p.Steps[1].Rationale, "/etc/nginx/ghost.conf") {
		t.Errorf("MANUAL 说明 = %q, want 点名没被证明的路径", p.Steps[1].Rationale)
	}
}

// 拿不到包管理器事实 → 计划期失败,不产生半截计划。
func TestPlan_AptFallbackFailsWhenDpkgQueryFails(t *testing.T) {
	for _, key := range []string{
		"dpkg-query -L nginx",
		"dpkg-query -W -f=${Conffiles}\\n nginx",
	} {
		t.Run(key, func(t *testing.T) {
			fc := aptFallbackFake()
			fc.err[key] = errors.New("exit status 1")
			withFakeCommands(t, fc)
			withAptLog(t, aptLogNginx)

			p, err := BuildPlan(context.Background(), aptNginxHandle, allowAll{})
			if err == nil {
				t.Fatalf("BuildPlan(%q 失败) = nil error, want fail-closed", key)
			}
			if len(p.Steps) != 0 {
				t.Fatalf("失败时返回了半截计划: %+v", p.Steps)
			}
		})
	}
}

// 形状不合 dpkg 的 conffile 记录就不能"猜着"处理:猜错就是把别人的文件写进
// 删除目标。
func TestPlan_AptFallbackRejectsMalformedConffiles(t *testing.T) {
	for _, line := range []string{
		" /etc/nginx/a.conf\n",                                     // 只有路径,缺 md5
		" /etc/nginx/a.conf " + conffileMD5Nginx + " bogus\n",      // 不认识的状态词
		" /etc/nginx/a.conf " + conffileMD5Nginx + " obsolete x\n", // 四段
	} {
		t.Run(strings.TrimSpace(line), func(t *testing.T) {
			fc := aptFallbackFake()
			fc.out["dpkg-query -W -f=${Conffiles}\\n nginx"] = line
			withFakeCommands(t, fc)
			withAptLog(t, aptLogNginx)

			if _, err := BuildPlan(context.Background(), aptNginxHandle, allowAll{}); err == nil {
				t.Fatalf("BuildPlan 对无法解析的 conffile 行 %q 返回 nil error, want fail-closed", line)
			}
		})
	}
}

// 样本逐字来自 ubuntu2604.txt(Ubuntu 26.04 / apt 3.2.1)的
// /var/log/apt/history.log 与 `apt history-info 87`:两边的 Start time 一致,
// 这才允许按日志序号原生撤销。
const ubuntuAptLog = `
Start-Date: 2026-11-06  10:28:29
Commandline: apt-get --yes -oDebug::pkgDepCache::AutoInstall=yes --no-install-recommends --force-yes upgrade
Upgrade: apt:amd64 (3.2.0 -> 3.2.1ubuntu0.1)
Upgrade: base-files:amd64 (14ubuntu6 -> 14ubuntu6.3)
End-Date: 2026-11-06  10:28:36
`

const ubuntuAptHistoryInfo = `Transaction ID: 0
Start time: 2026-11-06  10:28:29
End time: 2026-11-06  10:28:36
Command line: apt-get --yes -oDebug::pkgDepCache::AutoInstall=yes --no-install-recommends --force-yes upgrade
Packages changed:
    Upgrade apt:amd64 (3.2.0 -> 3.2.1ubuntu0.1)
    Upgrade base-files:amd64 (14ubuntu6 -> 14ubuntu6.3)
`

// apt ≥ 3.2 且 history-info 的 Start time 与日志对得上 → 单步原生撤销、自动
// 执行。序号是日志里的 0-based 位置。
func TestPlan_AptNativeUndoCrossChecked(t *testing.T) {
	fc := newFakeCommands()
	fc.out["apt-get --version"] = "apt 3.2.1 (amd64)\n"
	fc.out["apt history-info 0"] = ubuntuAptHistoryInfo
	withFakeCommands(t, fc)
	withAptLog(t, ubuntuAptLog)

	p, err := BuildPlan(context.Background(), aptLogHandle(ubuntuAptLog, "apt"), allowAll{})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if !p.AutoOK {
		t.Error("apt≥3.2 且交叉核对通过时应可自动执行")
	}
	if len(p.Steps) != 1 || p.Steps[0].Kind != StepNativeUndo {
		t.Fatalf("steps = %+v, want 单步 NATIVE_UNDO", p.Steps)
	}
	if want := "apt history-undo 0"; p.Steps[0].Command != want {
		t.Errorf("Command = %q, want %q", p.Steps[0].Command, want)
	}
	if p.Steps[0].HumanReview {
		t.Error("原生撤销不该要人审")
	}
}

// 序号必须是日志里的位置,不是随手取 0:目标条目排第三 → history-undo 2。
func TestPlan_AptNativeUndoUsesLogPosition(t *testing.T) {
	log := `
Start-Date: 2026-11-06  10:20:00
Commandline: apt-get install -y aaa
Install: aaa:amd64 (1.0)
End-Date: 2026-11-06  10:20:01

Start-Date: 2026-11-06  10:22:00
Commandline: apt-get install -y bbb
Install: bbb:amd64 (1.0)
End-Date: 2026-11-06  10:22:01
` + ubuntuAptLog

	fc := newFakeCommands()
	fc.out["apt-get --version"] = "apt 3.2.1 (amd64)\n"
	fc.out["apt history-info 2"] = ubuntuAptHistoryInfo
	withFakeCommands(t, fc)
	withAptLog(t, log)

	p, err := BuildPlan(context.Background(), aptLogHandle(log, "apt"), allowAll{})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(p.Steps) != 1 || p.Steps[0].Command != "apt history-undo 2" {
		t.Fatalf("steps = %+v, want [apt history-undo 2]", p.Steps)
	}
}

// 交叉核对不一致(history-info 说的是别的事务)→ 不许赌序号,退回退化计划。
func TestPlan_AptNativeUndoCrossCheckMismatchFallsBack(t *testing.T) {
	fc := newFakeCommands()
	fc.out["apt-get --version"] = "apt 3.2.1 (amd64)\n"
	fc.out["apt history-info 0"] = strings.Replace(ubuntuAptHistoryInfo,
		"Start time: 2026-11-06  10:28:29", "Start time: 2026-11-06  10:27:29", 1)
	fc.out["dpkg-query -L apt"] = "/.\n/usr/bin\n"
	fc.out["dpkg-query -W -f=${Conffiles}\\n apt"] = "\n"
	withFakeCommands(t, fc)
	withAptLog(t, ubuntuAptLog)

	p, err := BuildPlan(context.Background(), aptLogHandle(ubuntuAptLog, "apt"), allowAll{})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p.AutoOK {
		t.Error("核对不一致时必须退回退化计划(人审)")
	}
	if len(p.Steps) == 0 || p.Steps[0].Command != "apt-get remove -y apt" {
		t.Fatalf("steps = %+v, want 以 apt-get remove -y apt 开头", p.Steps)
	}
	for _, s := range p.Steps {
		if s.Kind == StepNativeUndo {
			t.Fatalf("核对不一致时不该出现原生撤销步: %+v", s)
		}
	}
}

// history-info 失败同样是"核对不了"→ 退回退化计划,不猜。
func TestPlan_AptNativeUndoHistoryInfoErrorFallsBack(t *testing.T) {
	fc := newFakeCommands()
	fc.out["apt-get --version"] = "apt 3.2.1 (amd64)\n"
	fc.err["apt history-info 0"] = errors.New("exit status 100")
	fc.out["dpkg-query -L apt"] = "/.\n/usr/bin\n"
	fc.out["dpkg-query -W -f=${Conffiles}\\n apt"] = "\n"
	withFakeCommands(t, fc)
	withAptLog(t, ubuntuAptLog)

	p, err := BuildPlan(context.Background(), aptLogHandle(ubuntuAptLog, "apt"), allowAll{})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p.AutoOK {
		t.Error("history-info 失败时必须退回退化计划(人审)")
	}
}

// 日志里已经没有句柄指向的 Start-Date(轮转 / 换机器)→ 计划失败,不猜。
func TestPlan_AptLogEntryMissing(t *testing.T) {
	fc := aptFallbackFake()
	withFakeCommands(t, fc)
	withAptLog(t, aptLogNginx)

	start := exec.ParseAptHistoryLog(aptLogNginx)[0].StartDate.Unix() + 1
	_, err := BuildPlan(context.Background(), "apt-log:"+strconv.FormatInt(start, 10)+":nginx", allowAll{})
	if !errors.Is(err, ErrPlanUnsupported) {
		t.Fatalf("got %v, want ErrPlanUnsupported", err)
	}
}

// 句柄里的包名不在该条目的变更集合里 → 计划失败:句柄与日志对不上时,
// 拿句柄里的名字去卸载等于删错东西。
func TestPlan_AptLogPackageNotInEntry(t *testing.T) {
	fc := aptFallbackFake()
	withFakeCommands(t, fc)
	withAptLog(t, aptLogNginx)

	start := exec.ParseAptHistoryLog(aptLogNginx)[0].StartDate.Unix()
	_, err := BuildPlan(context.Background(), "apt-log:"+strconv.FormatInt(start, 10)+":hello", allowAll{})
	if !errors.Is(err, ErrPlanUnsupported) {
		t.Fatalf("got %v, want ErrPlanUnsupported", err)
	}
	for _, c := range fc.calls {
		if strings.Contains(c, "dpkg-query") {
			t.Errorf("句柄与日志对不上时不该查 dpkg: %q", c)
		}
	}
}

// 日志读不了 → fail-closed(没有日志就证明不了任何事)。
func TestPlan_AptLogUnreadable(t *testing.T) {
	withFakeCommands(t, newFakeCommands())
	withAptLogError(t, errors.New("no such file"))

	_, err := BuildPlan(context.Background(), "apt-log:1763119725:nginx", allowAll{})
	if !errors.Is(err, ErrPlanUnsupported) {
		t.Fatalf("got %v, want ErrPlanUnsupported", err)
	}
}

// 句柄形状不合法 → 计划失败,且不产生半截计划。
func TestPlan_AptLogMalformedHandle(t *testing.T) {
	for _, id := range []string{
		"apt-log:notanumber:nginx",
		"apt-log:1763119725",
		"apt-log:1763119725:",
		"apt-log:1763119725:../nginx",
		"apt-log:1763119725:-evil",
	} {
		t.Run(id, func(t *testing.T) {
			withFakeCommands(t, newFakeCommands())
			withAptLog(t, aptLogNginx)

			p, err := BuildPlan(context.Background(), id, allowAll{})
			if !errors.Is(err, ErrPlanUnsupported) {
				t.Fatalf("got %v, want ErrPlanUnsupported", err)
			}
			if len(p.Steps) != 0 {
				t.Fatalf("失败时返回了半截计划: %+v", p.Steps)
			}
		})
	}
}

// dpkg 的 Conffiles 每行是 " <路径> <md5|newconffile> [obsolete|remove-on-upgrade]":
// 前导空格与 md5 都不是路径。
func TestParseConffiles_RealDpkgShape(t *testing.T) {
	out := strings.Join([]string{
		" /etc/a.conf " + conffileMD5Nginx,
		" /etc/b.conf " + conffileMD5Nginx + " obsolete",
		" /etc/c.conf " + conffileMD5Nginx + " remove-on-upgrade",
		" /etc/d.conf newconffile",
	}, "\n") + "\n"

	got, err := parseConffiles(out)
	if err != nil {
		t.Fatalf("parseConffiles: %v", err)
	}
	want := []string{"/etc/a.conf", "/etc/b.conf", "/etc/c.conf", "/etc/d.conf"}
	if !slices.Equal(got, want) {
		t.Errorf("parseConffiles = %v, want %v", got, want)
	}
}

// 第二段必须是 32 位小写十六进制或 newconffile;第三段只能是两个状态词之一。
func TestParseConffiles_RejectsMalformedLines(t *testing.T) {
	for name, line := range map[string]string{
		"只有路径":     " /etc/a.conf",
		"第二段不是校验和": " /etc/a.conf notamd5",
		"大写 md5":   " /etc/a.conf 89269E1298235F1B12B4C16E4065AD0D",
		"短 md5":    " /etc/a.conf 89269e12",
		"未知状态词":    " /etc/a.conf " + conffileMD5Nginx + " bogus",
		"四段":       " /etc/a.conf " + conffileMD5Nginx + " obsolete extra",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConffiles(line + "\n"); err == nil {
				t.Fatalf("parseConffiles(%q) = nil error, want fail-closed", line)
			}
		})
	}
}

// service 句柄 → 反向动词(恢复到 apply 之前的状态)。
func TestPlan_ServiceReverseVerb(t *testing.T) {
	withFakeCommands(t, newFakeCommands())

	handle := "service-state:" + base64.RawURLEncoding.EncodeToString([]byte(
		`{"unit":"nginx.service","active_state":"inactive","unit_file_state":"disabled","sub_state":"dead"}`))
	p, err := BuildPlan(context.Background(), handle, allowAll{})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	var cmds []string
	for _, s := range p.Steps {
		cmds = append(cmds, s.Command)
	}
	joined := strings.Join(cmds, " | ")
	if !strings.Contains(joined, "systemctl stop nginx.service") {
		t.Errorf("steps = %v, want 含 systemctl stop nginx.service", cmds)
	}
	if !strings.Contains(joined, "systemctl disable nginx.service") {
		t.Errorf("steps = %v, want 含 systemctl disable nginx.service", cmds)
	}
	for _, s := range p.Steps {
		if s.Kind != StepManual && s.HumanReview {
			t.Errorf("step %q 不该要人审:反向动词不销毁数据", s.Command)
		}
	}
}

// 前置状态是 active/enabled → 反向动词是 start/enable。
func TestPlan_ServiceReverseVerbActive(t *testing.T) {
	withFakeCommands(t, newFakeCommands())

	handle := "service-state:" + base64.RawURLEncoding.EncodeToString([]byte(
		`{"unit":"nginx.service","active_state":"active","unit_file_state":"enabled","sub_state":"running"}`))
	p, err := BuildPlan(context.Background(), handle, allowAll{})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	var cmds []string
	for _, s := range p.Steps {
		cmds = append(cmds, s.Command)
	}
	joined := strings.Join(cmds, " | ")
	if !strings.Contains(joined, "systemctl start nginx.service") {
		t.Errorf("steps = %v, want 含 systemctl start nginx.service", cmds)
	}
	if !strings.Contains(joined, "systemctl enable nginx.service") {
		t.Errorf("steps = %v, want 含 systemctl enable nginx.service", cmds)
	}
}

// 认不出来的句柄 → 拒绝(fail-closed,不猜)。
func TestPlan_RejectsUnknownHandle(t *testing.T) {
	withFakeCommands(t, newFakeCommands())

	for _, h := range []string{"", "bogus:1", "dnf-history:abc"} {
		if _, err := BuildPlan(context.Background(), h, allowAll{}); err == nil {
			t.Errorf("BuildPlan(%q) = nil error, want error", h)
		}
	}
}

// 命令不在白名单 → 计划生成失败(fail-closed)。
func TestPlan_RejectsDisallowedCommand(t *testing.T) {
	withFakeCommands(t, newFakeCommands())

	if _, err := BuildPlan(context.Background(), "dnf-history:42", denyAll{}); err == nil {
		t.Fatal("BuildPlan with a denying allowlist = nil error, want error")
	}
}

// 计划哈希必须稳定且只随计划内容变。
func TestPlan_HashIsStable(t *testing.T) {
	withFakeCommands(t, newFakeCommands())

	p1, err := BuildPlan(context.Background(), "dnf-history:42", allowAll{})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	p2, err := BuildPlan(context.Background(), "dnf-history:42", allowAll{})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p1.Hash() != p2.Hash() {
		t.Errorf("hash 不稳定: %q vs %q", p1.Hash(), p2.Hash())
	}
	if p1.Hash() == "" {
		t.Error("hash 不能为空")
	}

	p2.Steps[0].Command = "dnf history undo 43"
	if p1.Hash() == p2.Hash() {
		t.Error("命令变了哈希必须跟着变")
	}
}
