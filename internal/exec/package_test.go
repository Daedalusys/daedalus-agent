package exec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// fakeHost 模拟一台装了 apt-get 或 dnf 的宿主。它替换 runCmd 缝:
// 查询类命令(dpkg-query / rpm)回报 installed 里的状态,变更类命令
// (apt-get / dnf)记录 argv 并在成功时翻转状态。
//
// verbOut / verbErr 让测试注入管理器输出与退出错误,用来驱动错误分类;
// replies 按完整 argv 命中,给需要多条不同回包的历史查询用
// (dnf history list / info 的形状不同)。
type fakeHost struct {
	manager   string // "apt" | "dnf"
	installed map[string]bool
	calls     [][]string
	verbOut   string
	verbErr   error
	// historyOut / historyErr 是 dnf history --json 的回包;回滚句柄的 id
	// 就从这里来。
	historyOut string
	historyErr error
	// replies 按 "name arg1 arg2..." 命中,优先于上面的通用回包。
	// 命中顺序:完整 argv 精确匹配 → "name arg1" 前缀匹配(用于工作目录
	// 里带随机临时路径的命令,如 dpkg-deb -c /tmp/xxx/a.deb)→ 通用回包。
	replies map[string]fakeReply
	// dirCalls 记录经 runCmdInDir 发的命令(带工作目录):落点体检的下载
	// 必须发生在临时目录里,断言就靠它。
	dirCalls []dirCall
	// lastDownloadPkg 是最近一次模拟下载的包名,供 dpkg-deb -c / rpm -qlp
	// 的默认回包拼一份无害清单。
	lastDownloadPkg string
	// aptLog / aptLogErr 是 /var/log/apt/history.log 的内容(apt 句柄的唯一
	// 锚点来源)。
	aptLog    string
	aptLogErr error
	// versions 是已装包的实际版本(dpkg-query / rpm 的版本查询回包)。
	// 未登记的已装包回一个默认版本,让"未声明版本"的旧用例不必改写。
	versions map[string]string
	// versionOut / versionErr 让测试注入"版本查询本身失败"。
	versionOut string
	versionErr error
}

// fakeReply 是一条按 argv 命中的回包。
type fakeReply struct {
	out string
	err error
}

// dirCall 是一条经 runCmdInDir 发出的命令。
type dirCall struct {
	dir  string
	name string
	args []string
}

// reply 按 argv 找一条回包:先精确命中完整 argv,再退到"命令名 + 首个参数"
// (下载/装包命令的参数里可能带随机临时路径,测试无法预先拼出完整 argv)。
func (h *fakeHost) reply(name string, args []string) (fakeReply, bool) {
	if r, ok := h.replies[strings.TrimSpace(name+" "+strings.Join(args, " "))]; ok {
		return r, true
	}
	if len(args) > 0 {
		if r, ok := h.replies[name+" "+args[0]]; ok {
			return r, true
		}
	}
	return fakeReply{}, false
}

