//go:build realmachine

// 真机形状验证。这个文件只在真发行版容器里编译运行(CI 的
// package-manager-shapes job):单测里的样本是从这些容器抄回来的,但"抄回来
// 的样本"证明不了捕获与计划在真宿主上仍然对得上 —— 这里真装包、真读
// /var/log/apt/history.log、真跑 dnf history。
//
// 为什么放在 internal/rollback 而不是 internal/exec:capture* 不导出,而
// rollback 反向 import exec,包内测试 import rollback 会成 import cycle。
// 这里用 exec 的 realmachine 构建出口调 capture,再调 rollback.BuildPlan。
package rollback_test

import (
	"context"
	"os"
	osexec "os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	agentexec "github.com/Daedalusys/daedalus-agent/internal/exec"
	"github.com/Daedalusys/daedalus-agent/internal/rollback"
)

func TestRealPackageManager(t *testing.T) {
	switch {
	case pathExists("/usr/bin/apt-get"):
		testRealApt(t)
	case pathExists("/usr/bin/dnf"):
		testRealDnf(t)
	default:
		t.Skip("no apt-get or dnf on this host")
	}
}

// testRealApt 在 apt 镜像里真装 hello,再从日志捕获句柄并生成计划。
//
// 断言按 apt 版本分岔:≥3.2 必须走到原生 undo(否则交叉核对退化了),<3.2
// (Debian 13 的 3.0.4)必须给退化计划 —— 那条路径正是 apt 没有 history
// 子命令时唯一能走的路。
func testRealApt(t *testing.T) {
	runCmd(t, "apt-get", "update", "-qq")
	since := time.Now()
	runCmd(t, "apt-get", "install", "-y", "-qq", "--no-install-recommends", "hello")

	handle, err := agentexec.CaptureAptHandleForRealMachine(context.Background(), "hello", since)
	if err != nil {
		t.Fatalf("captureAptHandle: %v", err)
	}
	if !strings.HasPrefix(handle, "apt-log:") || !strings.HasSuffix(handle, ":hello") {
		t.Fatalf("handle = %q, want apt-log:<start-unix>:hello", handle)
	}

	plan, err := rollback.BuildPlan(context.Background(), handle, rollback.NewCommandAllower())
	if err != nil {
		t.Fatalf("BuildPlan(%q): %v", handle, err)
	}

	if aptHasHistory(t) {
		if !plan.AutoOK {
			t.Fatalf("apt ≥ 3.2 应给出可自动执行的原生 undo 计划, got %+v", plan.Steps)
		}
		if len(plan.Steps) != 1 || plan.Steps[0].Kind != rollback.StepNativeUndo {
			t.Fatalf("steps = %+v, want 单步 NATIVE_UNDO", plan.Steps)
		}
		if !strings.HasPrefix(plan.Steps[0].Command, "apt history-undo ") {
			t.Fatalf("Command = %q, want apt history-undo <idx>", plan.Steps[0].Command)
		}
		return
	}

	if plan.AutoOK {
		t.Fatalf("apt < 3.2 没有原生 undo,计划不该自动执行: %+v", plan.Steps)
	}
	if len(plan.Steps) == 0 || plan.Steps[0].Command != "apt-get remove -y hello" {
		t.Fatalf("steps = %+v, want 以 apt-get remove -y hello 开头", plan.Steps)
	}
	for _, s := range plan.Steps {
		if s.Kind == rollback.StepNativeUndo {
			t.Fatalf("apt < 3.2 不该出现原生撤销步: %+v", s)
		}
	}
}

// testRealDnf 用镜像自带历史里的 bash(dnf 镜像不联网装包):dnf4 的
// `history --json` 是 exit 2,句柄必须从 list/info 文本路径拿到。
func testRealDnf(t *testing.T) {
	handle, err := agentexec.CaptureDnfHandleForRealMachine(context.Background(), "bash", time.Unix(0, 0))
	if err != nil {
		t.Fatalf("captureDnfHandle: %v", err)
	}
	if !strings.HasPrefix(handle, "dnf-history:") {
		t.Fatalf("handle = %q, want dnf-history:<id>", handle)
	}
	id := strings.TrimPrefix(handle, "dnf-history:")

	plan, err := rollback.BuildPlan(context.Background(), handle, rollback.NewCommandAllower())
	if err != nil {
		t.Fatalf("BuildPlan(%q): %v", handle, err)
	}
	if !plan.AutoOK || len(plan.Steps) != 1 || plan.Steps[0].Kind != rollback.StepNativeUndo {
		t.Fatalf("plan = %+v, want 单步 NATIVE_UNDO", plan)
	}
	if want := "dnf history undo " + id; plan.Steps[0].Command != want {
		t.Fatalf("Command = %q, want %q", plan.Steps[0].Command, want)
	}
}

// aptHasHistory 报告宿主 apt 是否自带 history 子命令(≥ 3.2)。
func aptHasHistory(t *testing.T) bool {
	t.Helper()
	out, err := osexec.Command("apt-get", "--version").Output()
	if err != nil {
		t.Fatalf("apt-get --version: %v", err)
	}
	m := regexp.MustCompile(`(?m)^apt\s+(\d+)\.(\d+)`).FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("cannot parse apt version from %q", out)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return major > 3 || (major == 3 && minor >= 2)
}

func runCmd(t *testing.T, name string, args ...string) {
	t.Helper()
	out, err := osexec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
