package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// file.set 的 kind 字符串。上线的是**资源 kind**(objectmodel.Kind 的
// "file"),不是 core 风险判定表内部的适配器名。
const kindFile = "file"

// fileSpec 是 file intent 的 SpecJson schema。MVP 只支持整体覆盖:按行
// 编辑或追加需要"当前内容是预期的"这一前提,agent 校验不了。
type fileSpec struct {
	Content string `json:"content"`
}

// fileExecutor 实现 file kind。无状态,可复用。
type fileExecutor struct{}

// Apply 把 file intent 落到宿主文件系统上。
//
// 只认普通文件:目录会把无关文件卷进还原范围,符号链接会把读写引到别处。
// 父目录必须已存在 —— 建目录应是独立 intent,隐式创建会让一次"写文件"
// 顺手改掉目录树。
func (fileExecutor) Apply(ctx context.Context, intent *agentv1.ApplyIntentRequest) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("exec: file.set %q cancelled: %w", intent.Target, ctxErr)
	}

	target := intent.Target
	// 目标由控制面给定,必须绝对:相对路径相对的是 agent 进程的 CWD,
	// 那是个不受控的目录。
	if !filepath.IsAbs(target) {
		return fmt.Errorf("%w: file.set target %q must be an absolute path", ErrPersistent, target)
	}

	var spec fileSpec
	dec := json.NewDecoder(strings.NewReader(intent.SpecJson))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return fmt.Errorf("%w: invalid file.set spec_json: %v", ErrPersistent, err)
	}

	mode, err := targetWriteMode(target)
	if err != nil {
		return err
	}

	// O_NOFOLLOW 让"目标不是符号链接"这个判断与打开是同一个原子动作:
	// 先 lstat 再 open 的写法在两步之间留了一个可被换链接的窗口。
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return fmt.Errorf("%w: open %q: %v", ErrPersistent, target, err)
	}
	if _, werr := f.Write([]byte(spec.Content)); werr != nil {
		f.Close()
		return fmt.Errorf("%w: write %q: %v", ErrPersistent, target, werr)
	}
	// 用 fd 上的 chmod 而不是按路径再来一次:新建时 O_CREATE 的 mode 会
	// 被 umask 削掉,不补这一步,"新建默认 0644"就是句空话。
	if cerr := f.Chmod(mode); cerr != nil {
		f.Close()
		return fmt.Errorf("%w: chmod %q: %v", ErrPersistent, target, cerr)
	}
	if cerr := f.Close(); cerr != nil {
		return fmt.Errorf("%w: close %q: %v", ErrPersistent, target, cerr)
	}
	return nil
}

// targetWriteMode 决定往 target 写用什么 mode,并在这里挡住不允许的目标
// 类型与缺失的父目录。
func targetWriteMode(target string) (os.FileMode, error) {
	fi, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		parent := filepath.Dir(target)
		pfi, perr := os.Stat(parent)
		if perr != nil {
			return 0, fmt.Errorf("%w: parent dir of %q is not usable: %v", ErrPersistent, target, perr)
		}
		if !pfi.IsDir() {
			return 0, fmt.Errorf("%w: parent of %q is not a directory", ErrPersistent, target)
		}
		return 0o644, nil
	}
	if err != nil {
		return 0, fmt.Errorf("%w: inspect %q: %v", ErrPersistent, target, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return 0, fmt.Errorf("%w: target %q is a symlink; refusing to write through it", ErrPersistent, target)
	}
	if !fi.Mode().IsRegular() {
		return 0, fmt.Errorf("%w: target %q is not a regular file", ErrPersistent, target)
	}
	// 覆盖保留原 mode:改内容和改权限是两件事。
	return fi.Mode().Perm(), nil
}
