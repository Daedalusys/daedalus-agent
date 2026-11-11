package exec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

func TestFileExecutor_WritesNewFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "app.conf")
	err := fileExecutor{}.Apply(context.Background(), &agentv1.ApplyIntentRequest{
		Kind: "file", Target: target, SpecJson: `{"content":"hello"}`,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("内容 = %q, want hello", got)
	}
	fi, _ := os.Stat(target)
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("新建文件 mode = %v, want 0644", fi.Mode().Perm())
	}
}

func TestFileExecutor_OverwriteKeepsMode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "app.conf")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := fileExecutor{}.Apply(context.Background(), &agentv1.ApplyIntentRequest{
		Kind: "file", Target: target, SpecJson: `{"content":"new"}`,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "new" {
		t.Errorf("内容 = %q, want new", got)
	}
	fi, _ := os.Stat(target)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("覆盖后 mode = %v, want 0600(保留原 mode)", fi.Mode().Perm())
	}
}

func TestFileExecutor_RejectsDirectoryTarget(t *testing.T) {
	dir := t.TempDir()
	err := fileExecutor{}.Apply(context.Background(), &agentv1.ApplyIntentRequest{
		Kind: "file", Target: dir, SpecJson: `{"content":"x"}`,
	})
	if err == nil {
		t.Fatal("目录目标应被拒绝")
	}
	if !errors.Is(err, ErrPersistent) {
		t.Errorf("err = %v, want 裹 ErrPersistent", err)
	}
}

func TestFileExecutor_RejectsMissingParent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nope", "app.conf")
	err := fileExecutor{}.Apply(context.Background(), &agentv1.ApplyIntentRequest{
		Kind: "file", Target: target, SpecJson: `{"content":"x"}`,
	})
	if err == nil {
		t.Fatal("父目录不存在应被拒绝")
	}
}

// 符号链接不是普通文件:顺着它写会改到链接指向的别处,agent 不做这件事。
func TestFileExecutor_RejectsSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	elsewhere := filepath.Join(dir, "elsewhere.conf")
	if err := os.WriteFile(elsewhere, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.conf")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	err := fileExecutor{}.Apply(context.Background(), &agentv1.ApplyIntentRequest{
		Kind: "file", Target: link, SpecJson: `{"content":"x"}`,
	})
	if err == nil {
		t.Fatal("符号链接目标应被拒绝")
	}
	if !errors.Is(err, ErrPersistent) {
		t.Errorf("err = %v, want 裹 ErrPersistent", err)
	}
	got, _ := os.ReadFile(elsewhere)
	if string(got) != "do-not-touch" {
		t.Errorf("链接指向的文件被改了: %q", got)
	}
}

// 相对路径的目标含义不明(相对于 agent 进程的 CWD),拒绝而不是猜。
func TestFileExecutor_RejectsRelativeTarget(t *testing.T) {
	err := fileExecutor{}.Apply(context.Background(), &agentv1.ApplyIntentRequest{
		Kind: "file", Target: "app.conf", SpecJson: `{"content":"x"}`,
	})
	if err == nil {
		t.Fatal("相对路径目标应被拒绝")
	}
	if !errors.Is(err, ErrPersistent) {
		t.Errorf("err = %v, want 裹 ErrPersistent", err)
	}
}

// spec_json 拼错键名时必须拒:把 content 拼成 contents,若被忽略就会写出
// 一个空文件,而控制面以为内容已经落盘。
func TestFileExecutor_RejectsUnknownSpecKey(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "app.conf")
	err := fileExecutor{}.Apply(context.Background(), &agentv1.ApplyIntentRequest{
		Kind: "file", Target: target, SpecJson: `{"contents":"x"}`,
	})
	if err == nil {
		t.Fatal("未知 spec 键应被拒绝")
	}
	if !errors.Is(err, ErrPersistent) {
		t.Errorf("err = %v, want 裹 ErrPersistent", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("被拒的 intent 不该留下文件, stat err = %v", err)
	}
}