func (h *fakeHost) run(_ context.Context, name string, args ...string) (string, error) {
	h.calls = append(h.calls, append([]string{name}, args...))
	if r, ok := h.reply(name, args); ok {
		return r.out, r.err
	}
	last := ""
	if len(args) > 0 {
		last = args[len(args)-1]
	}
	// 落点体检的清单类查询:默认回一份无害清单(/usr/sbin/<name>),免得
	// 每个既有用例都要写一条 replies。要害路径的判断由新用例用 replies 覆盖。
	switch {
	case name == "dpkg-query" && len(args) == 2 && args[0] == "-L":
		return "./\n/usr/sbin/" + last + "\n", nil
	case name == "rpm" && len(args) == 2 && args[0] == "-ql":
		return "/usr/sbin/" + last + "\n", nil
	case name == "dpkg-deb" && len(args) == 2 && args[0] == "-c":
		return "drwxr-xr-x root/root 0 2024-01-01 00:00 ./\n" +
			"-rwxr-xr-x root/root 0 2024-01-01 00:00 ./usr/sbin/" + h.downloadedPkg() + "\n", nil
	case name == "rpm" && len(args) == 2 && args[0] == "-qlp":
		return "/usr/sbin/" + h.downloadedPkg() + "\n", nil
	}
	// 版本查询(dpkg-query -f=${Version} / rpm --qf)与"是否已装"的查询
	// 走不同的 argv,必须分开回包:否则实际版本会被回成状态串。
	if isVersionArgv(name, args) {
		if h.versionErr != nil {
			return h.versionOut, h.versionErr
		}
		if !h.installed[last] {
			return "package " + last + " is not installed", errors.New("exit status 1")
		}
		if v, ok := h.versions[last]; ok {
			return v, nil
		}
		return "1.24.0-1", nil
	}
	switch name {
	case "dpkg-query":
		if h.installed[last] {
			return "install ok installed", nil
		}
		return "dpkg-query: no packages found matching " + last, errors.New("exit status 1")
	case "rpm":
		if h.installed[last] {
			return last + "-1.24.0-1", nil
		}
		return "package " + last + " is not installed", errors.New("exit status 1")
	case "dnf", "apt":
		// 历史查询(dnf history --json 及其文本回退):只读,不改状态。
		return h.historyOut, h.historyErr
	case "/usr/bin/apt-get", "/usr/bin/dnf":
		verb := ""
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				verb = a
				break
			}
		}
		// verbOut / verbErr 描述的是**变更**动词的失败(装/升/卸)。download
		// 是落点体检自己的只读动作,不该被"安装失败"的注入连坐 —— 否则
		// 既有用例测的就不再是变更失败,而是落点体检失败。
		if verb == "download" {
			return "", nil
		}
		if h.verbErr != nil {
			return h.verbOut, h.verbErr
		}
		switch verb {
		case "install", "upgrade":
			h.installed[last] = true
		case "remove":
			h.installed[last] = false
		}
		return "", nil
	}
	return "", errors.New("fakeHost: unexpected command " + name)
}

// managerBin 返回该宿主的包管理器绝对路径。
func (h *fakeHost) managerBin() string {
	if h.manager == "apt" {
		return "/usr/bin/apt-get"
	}
	return "/usr/bin/dnf"
}

// downloadedPkg 返回最近一次模拟下载的包名;没发生过下载时给一个固定的
// 无害名字(清单本身是不是要害由用例决定,这里只要不命中要害)。
func (h *fakeHost) downloadedPkg() string {
	if h.lastDownloadPkg != "" {
		return h.lastDownloadPkg
	}
	return "placeholder"
}

// isVersionArgv 判定一次外部命令是不是"取实际版本"的查询。
//
// 形状与生产实现一一对应:apt 用 dpkg-query -W -f=${Version},
// dnf 用 rpm -q --qf %{VERSION}-%{RELEASE}。
func isVersionArgv(name string, args []string) bool {
	switch name {
	case "dpkg-query":
		return len(args) == 3 && args[0] == "-W" && args[1] == "-f=${Version}"
	case "rpm":
		return len(args) == 4 && args[0] == "-q" && args[1] == "--qf" && args[2] == "%{VERSION}-%{RELEASE}"
	}
	return false
}

// versionQueryCount 数"取实际版本"的查询次数。
func (h *fakeHost) versionQueryCount() int {
	n := 0
	for _, c := range h.calls {
		if isVersionArgv(c[0], c[1:]) {
			n++
		}
	}
	return n
}

// lastManagerCall 返回最后一次调用包管理器**变更动词**的 argv。
//
// 不能用"最后一次调用":句柄捕获会在变更之后追加一次历史查询,那不是变更。
func (h *fakeHost) lastManagerCall() []string {
	for i := len(h.calls) - 1; i >= 0; i-- {
		if h.calls[i][0] == h.managerBin() {
			return h.calls[i]
		}
	}
	return nil
}

