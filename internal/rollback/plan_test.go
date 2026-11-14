package rollback

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
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

// apt 版本不够(没有原生 undo)→ 退化成"卸载 + 人工核对副作用",且必须人审。
func TestPlan_AptFallbackNeedsReview(t *testing.T) {
	fc := newFakeCommands()
	fc.out["apt-get --version"] = "apt 2.4.11 (amd64)\n"
	fc.out["apt history-info 7"] = "Commandline: install nginx\n"
	withFakeCommands(t, fc)

	p, err := BuildPlan(context.Background(), "apt-history:7", allowAll{})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p.AutoOK {
		t.Error("apt 退化路径不该自动执行")
	}
	if len(p.Steps) == 0 {
		t.Fatal("steps 为空")
	}
	step := p.Steps[0]
	if step.Kind != StepPackageRemove {
		t.Errorf("Kind = %v, want StepPackageRemove", step.Kind)
	}
	if !step.HumanReview {
		t.Error("apt 退化路径必须人审")
	}
	if !strings.Contains(step.Command, "nginx") {
		t.Errorf("Command = %q, want 含包名 nginx", step.Command)
	}
	// 副作用(配置残留/postinst)只能给人看的说明步,不带命令。
	var manual bool
	for _, s := range p.Steps {
		if s.Kind == StepManual {
			manual = true
			if s.Command != "" {
				t.Errorf("MANUAL 步不该带命令, got %q", s.Command)
			}
			if !s.HumanReview {
				t.Error("MANUAL 步必须人审")
			}
		}
	}
	if !manual {
		t.Error("apt 退化路径必须带一条 MANUAL 步说明副作用")
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
