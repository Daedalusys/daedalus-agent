package exec

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// dnf 装完包必须留下可持久引用的回滚句柄:控制面只存这个串,事后靠它发起
// 回滚。句柄里的 id 必须是**本次**事务,不是同包的历史事务。
func TestPackage_HandleAfterDnfInstall(t *testing.T) {
	h := &fakeHost{manager: "dnf", installed: map[string]bool{}}
	h.historyOut = `[{"id":41,"command_line":"upgrade curl"},{"id":42,"command_line":"install nginx"}]`
	withFakeHost(t, h)

	handle, err := ApplyWithHandle(context.Background(), pkgIntent(`{"desired_state":"present"}`, "nginx"))
	if err != nil {
		t.Fatalf("ApplyWithHandle: unexpected err: %v", err)
	}
	if want := handleDnfHistory + "42"; handle != want {
		t.Fatalf("handle = %q, want %q", handle, want)
	}
}

// apt 同理:句柄锚定 /var/log/apt/history.log 里本次事务的 Start-Date。
//
// 不能锚历史序号:apt 3.2 之前没有 history 子命令(序号无从取得),而日志
// 序号会被 logrotate 平移,存下来的旧句柄会指向别的事务。
func TestPackage_HandleAfterAptInstall(t *testing.T) {
	start := time.Now().Truncate(time.Second)
	h := &fakeHost{
		manager:   "apt",
		installed: map[string]bool{},
		aptLog: aptLogBlock(start,
			"apt-get install -y nginx",
			"Install: nginx:amd64 (1.24.0-1)"),
	}
	withFakeHost(t, h)

	handle, err := ApplyWithHandle(context.Background(), pkgIntent(`{"desired_state":"present"}`, "nginx"))
	if err != nil {
		t.Fatalf("ApplyWithHandle: unexpected err: %v", err)
	}
	want := handleAptLog + strconv.FormatInt(start.Unix(), 10) + ":nginx"
	if handle != want {
		t.Fatalf("handle = %q, want %q", handle, want)
	}
}

// apt 日志已经存在但本次 apply 的包不在窗口内(典型:同名历史事务来自很久
// 以前):宁可空句柄,也不能把别的事务当成这次的回滚锚点。
func TestPackage_HandleAfterAptInstallSkipsOldTransaction(t *testing.T) {
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	h := &fakeHost{
		manager:   "apt",
		installed: map[string]bool{},
		aptLog: aptLogBlock(old,
			"apt-get install -y nginx",
			"Install: nginx:amd64 (1.24.0-1)"),
	}
	withFakeHost(t, h)

	handle, err := ApplyWithHandle(context.Background(), pkgIntent(`{"desired_state":"present"}`, "nginx"))
	if err != nil {
		t.Fatalf("ApplyWithHandle: unexpected err: %v", err)
	}
	if handle != "" {
		t.Fatalf("handle = %q, want empty: 旧事务不是本次 apply 的锚点", handle)
	}
}

// aptLogBlock 拼一段 history.log 条目。时间用两个空格分隔日期与时间,与
// apt 的真实写法一致。
func aptLogBlock(start time.Time, cmdline, action string) string {
	ts := start.Format("2006-01-02  15:04:05")
	return "\nStart-Date: " + ts + "\n" +
		"Commandline: " + cmdline + "\n" +
		action + "\n" +
		"End-Date: " + ts + "\n"
}

// apply 是 no-op 时不能给句柄:句柄会让控制面事后"回滚"一次从未发生的变更
// (比如把本来就在跑的 nginx 停掉)。
func TestPackage_HandleEmptyWhenNoChange(t *testing.T) {
	h := &fakeHost{manager: "dnf", installed: map[string]bool{"nginx": true}}
	h.historyOut = `[{"id":42,"command_line":"install nginx"}]`
	withFakeHost(t, h)

	handle, err := ApplyWithHandle(context.Background(), pkgIntent(`{"desired_state":"present"}`, "nginx"))
	if err != nil {
		t.Fatalf("ApplyWithHandle: unexpected err: %v", err)
	}
	if handle != "" {
		t.Fatalf("handle = %q, want empty: 本次 apply 没有改动宿主", handle)
	}
}