// withFakeHost 把 runCmd / runCmdInDir / fileExists / readFile 四个缝换成 h,
// 并在测试结束时还原。不 t.Parallel:它们是包级变量。
func withFakeHost(t *testing.T, h *fakeHost) {
	t.Helper()
	prevRun, prevRunInDir, prevExists, prevRead := runCmd, runCmdInDir, fileExists, readFile
	t.Cleanup(func() { runCmd, runCmdInDir, fileExists, readFile = prevRun, prevRunInDir, prevExists, prevRead })
	runCmd = h.run
	runCmdInDir = func(ctx context.Context, dir, name string, args ...string) (string, error) {
		h.dirCalls = append(h.dirCalls, dirCall{dir: dir, name: name, args: append([]string{}, args...)})
		out, err := h.run(ctx, name, args...)
		if err != nil {
			return out, err
		}
		// 模拟宿主把包下载进工作目录:成功就放一个占位包文件,供后续
		// dpkg-deb -c / rpm -qlp 列出内容。失败时不落文件 —— 那是
		// "工具在,但这次查不到"的形状。
		if len(args) > 0 && args[0] == "download" {
			pkg := args[len(args)-1]
			h.lastDownloadPkg = pkg
			ext := ".deb"
			if strings.HasSuffix(name, "dnf") {
				ext = ".rpm"
			}
			if werr := os.WriteFile(filepath.Join(dir, pkg+ext), []byte("fake-package"), 0o644); werr != nil {
				return "", werr
			}
		}
		return out, nil
	}
	fileExists = func(p string) bool { return p == h.managerBin() }
	readFile = func(string) ([]byte, error) {
		if h.aptLogErr != nil {
			return nil, h.aptLogErr
		}
		return []byte(h.aptLog), nil
	}
}

// pkgIntent 造一条 package intent。RiskClass 显式给 LOW:落点体检只对
// **未被判 high** 的 intent 生效,不给就是 UNSPECIFIED —— 语义含混,而
// 且会让"高危跳过体检"的用例失去对照。
func pkgIntent(spec, name string) *agentv1.ApplyIntentRequest {
	return &agentv1.ApplyIntentRequest{
		IntentId:  "i-1",
		Kind:      kindPackage,
		Target:    name,
		SpecJson:  spec,
		RiskClass: agentv1.RiskClass_RISK_CLASS_LOW,
	}
}

// 装了就要调宿主的包管理器;argv 必须是该管理器的原生装包形态。
func TestPackage_PresentCallsHostManager(t *testing.T) {
	cases := []struct {
		manager string
		want    []string
	}{
		{"apt", []string{"/usr/bin/apt-get", "install", "-y", "nginx"}},
		{"dnf", []string{"/usr/bin/dnf", "install", "-y", "nginx"}},
	}
	for _, tc := range cases {
		t.Run(tc.manager, func(t *testing.T) {
			h := &fakeHost{manager: tc.manager, installed: map[string]bool{}}
			withFakeHost(t, h)

			if err := Apply(context.Background(), pkgIntent(`{"desired_state":"present"}`, "nginx")); err != nil {
				t.Fatalf("Apply: unexpected err: %v", err)
			}
			got := h.lastManagerCall()
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("argv = %v, got %v, want %v", got, got, tc.want)
			}
		})
	}
}

// absent 且已装 → 调管理器卸载。
func TestPackage_AbsentCallsRemove(t *testing.T) {
	cases := []struct {
		manager string
		want    []string
	}{
		{"apt", []string{"/usr/bin/apt-get", "remove", "-y", "nginx"}},
		{"dnf", []string{"/usr/bin/dnf", "remove", "-y", "nginx"}},
	}
	for _, tc := range cases {
		t.Run(tc.manager, func(t *testing.T) {
			h := &fakeHost{manager: tc.manager, installed: map[string]bool{"nginx": true}}
			withFakeHost(t, h)

			if err := Apply(context.Background(), pkgIntent(`{"desired_state":"absent"}`, "nginx")); err != nil {
				t.Fatalf("Apply: unexpected err: %v", err)
			}
			got := h.lastManagerCall()
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("argv = %v, got %v, want %v", got, got, tc.want)
			}
		})
	}
}

