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

// 一次 intent 改目录里的多个子路径:每条 entry 各自落盘,互不影响。
func TestFile_WritesMultipleEntries(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "conf.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(dir, "conf.d", "b.conf")
	if err := os.WriteFile(existing, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := `{"entries":[` +
		`{"path":"conf.d/a.conf","op":"write","content":"A"},` +
		`{"path":"conf.d/b.conf","op":"write","content":"B"}` +
		`]}`
	if err := (fileExecutor{}).Apply(context.Background(), &agentv1.ApplyIntentRequest{
		Kind: "file", Target: dir, SpecJson: spec,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	gotA, err := os.ReadFile(filepath.Join(dir, "conf.d", "a.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotA) != "A" {
		t.Errorf("a.conf = %q, want A", gotA)
	}
	gotB, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotB) != "B" {
		t.Errorf("b.conf = %q, want B", gotB)
	}
	fi, _ := os.Stat(existing)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("覆盖后 mode = %v, want 0600(保留原 mode)", fi.Mode().Perm())
	}
}

// repoint 改的是软链本身:目标字符串换掉,链接不动。
func TestFile_RepointsSymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "current")
	if err := os.Symlink("../releases/1", link); err != nil {
		t.Fatal(err)
	}
	spec := `{"entries":[{"path":"current","op":"repoint","symlink_target":"../releases/2"}]}`
	if err := (fileExecutor{}).Apply(context.Background(), &agentv1.ApplyIntentRequest{
		Kind: "file", Target: dir, SpecJson: spec,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if got != "../releases/2" {
		t.Errorf("symlink target = %q, want ../releases/2", got)
	}
}

// 原本没有的链接可以新建。
func TestFile_RepointCreatesSymlink(t *testing.T) {
	dir := t.TempDir()
	spec := `{"entries":[{"path":"current","op":"repoint","symlink_target":"../releases/1"}]}`
	if err := (fileExecutor{}).Apply(context.Background(), &agentv1.ApplyIntentRequest{
		Kind: "file", Target: dir, SpecJson: spec,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.Readlink(filepath.Join(dir, "current"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if got != "../releases/1" {
		t.Errorf("symlink target = %q, want ../releases/1", got)
	}
}

// 越界路径不会被写出去,而且错误要说明是越界(不是别的原因碰巧失败)。
func TestFile_RejectsEscape(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "target")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := `{"entries":[{"path":"../escape.conf","op":"write","content":"x"}]}`
	err := (fileExecutor{}).Apply(context.Background(), &agentv1.ApplyIntentRequest{
		Kind: "file", Target: dir, SpecJson: spec,
	})
	if err == nil {
		t.Fatal("越界路径应被拒绝")
	}
	if !errors.Is(err, ErrPersistent) {
		t.Errorf("err = %v, want 裹 ErrPersistent", err)
	}
	if !strings.Contains(err.Error(), "escape") {
		t.Errorf("err = %q, want 说明越界", err)
	}
	if _, statErr := os.Stat(filepath.Join(parent, "escape.conf")); !os.IsNotExist(statErr) {
		t.Errorf("越界文件被写出来了, stat err = %v", statErr)
	}
}

// 旧 spec 的行为逐字不变:单个文件、保留原 mode、目录目标仍拒绝。
func TestFile_LegacyContentUnchanged(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "app.conf")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (fileExecutor{}).Apply(context.Background(), &agentv1.ApplyIntentRequest{
		Kind: "file", Target: target, SpecJson: `{"content":"new"}`,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "new" {
		t.Errorf("内容 = %q, want new", got)
	}
	fi, _ := os.Stat(target)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}

	if err := (fileExecutor{}).Apply(context.Background(), &agentv1.ApplyIntentRequest{
		Kind: "file", Target: dir, SpecJson: `{"content":"x"}`,
	}); err == nil {
		t.Fatal("旧 spec + 目录 target 仍应被拒绝")
	}
}