// 句柄捕获失败只能让句柄为空,绝不能再把已经落地的 apply 报成失败
// (否则控制面会重试一个已经生效的变更)。
func TestPackage_HandleCaptureFailureStillSucceeds(t *testing.T) {
	h := &fakeHost{manager: "dnf", installed: map[string]bool{}}
	h.historyOut = "error: cannot read history"
	h.historyErr = errors.New("exit status 1")
	withFakeHost(t, h)

	handle, err := ApplyWithHandle(context.Background(), pkgIntent(`{"desired_state":"present"}`, "nginx"))
	if err != nil {
		t.Fatalf("apply 结局被句柄捕获失败改写了: %v", err)
	}
	if handle != "" {
		t.Fatalf("handle = %q, want empty", handle)
	}
}

// service 的句柄必须带 apply **之前**的状态:apply 之后读到的状态是变更
// 结果,拿它算反向动词只会原地打转。
func TestService_HandleCapturesPreState(t *testing.T) {
	const unit = "nginx.service"
	h := newFakeSystemd(unit) // ActiveState=inactive / UnitFileState=disabled
	withFakeSystemd(t, h)

	handle, err := ApplyWithHandle(context.Background(), svcIntent("started", unit))
	if err != nil {
		t.Fatalf("ApplyWithHandle: unexpected err: %v", err)
	}
	if !strings.HasPrefix(handle, handleService) {
		t.Fatalf("handle = %q, want prefix %q", handle, handleService)
	}
	st, err := DecodeServiceState(strings.TrimPrefix(handle, handleService))
	if err != nil {
		t.Fatalf("DecodeServiceState: %v", err)
	}
	if st.Unit != unit {
		t.Errorf("Unit = %q, want %q", st.Unit, unit)
	}
	if st.ActiveState != "inactive" {
		t.Errorf("ActiveState = %q, want inactive (apply 前的状态)", st.ActiveState)
	}
	if st.UnitFileState != "disabled" {
		t.Errorf("UnitFileState = %q, want disabled", st.UnitFileState)
	}
}

// service 是 no-op 时不给句柄(理由同 package:回滚会停掉一个我们没启动的服务)。
func TestService_HandleEmptyWhenNoChange(t *testing.T) {
	const unit = "nginx.service"
	h := newFakeSystemd(unit)
	h.active[unit] = "active"
	withFakeSystemd(t, h)

	handle, err := ApplyWithHandle(context.Background(), svcIntent("started", unit))
	if err != nil {
		t.Fatalf("ApplyWithHandle: unexpected err: %v", err)
	}
	if handle != "" {
		t.Fatalf("handle = %q, want empty", handle)
	}
}

// file 的回滚点仍是内建 copy,不产句柄。
func TestHandle_FileKindHasNoHandle(t *testing.T) {
	target := filepath.Join(t.TempDir(), "app.conf")
	intent := &agentv1.ApplyIntentRequest{
		IntentId: "i-file",
		Kind:     kindFile,
		Target:   target,
		SpecJson: `{"content":"hello"}`,
	}
	handle, err := ApplyWithHandle(context.Background(), intent)
	if err != nil {
		t.Fatalf("ApplyWithHandle: unexpected err: %v", err)
	}
	if handle != "" {
		t.Fatalf("handle = %q, want empty for kind=file", handle)
	}
}

// 句柄前缀是控制面与 agent 之间的契约:认识的前缀要能拆出 id,不认识的一律
// 报错(fail-closed,不猜)。
func TestHandle_DecodeKnownAndUnknown(t *testing.T) {
	kind, id, err := decodeHandle(handleDnfHistory + "42")
	if err != nil {
		t.Fatalf("decodeHandle: %v", err)
	}
	if kind != "dnf-history" || id != "42" {
		t.Fatalf("decodeHandle = (%q,%q), want (dnf-history,42)", kind, id)
	}
	kind, id, err = decodeHandle(handleAptLog + "1763119725:hello")
	if err != nil {
		t.Fatalf("decodeHandle: %v", err)
	}
	if kind != "apt-log" || id != "1763119725:hello" {
		t.Fatalf("decodeHandle = (%q,%q), want (apt-log,1763119725:hello)", kind, id)
	}
	if _, _, err := decodeHandle("bogus:1"); err == nil {
		t.Fatal("decodeHandle(bogus) = nil error, want error")
	}
}