// 三值( present | absent | latest )之外的值必须拒绝,且不可重试。
func TestPackage_UnknownDesiredStateIsPersistent(t *testing.T) {
	h := &fakeHost{manager: "apt", installed: map[string]bool{}}
	withFakeHost(t, h)

	err := Apply(context.Background(), pkgIntent(`{"desired_state":"totally-bogus"}`, "nginx"))
	if !errors.Is(err, ErrPersistent) {
		t.Fatalf("got %v, want ErrPersistent", err)
	}
}

// 管理器回「无此包」→ 不可重试(重试一百次也不会出现这个包)。
func TestPackage_MissingPackageIsPersistent(t *testing.T) {
	cases := []struct {
		manager string
		out     string
	}{
		{"apt", "E: Unable to locate package nope\n"},
		{"dnf", "Error: Unable to find a match: nope\n"},
	}
	for _, tc := range cases {
		t.Run(tc.manager, func(t *testing.T) {
			h := &fakeHost{manager: tc.manager, installed: map[string]bool{}, verbOut: tc.out, verbErr: errors.New("exit status 100")}
			withFakeHost(t, h)

			err := Apply(context.Background(), pkgIntent(`{"desired_state":"present"}`, "nope"))
			if !errors.Is(err, ErrPersistent) {
				t.Fatalf("got %v, want ErrPersistent", err)
			}
		})
	}
}

// 包管理器锁被占 → 可重试。
func TestPackage_LockHeldIsTransient(t *testing.T) {
	cases := []struct {
		manager string
		out     string
	}{
		{"apt", "E: Could not get lock /var/lib/dpkg/lock-frontend. It is held by process 1234 (apt-get)\n"},
		{"dnf", "Error: Failed to obtain the transaction lock (logged in as: root).\n"},
	}
	for _, tc := range cases {
		t.Run(tc.manager, func(t *testing.T) {
			h := &fakeHost{manager: tc.manager, installed: map[string]bool{}, verbOut: tc.out, verbErr: errors.New("exit status 100")}
			withFakeHost(t, h)

			err := Apply(context.Background(), pkgIntent(`{"desired_state":"present"}`, "nginx"))
			if !errors.Is(err, ErrTransient) {
				t.Fatalf("got %v, want ErrTransient", err)
			}
		})
	}
}

// 幂等:同 intent 连投两次,第二次观测到已到位即 no-op(不再调管理器变更动词)。
func TestPackage_IsIdempotent(t *testing.T) {
	h := &fakeHost{manager: "apt", installed: map[string]bool{}}
	withFakeHost(t, h)

	intent := pkgIntent(`{"desired_state":"present"}`, "nginx")
	if err := Apply(context.Background(), intent); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if err := Apply(context.Background(), intent); err != nil {
		t.Fatalf("second Apply: unexpected err: %v", err)
	}

	// 只数**变更**调用:落点体检的 download 不算变更,第二次 apply 也不该再下载
	// (已装 → 本地清单)。
	var changes int
	for _, c := range h.calls {
		if c[0] == "/usr/bin/apt-get" && mutatingCall(c) {
			changes++
		}
	}
	if changes != 1 {
		t.Errorf("apt-get 变更调用 = %d, want 1 (second apply must be a no-op)", changes)
	}
}

// 未声明 desired_state 时按 present(安装)处理:Plan 层的 package 意图
// 语义就是"把这个包装上",而 v1 的冻结三值里 present 是其自然默认。
func TestPackage_MissingDesiredStateDefaultsToPresent(t *testing.T) {
	h := &fakeHost{manager: "dnf", installed: map[string]bool{}}
	withFakeHost(t, h)

	if err := Apply(context.Background(), pkgIntent(`{"version":"1.24.0"}`, "nginx")); err != nil {
		t.Fatalf("Apply: unexpected err: %v", err)
	}
	got := h.lastManagerCall()
	want := []string{"/usr/bin/dnf", "install", "-y", "nginx"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v, got %v, want %v", got, got, want)
	}
}

