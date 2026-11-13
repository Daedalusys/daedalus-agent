// snapper 后端的形状测试。
//
// snapper.go 在 Task 4 之前没有任何测试:它把快照号写进 sidecar、
// Restore 再读回来执行 undochange。这里钉住三件事 —— 命令形状、sidecar
// 内容、以及"拿不到号/号不合法时绝不猜"。
package rollback

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// create 的 stdout 是唯一可靠快照号来源:把它原样落进 sidecar,并断言
// 命令形状仍是 `snapper -c root create --type pre --description ...`。
func TestSnapperCreateWritesSidecarNumber(t *testing.T) {
	resetRunCommand(t)
	resetRunCommandOutput(t)

	var got recordedInvocation
	runCommandOutput = func(name string, args ...string) ([]byte, error) {
		got = recordedInvocation{name: name, args: append([]string(nil), args...)}
		return []byte("42\n"), nil
	}
	runCommand = func(string, ...string) error {
		t.Fatal("create 不该走 runCommand(答案在 stdout 里)")
		return nil
	}

	sidecar := filepath.Join(t.TempDir(), "snap.num")
	if err := (&snapperBackend{}).CreateSnapshot("/etc/hosts", sidecar); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	raw, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if string(raw) != "42\n" {
		t.Errorf("sidecar = %q, want %q", string(raw), "42\n")
	}
	if got.name != "snapper" {
		t.Errorf("命令 = %q, want snapper", got.name)
	}
	for _, want := range []string{"-c", "root", "create", "--type", "pre"} {
		if !containsToken(got.args, want) {
			t.Errorf("argv = %v, want 含 %q", got.args, want)
		}
	}
	if !containsToken(got.args, "daedalus pre-apply: /etc/hosts") {
		t.Errorf("argv = %v, description 必须点名被快照路径", got.args)
	}
}

// stdout 里没有可解析的号:报错,且绝不写 sidecar —— 写错号等于回滚时
// 打错目标。
func TestSnapperCreateRefusesNoNumber(t *testing.T) {
	resetRunCommand(t)
	resetRunCommandOutput(t)
	runCommandOutput = func(string, ...string) ([]byte, error) {
		return []byte("snapper: cannot create snapshot\nno number here\n"), nil
	}

	sidecar := filepath.Join(t.TempDir(), "snap.num")
	err := (&snapperBackend{}).CreateSnapshot("/etc/hosts", sidecar)
	if err == nil {
		t.Fatal("CreateSnapshot: nil err; want error when stdout has no number")
	}
	if _, statErr := os.Stat(sidecar); !os.IsNotExist(statErr) {
		t.Errorf("sidecar 不应存在, stat err = %v", statErr)
	}
}

// Restore 读 sidecar 的号,执行 `undochange <n>..0`。
func TestSnapperRestoreUndoesRange(t *testing.T) {
	resetRunCommand(t)

	var got recordedInvocation
	runCommand = func(name string, args ...string) error {
		got = recordedInvocation{name: name, args: append([]string(nil), args...)}
		return nil
	}

	sidecar := filepath.Join(t.TempDir(), "snap.num")
	if err := os.WriteFile(sidecar, []byte("42"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&snapperBackend{}).Restore(sidecar, "/etc/hosts"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got.name != "snapper" {
		t.Errorf("命令 = %q, want snapper", got.name)
	}
	if !containsToken(got.args, "undochange") || !containsToken(got.args, "42..0") {
		t.Errorf("argv = %v, want 含 undochange 42..0", got.args)
	}
}

// sidecar 不是数字:报错,且一个命令都不执行 —— 别把可疑值发给宿主。
func TestSnapperRestoreRejectsNonNumericSidecar(t *testing.T) {
	resetRunCommand(t)
	called := 0
	runCommand = func(string, ...string) error { called++; return nil }

	sidecar := filepath.Join(t.TempDir(), "snap.num")
	if err := os.WriteFile(sidecar, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := (&snapperBackend{}).Restore(sidecar, "/etc/hosts")
	if err == nil {
		t.Fatal("Restore: nil err; want error for non-numeric sidecar")
	}
	if called != 0 {
		t.Errorf("runCommand called %d times, want 0", called)
	}
	if !strings.Contains(err.Error(), "snapshot number") {
		t.Errorf("err = %q, want 说明 sidecar 里没有快照号", err)
	}
}