// 样本逐字来自 Debian 13 / apt 3.0.4 真装 hello 之后的
// /var/log/apt/history.log(`apt history-list` 在该版本是 exit 100,日志是
// 唯一可用的锚点)。
const debian13HelloLog = `
Start-Date: 2026-11-14  11:28:45
Commandline: apt-get install -y -qq --no-install-recommends hello
Install: hello:amd64 (2.10-5)
End-Date: 2026-11-14  11:28:45
`

func mustAptTime(t *testing.T, v string) time.Time {
	t.Helper()
	tm, err := ParseAptLogTime(v)
	if err != nil {
		t.Fatalf("ParseAptLogTime(%q): %v", v, err)
	}
	return tm
}

// apt 的日志条目形状:Start-Date / Commandline / 动作行 / End-Date。包名要剥掉
// :arch,行尾的 (version, automatic) 是括号内的逗号,不能当包分隔符。
func TestParseAptHistoryLog_Debian13Shape(t *testing.T) {
	entries := ParseAptHistoryLog(debian13HelloLog)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if want := mustAptTime(t, "2026-11-14  11:28:45"); !e.StartDate.Equal(want) {
		t.Errorf("StartDate = %v, want %v", e.StartDate, want)
	}
	if e.CommandLine != "apt-get install -y -qq --no-install-recommends hello" {
		t.Errorf("CommandLine = %q", e.CommandLine)
	}
	if len(e.Packages) != 1 || e.Packages[0] != "hello" {
		t.Errorf("Packages = %v, want [hello]", e.Packages)
	}
}

// 一行多个包、长行续行(续行以空格开头)、括号内的 ", automatic" 都不是包
// 分隔符。
func TestParseAptHistoryLog_MultiPackageAndContinuation(t *testing.T) {
	const log = `
Start-Date: 2026-11-14  11:28:45
Commandline: apt-get install -y aaa bbb ccc ddd
Install: aaa:amd64 (1.0), bbb:amd64 (2.0), ccc:amd64
 (3.0), ddd:amd64 (4.0, automatic)
End-Date: 2026-11-14  11:28:46

Start-Date: 2026-11-14  11:30:00
Commandline: apt-get remove -y bbb
Remove: bbb:amd64 (2.0)
End-Date: 2026-11-14  11:30:01
`
	entries := ParseAptHistoryLog(log)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	want := []string{"aaa", "bbb", "ccc", "ddd"}
	if strings.Join(entries[0].Packages, ",") != strings.Join(want, ",") {
		t.Errorf("Packages = %v, want %v", entries[0].Packages, want)
	}
	if len(entries[1].Packages) != 1 || entries[1].Packages[0] != "bbb" {
		t.Errorf("second entry Packages = %v, want [bbb]", entries[1].Packages)
	}
}

// 解析不了的 token / 行跳过,不猜:空包名、缺版本的动作行都不该变成包名。
func TestParseAptHistoryLog_SkipsUnparseablePieces(t *testing.T) {
	const log = `
Start-Date: 2026-11-14  11:28:45
Commandline: apt-get install -y good other
Install: good:amd64 (1.0), :amd64 (2.0), -bad:amd64 (3.0)
Upgrade: other:amd64 (1.0 -> 1.1)
Requested-By: root (1000)
End-Date: 2026-11-14  11:28:45
`
	entries := ParseAptHistoryLog(log)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	want := []string{"good", "other"}
	if strings.Join(entries[0].Packages, ",") != strings.Join(want, ",") {
		t.Errorf("Packages = %v, want %v", entries[0].Packages, want)
	}
}

// 没有可解析 Start-Date 的块不是锚点:宁可没有条目,也不拿垃圾时间当锚点。
func TestParseAptHistoryLog_SkipsEntriesWithoutStartDate(t *testing.T) {
	const log = `
Start-Date: not-a-time
Commandline: apt-get install -y nginx
Install: nginx:amd64 (1.0)
End-Date: also-not-a-time
`
	if entries := ParseAptHistoryLog(log); len(entries) != 0 {
		t.Fatalf("entries = %v, want none", entries)
	}
}