// key 拼错(例如 desired_stat)不得静默 fail-open 成 install:未知键即拒。
func TestPackage_TypoKeyIsPersistent(t *testing.T) {
	h := &fakeHost{manager: "apt", installed: map[string]bool{}}
	withFakeHost(t, h)

	err := Apply(context.Background(), pkgIntent(`{"desired_stat":"absent"}`, "nginx"))
	if !errors.Is(err, ErrPersistent) {
		t.Fatalf("got %v, want ErrPersistent", err)
	}
	for _, c := range h.calls {
		if c[0] == "/usr/bin/apt-get" {
			t.Errorf("mutating call %v must not happen for an unparseable spec", c)
		}
	}
}

// absent 也要幂等:已卸载时再投一次不得再调卸载。
func TestPackage_AbsentIsIdempotent(t *testing.T) {
	h := &fakeHost{manager: "dnf", installed: map[string]bool{}}
	withFakeHost(t, h)

	intent := pkgIntent(`{"desired_state":"absent"}`, "nginx")
	for i := 0; i < 2; i++ {
		if err := Apply(context.Background(), intent); err != nil {
			t.Fatalf("Apply #%d: unexpected err: %v", i+1, err)
		}
	}
	for _, c := range h.calls {
		if c[0] == "/usr/bin/dnf" {
			t.Errorf("mutating call %v must not happen: already absent", c)
		}
	}
}

// 网络类失败可重试(DNS / 连接层)。
func TestPackage_NetworkFailureIsTransient(t *testing.T) {
	h := &fakeHost{
		manager:   "apt",
		installed: map[string]bool{},
		verbOut:   "Err:1 http://archive.example.test/... Temporary failure resolving 'archive.example.test'\n",
		verbErr:   errors.New("exit status 100"),
	}
	withFakeHost(t, h)

	err := Apply(context.Background(), pkgIntent(`{"desired_state":"present"}`, "nginx"))
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("got %v, want ErrTransient", err)
	}
}

