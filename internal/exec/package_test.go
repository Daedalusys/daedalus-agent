package exec

import (
	"context"
	"errors"
	"strings"
	"testing"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// fakeHost 模拟一台装了 apt-get 或 dnf 的宿主。它替换 runCmd 缝:
// 查询类命令(dpkg-query / rpm)回报 installed 里的状态,变更类命令
// (apt-get / dnf)记录 argv 并在成功时翻转状态。
//
// verbOut / verbErr 让测试注入管理器输出与退出错误,用来驱动错误分类。
type fakeHost struct {
	manager   string // "apt" | "dnf"
	installed map[string]bool
	calls     [][]string
	verbOut   string
	verbErr   error
	// historyOut / historyErr 是历史查询(dnf history --json / apt
	// history-list)的回包;回滚句柄的 id 就从这里来。
	historyOut string
	historyErr error
}

func (h *fakeHost) run(_ context.Context, name string, args ...string) (string, error) {
	h.calls = append(h.calls, append([]string{name}, args...))
	last := ""
	if len(args) > 0 {
		last = args[len(args)-1]
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
		// 历史查询(dnf history --json / apt history-list):只读,不改状态。
		return h.historyOut, h.historyErr
	case "/usr/bin/apt-get", "/usr/bin/dnf":
		if h.verbErr != nil {
			return h.verbOut, h.verbErr
		}
		verb := ""
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				verb = a
				break
			}
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

// withFakeHost 把 runCmd / fileExists 两个缝换成 h,并在测试结束时还原。
// 不 t.Parallel:这两个是包级变量。
func withFakeHost(t *testing.T, h *fakeHost) {
	t.Helper()
	prevRun, prevExists := runCmd, fileExists
	t.Cleanup(func() { runCmd, fileExists = prevRun, prevExists })
	runCmd = h.run
	fileExists = func(p string) bool { return p == h.managerBin() }
}

func pkgIntent(spec, name string) *agentv1.ApplyIntentRequest {
	return &agentv1.ApplyIntentRequest{IntentId: "i-1", Kind: kindPackage, Target: name, SpecJson: spec}
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

	var installs int
	for _, c := range h.calls {
		if c[0] == "/usr/bin/apt-get" {
			installs++
		}
	}
	if installs != 1 {
		t.Errorf("apt-get invocations = %d, want 1 (second apply must be a no-op)", installs)
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
