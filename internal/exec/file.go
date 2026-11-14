package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/filespec"
)

// file.set 的 kind 字符串。上线的是**资源 kind**(objectmodel.Kind 的
// "file"),不是 core 风险判定表内部的适配器名。
const kindFile = "file"

// fileExecutor 实现 file kind。无状态,可复用。
type fileExecutor struct{}

// Apply 把 file intent 落到宿主文件系统上。
//
// 两种 spec 形态:
//
//   - 旧形态 {"content"}:target 是单个普通文件。只认普通文件 —— 符号链接
//     会把读写引到别处,目录不是文件;父目录必须已存在,建目录应是独立
//     intent。
//   - 目录形态 {"entries"}:target 是目录根,entries 逐条点名子路径。整批
//     先校验(spec.Validate),任何一条越界/类型不符都整批拒绝 —— 校验在
//     写第一条之前完成,才谈得上"失败可整批还原"。
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

	spec, err := filespec.Parse(intent.SpecJson)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPersistent, err)
	}

	if spec.IsLegacy() {
		return writeRegularFile(target, spec.Content)
	}

	if verr := spec.Validate(target); verr != nil {
		return fmt.Errorf("%w: %v", ErrPersistent, verr)
	}
	for _, entry := range spec.Entries {
		full := filepath.Join(target, entry.Path)
		switch entry.Op {
		case filespec.OpWrite:
			if werr := writeRegularFile(full, entry.Content); werr != nil {
				return werr
			}
		case filespec.OpRepoint:
			if rerr := repointSymlink(full, entry.SymlinkTarget); rerr != nil {
				return rerr
			}
		default:
			// filespec.Parse 已经挡过未知 op;真走到这里说明两处规则走偏了,
			// fail-closed 总比猜一个动作好。
			return fmt.Errorf("%w: unsupported entry op %q for %q", ErrPersistent, entry.Op, full)
		}
	}
	return nil
}

// writeRegularFile 用一个 open + write + chmod 完成覆盖,保留原 mode。
func writeRegularFile(target, content string) error {
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
	if _, werr := f.Write([]byte(content)); werr != nil {
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

// repointSymlink 把软链换到新的目标字符串。临时链接 + rename:先删后建
// 中间有一段"链接不存在"的窗口,读它的进程会看到另一个状态。
func repointSymlink(link, target string) error {
	dir := filepath.Dir(link)
	tmp, err := os.CreateTemp(dir, ".repoint-*")
	if err != nil {
		return fmt.Errorf("%w: prepare repoint of %q: %v", ErrPersistent, link, err)
	}
	tmpName := tmp.Name()
	tmp.Close()
	// CreateTemp 建出来的是普通文件,rename 之前必须让位给链接。
	if err := os.Remove(tmpName); err != nil {
		return fmt.Errorf("%w: prepare repoint of %q: %v", ErrPersistent, link, err)
	}
	if err := os.Symlink(target, tmpName); err != nil {
		return fmt.Errorf("%w: create symlink %q -> %q: %v", ErrPersistent, link, target, err)
	}
	if err := os.Rename(tmpName, link); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("%w: install symlink %q: %v", ErrPersistent, link, err)
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