// 捕获的窗口语义:Start-Date 不早于 since-5s(秒级取整余量);两条都命中时
// 取日志里最后一条(刚发生的排在最后)。
func TestCaptureAptHandle_TimeWindowAndLatest(t *testing.T) {
	const log = `
Start-Date: 2026-11-14  11:20:00
Commandline: apt-get install -y hello
Install: hello:amd64 (2.10-5)
End-Date: 2026-11-14  11:20:01

Start-Date: 2026-11-14  11:28:45
Commandline: apt-get install -y hello
Install: hello:amd64 (2.10-5)
End-Date: 2026-11-14  11:28:46
`
	early := mustAptTime(t, "2026-11-14  11:20:00")
	late := mustAptTime(t, "2026-11-14  11:28:45")

	cases := []struct {
		name  string
		since time.Time
		want  string
	}{
		{"窗口内取最后一条", early.Add(-time.Second), handleAptLog + strconv.FormatInt(late.Unix(), 10) + ":hello"},
		{"边界 since-5s 仍算命中", late.Add(5 * time.Second), handleAptLog + strconv.FormatInt(late.Unix(), 10) + ":hello"},
		{"两条都太早", late.Add(6 * time.Second), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFakeHost(t, &fakeHost{manager: "apt", aptLog: log})
			got, err := captureAptHandle(context.Background(), "hello", tc.since)
			if err != nil {
				t.Fatalf("captureAptHandle: %v", err)
			}
			if got != tc.want {
				t.Fatalf("handle = %q, want %q", got, tc.want)
			}
		})
	}
}

// 日志不存在 / 读不了不是错误(与 dnf 同语义):句柄为空,由控制面按"没有
// 回滚锚点"处理。
func TestCaptureAptHandle_MissingLogIsNotAnError(t *testing.T) {
	withFakeHost(t, &fakeHost{manager: "apt", aptLogErr: errors.New("no such file")})

	got, err := captureAptHandle(context.Background(), "hello", time.Unix(0, 0))
	if err != nil {
		t.Fatalf("captureAptHandle: %v", err)
	}
	if got != "" {
		t.Fatalf("handle = %q, want empty", got)
	}
}

// 样本逐字来自 AlmaLinux 9 / dnf 4.14 的 `dnf history list` 与
// `dnf history info 1`(`dnf history --json` 在该版本是 exit 2)。
const (
	alma9HistoryList = `ID     | Command line             | Date and time    | Action(s)      | Altered
-------------------------------------------------------------------------------
     2 | reinstall -y --installro | 2026-10-27 10:52 | R              |    2   
     1 | install -y --installroot | 2026-10-27 10:52 | Install        |  158 EE
`
	alma9HistoryInfo1 = `Transaction ID : 1
Begin time     : Tue Oct 27 10:52:29 2026
Begin rpmdb    : a868cd6381e65568d08c840aaffe0f8e59e7274c937345831503caf8a4ec388a
End time       : Tue Oct 27 10:52:44 2026 (15 seconds)
Command Line   : install -y --installroot /mnt/sys-root --releasever 9 --setopt install_weak_deps=false --nodocs almalinux-release bash binutils
Persistence    : Persist
Packages Altered:
    Install acl-2.4.0-1.el9_8.x86_64                                   @baseos
    Install bash-5.1.8-6.el9_1.x86_64                                  @baseos
`
	alma9HistoryInfo2 = `Transaction ID : 2
Begin time     : Tue Oct 27 10:52:35 2026
Command Line   : reinstall -y --installroot /mnt/sys-root almalinux-release
Packages Altered:
    Reinstall almalinux-release-9.8-1.el9.x86_64                       @baseos
`
)

func dnfTextFake() *fakeHost {
	return &fakeHost{
		manager:   "dnf",
		installed: map[string]bool{"bash": true},
		replies: map[string]fakeReply{
			"dnf history --json": {out: "usage: dnf history [OPTIONS]", err: errors.New("exit status 2")},
			"dnf history list":   {out: alma9HistoryList},
			"dnf history info 2": {out: alma9HistoryInfo2},
			"dnf history info 1": {out: alma9HistoryInfo1},
		},
	}
}

