package rollback

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// fakeRunner 替换 runCommand 缝:记录 argv,按命令注入失败。
type fakeRunner struct {
	mu    sync.Mutex
	calls []string
	fail  map[string]error

	// block 非 nil 时,每次调用先等它;started 用来告诉测试"第一发已经进来了"。
	block   chan struct{}
	started chan struct{}
	once    sync.Once
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{fail: map[string]error{}}
}

func (f *fakeRunner) run(name string, args ...string) error {
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.mu.Unlock()
	if f.started != nil {
		f.once.Do(func() { close(f.started) })
	}
	if f.block != nil {
		<-f.block
	}
	return f.fail[key]
}

func (f *fakeRunner) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func withFakeRun(t *testing.T, f *fakeRunner) {
	t.Helper()
	prev := runCommand
	t.Cleanup(func() { runCommand = prev })
	runCommand = f.run
}

// 原生撤销是白名单里的合法形状:必须真的执行它。
func TestApply_AllowsNativeUndo(t *testing.T) {
	fr := newFakeRunner()
	withFakeRun(t, fr)

	r := NewStepRunner()
	err := r.Run(context.Background(), []Step{{
		Kind:    StepNativeUndo,
		Command: "dnf history undo 42",
		Handle:  "dnf-history:42",
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := fr.recorded(); len(got) != 1 || got[0] != "dnf history undo 42" {
		t.Fatalf("calls = %v, want [dnf history undo 42]", got)
	}
}

// 销毁性步骤没标人审 → 直接拒绝,且一个命令都不执行。
func TestApply_RejectsUnmarkedDestructiveStep(t *testing.T) {
	fr := newFakeRunner()
	withFakeRun(t, fr)

	r := NewStepRunner()
	err := r.Run(context.Background(), []Step{{
		Kind:    StepFileDelete,
		Command: "rm -rf /var/lib/nginx",
		Handle:  "dnf-history:42",
		// HumanReview 漏标
	}})
	if !errors.Is(err, ErrDestructiveNeedsReview) {
		t.Fatalf("got %v, want ErrDestructiveNeedsReview", err)
	}
	if got := fr.recorded(); len(got) != 0 {
		t.Fatalf("calls = %v, want none: 未标人审的销毁步骤不得执行", got)
	}
}

// aptOwnedFake 伪造一台"nginx 已装"的宿主:执行期复核 rm 路径要重查
// apt 历史(拿包名)与 dpkg 的文件清单。
func aptOwnedFake() *fakeCommands {
	fc := newFakeCommands()
	fc.out["apt history-info 7"] = "Commandline: apt-get install nginx\n"
	fc.out["dpkg-query -L nginx"] = "/etc/nginx/nginx.conf\n/var/lib/nginx\n/usr/sbin/nginx\n"
	return fc
}

// 重查清单里有这个路径 + 标了人审 → 放行。
func TestApply_AllowsPackageOwnedRmAfterReview(t *testing.T) {
	fr := newFakeRunner()
	withFakeRun(t, fr)
	withFakeCommands(t, aptOwnedFake())

	r := NewStepRunner()
	err := r.Run(context.Background(), []Step{{
		Kind:        StepFileDelete,
		Command:     "rm -rf /var/lib/nginx",
		HumanReview: true,
		Handle:      "apt-history:7",
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := fr.recorded(); len(got) != 1 || got[0] != "rm -rf /var/lib/nginx" {
		t.Fatalf("calls = %v, want [rm -rf /var/lib/nginx]", got)
	}
}

// 路径不在**重查**到的清单里 → 拒绝。句柄是包管理器给的,不该能删包以外的文件。
func TestApply_RejectsNonPackageOwnedRm(t *testing.T) {
	fr := newFakeRunner()
	withFakeRun(t, fr)
	withFakeCommands(t, aptOwnedFake())

	r := NewStepRunner()
	err := r.Run(context.Background(), []Step{{
		Kind:        StepFileDelete,
		Command:     "rm -rf /etc/shadow",
		HumanReview: true,
		Handle:      "apt-history:7",
	}})
	if !errors.Is(err, ErrPathNotOwned) {
		t.Fatalf("got %v, want ErrPathNotOwned", err)
	}
	if got := fr.recorded(); len(got) != 0 {
		t.Fatalf("calls = %v, want none", got)
	}
}

// 步骤自己声明的 OwnedPaths 不参与判定:清单只有一个来源(按句柄重查)。
// 否则请求方只要声明一句"这些路径归我",就能删任何东西。
func TestApply_IgnoresStepDeclaredOwnedPaths(t *testing.T) {
	fr := newFakeRunner()
	withFakeRun(t, fr)
	withFakeCommands(t, aptOwnedFake())

	r := NewStepRunner()
	err := r.Run(context.Background(), []Step{{
		Kind:        StepFileDelete,
		Command:     "rm -rf /etc/shadow",
		HumanReview: true,
		Handle:      "apt-history:7",
		OwnedPaths:  []string{"/etc/shadow"}, // 请求方自带清单,不算数
	}})
	if !errors.Is(err, ErrPathNotOwned) {
		t.Fatalf("got %v, want ErrPathNotOwned", err)
	}
	if got := fr.recorded(); len(got) != 0 {
		t.Fatalf("calls = %v, want none", got)
	}
}

// 重查失败 → fail-closed:拿不到清单就不执行,不猜、不放行。
func TestApply_RejectsFileDeleteWhenOwnershipLookupFails(t *testing.T) {
	for _, key := range []string{
		"apt history-info 7",
		"dpkg-query -L nginx",
	} {
		t.Run(key, func(t *testing.T) {
			fr := newFakeRunner()
			withFakeRun(t, fr)
			fc := aptOwnedFake()
			fc.err[key] = errors.New("exit status 1")
			withFakeCommands(t, fc)

			r := NewStepRunner()
			err := r.Run(context.Background(), []Step{{
				Kind:        StepFileDelete,
				Command:     "rm -rf /var/lib/nginx",
				HumanReview: true,
				Handle:      "apt-history:7",
			}})
			if !errors.Is(err, ErrOwnershipUnverifiable) {
				t.Fatalf("got %v, want ErrOwnershipUnverifiable", err)
			}
			if got := fr.recorded(); len(got) != 0 {
				t.Fatalf("calls = %v, want none", got)
			}
		})
	}
}

// 句柄不是 apt 类 → 没有可证明的文件清单,拒执行删除步(fail-closed)。
// 即便步骤自带 OwnedPaths 也不行:那等于让调用方自己声明能删什么。
func TestApply_RejectsFileDeleteForNonAptHandle(t *testing.T) {
	fr := newFakeRunner()
	withFakeRun(t, fr)

	r := NewStepRunner()
	err := r.Run(context.Background(), []Step{{
		Kind:        StepFileDelete,
		Command:     "rm -rf /var/lib/nginx",
		HumanReview: true,
		Handle:      "dnf-history:42",
		OwnedPaths:  []string{"/var/lib/nginx"},
	}})
	if !errors.Is(err, ErrOwnershipUnverifiable) {
		t.Fatalf("got %v, want ErrOwnershipUnverifiable", err)
	}
	if got := fr.recorded(); len(got) != 0 {
		t.Fatalf("calls = %v, want none", got)
	}
}

// 第一步失败 → 立刻停,后面的步骤一个都不跑(不自动重试、不跳步继续)。
func TestApply_StopsOnFirstFailure(t *testing.T) {
	fr := newFakeRunner()
	fr.fail["systemctl start b.service"] = errors.New("exit status 1")
	withFakeRun(t, fr)

	r := NewStepRunner()
	err := r.Run(context.Background(), []Step{
		{Kind: StepNativeUndo, Command: "systemctl start a.service", Handle: "service-state:x"},
		{Kind: StepNativeUndo, Command: "systemctl start b.service", Handle: "service-state:x"},
		{Kind: StepNativeUndo, Command: "systemctl start c.service", Handle: "service-state:x"},
	})
	if !errors.Is(err, ErrStepFailed) {
		t.Fatalf("got %v, want ErrStepFailed", err)
	}
	want := []string{"systemctl start a.service", "systemctl start b.service"}
	if got := fr.recorded(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v, want %v (第三步不得执行)", got, want)
	}
}

// 同一个 handle 同时只允许一个 APPLY:第二个必须被拒,而不是排队等。
func TestApply_ConcurrentSameHandleRejected(t *testing.T) {
	fr := newFakeRunner()
	fr.block = make(chan struct{})
	fr.started = make(chan struct{})
	withFakeRun(t, fr)

	r := NewStepRunner()
	steps := []Step{{
		Kind:    StepNativeUndo,
		Command: "systemctl start nginx.service",
		Handle:  "service-state:nginx",
	}}

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background(), steps) }()
	<-fr.started

	if err := r.Run(context.Background(), steps); !errors.Is(err, ErrRollbackInFlight) {
		t.Fatalf("got %v, want ErrRollbackInFlight", err)
	}

	close(fr.block)
	if err := <-done; err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// 第一次跑完之后锁必须释放,同一个 handle 可以再来一次。
	if err := r.Run(context.Background(), steps); err != nil {
		t.Fatalf("Run after completion: %v", err)
	}
}

// 不在白名单里的命令形状 → 拒绝。
func TestApply_RejectsCommandOutsideWhitelist(t *testing.T) {
	fr := newFakeRunner()
	withFakeRun(t, fr)

	r := NewStepRunner()
	err := r.Run(context.Background(), []Step{{
		Kind:    StepNativeUndo,
		Command: "sh -c 'rm -rf /'",
		Handle:  "dnf-history:42",
	}})
	if !errors.Is(err, ErrCommandNotAllowed) {
		t.Fatalf("got %v, want ErrCommandNotAllowed", err)
	}
	if got := fr.recorded(); len(got) != 0 {
		t.Fatalf("calls = %v, want none", got)
	}
}

// MANUAL 步不执行(只给人看),但整条计划照样能 APPLY。
func TestApply_SkipsManualSteps(t *testing.T) {
	fr := newFakeRunner()
	withFakeRun(t, fr)

	r := NewStepRunner()
	err := r.Run(context.Background(), []Step{
		{Kind: StepPackageRemove, Command: "apt-get remove -y nginx", HumanReview: true, Handle: "apt-history:7"},
		{Kind: StepManual, Rationale: "人工核对 nginx 的残留配置", HumanReview: true, Handle: "apt-history:7"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := fr.recorded(); len(got) != 1 || got[0] != "apt-get remove -y nginx" {
		t.Fatalf("calls = %v, want 只执行 remove", got)
	}
}