// 已取消的 ctx:必须在发变更命令前中止。
func TestPackage_CancelledContextAborts(t *testing.T) {
	h := &fakeHost{manager: "apt", installed: map[string]bool{}}
	withFakeHost(t, h)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Apply(ctx, pkgIntent(`{"desired_state":"present"}`, "nginx"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	for _, c := range h.calls {
		if c[0] == "/usr/bin/apt-get" {
			t.Errorf("mutating call %v must not happen after cancellation", c)
		}
	}
}

// 口径 C:实际版本的主干与声明版本相符 → apply 成功。
// 发行版后缀(-2ubuntu7.19 / -1.el9)必须被裁掉后再逐字比较。
func TestPackage_DeclaredVersionMatchesTrunk(t *testing.T) {
	cases := []struct {
		manager string
		actual  string
	}{
		{"apt", "1.24.0-2ubuntu7.19"},
		{"dnf", "1.24.0-1.el9"},
	}
	for _, tc := range cases {
		t.Run(tc.manager, func(t *testing.T) {
			h := &fakeHost{
				manager:   tc.manager,
				installed: map[string]bool{},
				versions:  map[string]string{"nginx": tc.actual},
			}
			withFakeHost(t, h)

			if err := Apply(context.Background(), pkgIntent(`{"desired_state":"present","version":"1.24.0"}`, "nginx")); err != nil {
				t.Fatalf("Apply: unexpected err: %v", err)
			}
			if n := h.versionQueryCount(); n != 1 {
				t.Errorf("version queries = %d, want 1 (apply 之后必须核对实际版本)", n)
			}
		})
	}
}

// 口径 C:主干不符 → ErrPersistent,且错误文案同时带上声明值与实际值。
// 不许把"装错了版本"报成成功,也不许自动降级/回滚。
func TestPackage_DeclaredVersionMismatchIsPersistent(t *testing.T) {
	h := &fakeHost{
		manager:   "apt",
		installed: map[string]bool{},
		versions:  map[string]string{"nginx": "1.22.1-9+deb12u10"},
	}
	withFakeHost(t, h)

	err := Apply(context.Background(), pkgIntent(`{"desired_state":"present","version":"1.24.0"}`, "nginx"))
	if !errors.Is(err, ErrPersistent) {
		t.Fatalf("got %v, want ErrPersistent", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "declared=1.24.0") {
		t.Errorf("err = %q, want it to contain declared=1.24.0", msg)
	}
	if !strings.Contains(msg, "actual=1.22.1-9+deb12u10") {
		t.Errorf("err = %q, want it to contain actual=1.22.1-9+deb12u10", msg)
	}
}

// 未声明 version → 不校验:行为与历史完全一致,连版本查询都不发。
func TestPackage_MissingVersionSkipsCheck(t *testing.T) {
	h := &fakeHost{
		manager:   "apt",
		installed: map[string]bool{},
		versions:  map[string]string{"nginx": "1.22.1-9+deb12u10"},
	}
	withFakeHost(t, h)

	if err := Apply(context.Background(), pkgIntent(`{"desired_state":"present"}`, "nginx")); err != nil {
		t.Fatalf("Apply: unexpected err: %v", err)
	}
	if n := h.versionQueryCount(); n != 0 {
		t.Errorf("version queries = %d, want 0 (未声明版本不得校验)", n)
	}
}

// desired_state=absent → 不校验版本(包本来就不该留在宿主上)。
func TestPackage_AbsentSkipsVersionCheck(t *testing.T) {
	h := &fakeHost{
		manager:   "apt",
		installed: map[string]bool{"nginx": true},
		versions:  map[string]string{"nginx": "1.22.1-9+deb12u10"},
	}
	withFakeHost(t, h)

	if err := Apply(context.Background(), pkgIntent(`{"desired_state":"absent","version":"1.24.0"}`, "nginx")); err != nil {
		t.Fatalf("Apply: unexpected err: %v", err)
	}
	if n := h.versionQueryCount(); n != 0 {
		t.Errorf("version queries = %d, want 0 (absent 不校验版本)", n)
	}
}

// 版本查询本身失败(非"未安装")→ ErrPersistent,如实说查不到实际版本;
// 既不许当成相符,也不许当成不符。
func TestPackage_VersionQueryFailureIsPersistent(t *testing.T) {
	h := &fakeHost{
		manager:    "apt",
		installed:  map[string]bool{},
		versionOut: "dpkg-query: error: unable to read status info\n",
		versionErr: errors.New("exit status 2"),
	}
	withFakeHost(t, h)

	err := Apply(context.Background(), pkgIntent(`{"desired_state":"present","version":"1.24.0"}`, "nginx"))
	if !errors.Is(err, ErrPersistent) {
		t.Fatalf("got %v, want ErrPersistent", err)
	}
	if !strings.Contains(err.Error(), "version check") {
		t.Errorf("err = %q, want it to say the version lookup failed", err.Error())
	}
}

// 幂等:同一个不符的 intent 重复投递,必须给出同一个结论 ——
// 第二次因为"已装"走 no-op,也不能把失败翻成成功。
func TestPackage_VersionMismatchIsStableAcrossRetries(t *testing.T) {
	h := &fakeHost{
		manager:   "dnf",
		installed: map[string]bool{},
		versions:  map[string]string{"nginx": "1.20.1-1.el9"},
	}
	withFakeHost(t, h)

	intent := pkgIntent(`{"desired_state":"present","version":"1.24.0"}`, "nginx")
	for i := 0; i < 2; i++ {
		err := Apply(context.Background(), intent)
		if !errors.Is(err, ErrPersistent) {
			t.Fatalf("Apply #%d: got %v, want ErrPersistent", i+1, err)
		}
		if !strings.Contains(err.Error(), "version mismatch") {
			t.Errorf("Apply #%d: err = %q, want version mismatch", i+1, err.Error())
		}
	}
}

// 主干裁剪规则本身:去 epoch,再在第一个 '-' / '+' / '~' 处截断。
// 这几条是口径 C 的比较基础,错了会让"同一上游版本"在别的发行版上误判。
func TestVersionTrunk(t *testing.T) {
	cases := []struct {
		actual string
		want   string
	}{
		{"1.24.0", "1.24.0"},
		{"1.24.0-2ubuntu7.19", "1.24.0"},
		{"1.22.1-9+deb12u10", "1.22.1"},
		{"1:2.4.0-1.el9", "2.4.0"},
		{"2.4.0~rc1-1", "2.4.0"},
	}
	for _, tc := range cases {
		if got := versionTrunk(tc.actual); got != tc.want {
			t.Errorf("versionTrunk(%q) = %q, want %q", tc.actual, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 落点体检(高危判定的权威兜底):低危包若会落到要害路径,装之前就拒绝。
// ---------------------------------------------------------------------------

// mutatingCall 报告一条命令是不是包管理器的**变更**动词。
//
// 不能用"调用过包管理器"来断言:落点体检自己也要调它(download),那不是变更。
func mutatingCall(c []string) bool {
	for _, a := range c[1:] {
		switch a {
		case "install", "upgrade", "remove":
			return true
		}
	}
	return false
}

// 低危包落到 /boot → 拒绝(ErrPersistent),且**不许**发出任何变更命令。
//
// 拒绝必须是不可重试的:高危变更失败后自动重试,正是这套兜底要防的事。
func TestPackage_LowRiskCriticalLandingRefused(t *testing.T) {
	h := &fakeHost{manager: "apt", installed: map[string]bool{"grub2-efi-x64": true}}
	h.replies = map[string]fakeReply{
		"dpkg-query -L grub2-efi-x64": {out: "/boot/efi/EFI/redhat/grubx64.efi\n/usr/sbin/grub2-mkimage\n"},
	}
	withFakeHost(t, h)

	err := Apply(context.Background(), pkgIntent(`{"desired_state":"latest"}`, "grub2-efi-x64"))
	if !errors.Is(err, ErrPersistent) {
		t.Fatalf("got %v, want ErrPersistent", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "critical path") {
		t.Errorf("err = %q, want 它点名 critical path", msg)
	}
	if !strings.Contains(msg, "/boot/efi/EFI/redhat/grubx64.efi") {
		t.Errorf("err = %q, want 它点名命中的路径", msg)
	}
	for _, c := range h.calls {
		if mutatingCall(c) {
			t.Errorf("变更命令 %v 不该发生(高危包必须装不下去)", c)
		}
	}
}

// 已判 high 的 intent → 不做落点体检(直接装)。
//
// 理由:atomic 节点上"有回滚点、人已批过"的高危变更是合法的;在这里二次
// 拒绝就与控制面准入冲突了。
func TestPackage_HighRiskSkipsLandingCheck(t *testing.T) {
	h := &fakeHost{manager: "apt", installed: map[string]bool{}}
	withFakeHost(t, h)

	intent := pkgIntent(`{"desired_state":"present"}`, "grub2-efi-x64")
	intent.RiskClass = agentv1.RiskClass_RISK_CLASS_HIGH
	if err := Apply(context.Background(), intent); err != nil {
		t.Fatalf("Apply: unexpected err: %v", err)
	}
	for _, c := range h.calls {
		if len(c) > 1 && (c[1] == "download" || c[1] == "-L" || c[1] == "-ql") {
			t.Errorf("高危 intent 不该查落点: %v", c)
		}
	}
	if len(h.dirCalls) != 0 {
		t.Errorf("高危 intent 不该下载: %+v", h.dirCalls)
	}
	if got := h.lastManagerCall(); strings.Join(got, " ") != "/usr/bin/apt-get install -y grub2-efi-x64" {
		t.Errorf("argv = %v, want 正常安装", got)
	}
}

// 工具在、但这次查不到落点 → 拒绝(fail-closed):判不出 ≠ 安全。
func TestPackage_UnknownLandingRefused(t *testing.T) {
	h := &fakeHost{manager: "apt", installed: map[string]bool{}}
	h.replies = map[string]fakeReply{
		"/usr/bin/apt-get download": {out: "E: Unable to locate package grub2-efi-x64\n", err: errors.New("exit status 100")},
	}
	withFakeHost(t, h)

	err := Apply(context.Background(), pkgIntent(`{"desired_state":"present"}`, "grub2-efi-x64"))
	if !errors.Is(err, ErrPersistent) {
		t.Fatalf("got %v, want ErrPersistent", err)
	}
	if !strings.Contains(err.Error(), "cannot determine landing paths") {
		t.Errorf("err = %q, want 它说清是判不出落点", err.Error())
	}
	for _, c := range h.calls {
		if mutatingCall(c) {
			t.Errorf("变更命令 %v 不该发生(判不出落点就不许装)", c)
		}
	}
}

// 工具缺失(dnf download 没装)→ **不拒绝**:跳过体检,继续装。
//
// 这是宿主环境问题,不是这次变更的问题;混为一谈会得到"要么整个发行版
// 不能装包,要么危险包照装"。
func TestPackage_ToolMissingProceeds(t *testing.T) {
	h := &fakeHost{manager: "dnf", installed: map[string]bool{}}
	h.replies = map[string]fakeReply{
		"/usr/bin/dnf download": {out: "Error: No such command: download\n", err: errors.New("exit status 1")},
	}
	withFakeHost(t, h)

	if err := Apply(context.Background(), pkgIntent(`{"desired_state":"present"}`, "nginx")); err != nil {
		t.Fatalf("工具缺失不该拒绝: %v", err)
	}
	if got := h.lastManagerCall(); strings.Join(got, " ") != "/usr/bin/dnf install -y nginx" {
		t.Errorf("argv = %v, want 正常安装", got)
	}
}

// desired_state=absent → 不查落点:卸载不新增文件,查落点没有意义。
func TestPackage_AbsentSkipsLandingCheck(t *testing.T) {
	h := &fakeHost{manager: "dnf", installed: map[string]bool{"nginx": true}}
	withFakeHost(t, h)

	if err := Apply(context.Background(), pkgIntent(`{"desired_state":"absent"}`, "nginx")); err != nil {
		t.Fatalf("Apply: unexpected err: %v", err)
	}
	for _, c := range h.calls {
		if len(c) > 1 && (c[1] == "download" || c[1] == "-L" || c[1] == "-ql") {
			t.Errorf("absent 不该查落点: %v", c)
		}
	}
	if got := h.lastManagerCall(); strings.Join(got, " ") != "/usr/bin/dnf remove -y nginx" {
		t.Errorf("argv = %v, want 卸载", got)
	}
}

// 升级路径(已装 + latest)→ 只走本地清单,**绝不联网**。
//
// 升级时仓库不可达是常事;若走去下载,合法的升级会被判成"判不出落点"
// 而拒绝。
func TestPackage_UpgradeUsesLocalListOnly(t *testing.T) {
	h := &fakeHost{manager: "dnf", installed: map[string]bool{"nginx": true}}
	h.replies = map[string]fakeReply{
		"rpm -ql nginx": {out: "/usr/sbin/nginx\n"},
	}
	withFakeHost(t, h)

	if err := Apply(context.Background(), pkgIntent(`{"desired_state":"latest"}`, "nginx")); err != nil {
		t.Fatalf("Apply: unexpected err: %v", err)
	}
	var sawQl bool
	for _, c := range h.calls {
		if c[0] == "rpm" && len(c) > 1 && c[1] == "-ql" {
			sawQl = true
		}
		if len(c) > 1 && c[1] == "download" {
			t.Errorf("已装包不许走网络: %v", c)
		}
	}
	if !sawQl {
		t.Errorf("没有查本地清单(rpm -ql)")
	}
	if len(h.dirCalls) != 0 {
		t.Errorf("已装包不该下载: %+v", h.dirCalls)
	}
	if got := h.lastManagerCall(); strings.Join(got, " ") != "/usr/bin/dnf upgrade -y nginx" {
		t.Errorf("argv = %v, want 升级", got)
	}
}