// dnf4 不认识 --json:必须退回文本界面(list 取候选 id,info 取 Begin time 与
// 包清单),否则 Alma 9/10 永远拿不到句柄。
func TestCaptureDnfHandle_FallsBackToText(t *testing.T) {
	h := dnfTextFake()
	withFakeHost(t, h)

	got, err := captureDnfHandle(context.Background(), "bash", time.Unix(0, 0))
	if err != nil {
		t.Fatalf("captureDnfHandle: %v", err)
	}
	if want := handleDnfHistory + "1"; got != want {
		t.Fatalf("handle = %q, want %q", got, want)
	}
	// 从 id 最大的候选开始:刚发生的变更排在最后。
	var infoOrder []string
	for _, c := range h.calls {
		if len(c) >= 2 && c[0] == "dnf" && c[1] == "history" && len(c) >= 3 && c[2] == "info" {
			infoOrder = append(infoOrder, strings.Join(c, " "))
		}
	}
	if want := "dnf history info 2|dnf history info 1"; strings.Join(infoOrder, "|") != want {
		t.Fatalf("info 调用顺序 = %v, want %q", infoOrder, want)
	}
}

// Begin time 早于 since-5s → 不是本次 apply 的事务;两条候选都不匹配时
// fail-closed 报错(不给空句柄假装"没有锚点")。
func TestCaptureDnfHandle_TextBeginTimeTooOld(t *testing.T) {
	h := dnfTextFake()
	withFakeHost(t, h)

	begin := mustDnfTime(t, "Tue Oct 27 10:52:29 2026")
	if _, err := captureDnfHandle(context.Background(), "bash", begin.Add(6*time.Second)); err == nil {
		t.Fatal("captureDnfHandle = nil error, want fail-closed")
	}
}

// 包名在命令行与包清单里都不出现 → 不匹配,报错(不拿别的事务凑数)。
func TestCaptureDnfHandle_TextPackageMissing(t *testing.T) {
	h := dnfTextFake()
	withFakeHost(t, h)

	if _, err := captureDnfHandle(context.Background(), "nginx", time.Unix(0, 0)); err == nil {
		t.Fatal("captureDnfHandle = nil error, want fail-closed")
	}
}

func mustDnfTime(t *testing.T, v string) time.Time {
	t.Helper()
	tm, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", v, time.Local)
	if err != nil {
		t.Fatalf("parse dnf time %q: %v", v, err)
	}
	return tm
}

// Packages Altered 列的是 NEVRA:包名自己可以带 '-',所以从右边剥 arch /
// release / version。
func TestDnfNevraName(t *testing.T) {
	cases := map[string]string{
		"acl-2.4.0-1.el9_8.x86_64":                    "acl",
		"python3-dnf-plugins-core-4.0.1-1.el9.noarch": "python3-dnf-plugins-core",
		"bash-5.1.8-6.el9_1.x86_64":                   "bash",
		"1:foo-1.0-1.el9.x86_64":                      "foo",
		"foo-1:1.0-1.el9.x86_64":                      "foo",
		"notanevra":                                   "",
		"too-short":                                   "",
		"-bad-1.0-1.x86_64":                           "",
	}
	for in, want := range cases {
		if got := dnfNevraName(in); got != want {
			t.Errorf("dnfNevraName(%q) = %q, want %q", in, got, want)
		}
	}
}

// 宿主 locale 会把 dnf/apt 的键名翻成中文(本机实测 "Begin time" → "起始时间"),
// 而句柄解析按英文字面匹配 —— 不固定 C locale,文本回退在中文宿主机上整条失效。
// 这里锁住 production runner 的环境构造(继承的 LC_ALL 必须被替换掉,
// getenv 取第一个匹配项,重复项会让继承值反压)。
func TestRunCmdProductionForcesCLocale(t *testing.T) {
	t.Setenv("LC_ALL", "zh_CN.UTF-8")

	out, err := runCmdProduction(context.Background(), "sh", "-c", `printf %s "$LC_ALL"`)
	if err != nil {
		t.Fatalf("runCmdProduction: %v", err)
	}
	if out != "C" {
		t.Fatalf("LC_ALL = %q, want C", out)
	}
}
