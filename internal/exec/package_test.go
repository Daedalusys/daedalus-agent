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
			got := h.calls[len(h.calls)-1]
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
			got := h.calls[len(h.calls)-1]
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
	got := h.calls[len(h.calls)-1]
	want := []string{"/usr/bin/dnf", "install", "-y", "nginx"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v, got %v, want %v", got, got, want)
	}
}
