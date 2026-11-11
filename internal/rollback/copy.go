package rollback

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// absentSuffix 是"快照时源本来不存在"的旁证文件名。单靠一份 .snap 无法
// 表达这个事实:还原时只能留一份空文件,而空文件和"原本没有这个文件"
// 是两种不同的宿主状态。
const absentSuffix = ".absent"

// CopyBackend 是纯 Go 的单文件快照后端。快照就是被改文件的一份副本,
// 不依赖宿主有没有 btrfs / rsync / lvm —— 那些回答的是"用什么手段还原",
// 不是"这次变更有没有还原点"。
//
// 只支持普通文件:目录会把同目录里无关的文件一起卷进还原范围,符号链接
// 会把读写都引到别处。两者都明确拒绝,不做部分支持。
type CopyBackend struct{}

var _ Backend = CopyBackend{}

// CreateSnapshot 把 sourcePath 复制成 snapshotPath,并保留 mode / 属主 /
// mtime。源不存在时写 <snapshotPath>.absent 标记,返回 nil —— "原本就
// 没有这个文件"同样是可还原的状态。
func (CopyBackend) CreateSnapshot(sourcePath, snapshotPath string) error {
	fi, err := os.Lstat(sourcePath)
	if errors.Is(err, os.ErrNotExist) {
		return markAbsent(sourcePath, snapshotPath)
	}
	if err != nil {
		return fmt.Errorf("rollback: inspect snapshot source %q: %w", sourcePath, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("rollback: snapshot source %q is not a regular file; refusing", sourcePath)
	}

	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o700); err != nil {
		return fmt.Errorf("rollback: create snapshot dir for %q: %w", snapshotPath, err)
	}
	if err := copyFile(sourcePath, snapshotPath); err != nil {
		return fmt.Errorf("rollback: snapshot %q -> %q: %w", sourcePath, snapshotPath, err)
	}
	if err := applyMeta(snapshotPath, fi); err != nil {
		return fmt.Errorf("rollback: snapshot metadata for %q: %w", snapshotPath, err)
	}
	// 同一 intent 重投时源可能又出现了,上一轮的 absent 标记必须清掉:
	// 两个互相矛盾的物证同时在场,还原时无从判断该信哪个。
	if err := os.Remove(snapshotPath + absentSuffix); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rollback: clear stale absent marker %q: %w", snapshotPath+absentSuffix, err)
	}
	return nil
}

// Restore 把快照还原到 targetPath。存在 absent 标记时删除 targetPath
// (存在才删),否则把快照内容拷回去并恢复 mode / 属主 / mtime。
func (CopyBackend) Restore(snapshotPath, targetPath string) error {
	marker := snapshotPath + absentSuffix
	if _, err := os.Lstat(marker); err == nil {
		if rerr := os.Remove(targetPath); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return fmt.Errorf("rollback: restore-to-absent could not remove %q: %w", targetPath, rerr)
		}
		if rerr := os.Remove(marker); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return fmt.Errorf("rollback: clear absent marker %q: %w", marker, rerr)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rollback: inspect absent marker %q: %w", marker, err)
	}

	fi, err := os.Lstat(snapshotPath)
	if err != nil {
		return fmt.Errorf("rollback: snapshot %q unusable: %w", snapshotPath, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("rollback: snapshot %q is not a regular file; refusing", snapshotPath)
	}

	// 快照与还原之间的时间窗足够把目标换成符号链接;O_TRUNC 会顺着链接
	// 写到别处的文件上,所以按 lstat 判类型,不给穿透的机会。
	if tfi, terr := os.Lstat(targetPath); terr == nil {
		if !tfi.Mode().IsRegular() {
			return fmt.Errorf("rollback: restore target %q is not a regular file; refusing", targetPath)
		}
	} else if !errors.Is(terr, os.ErrNotExist) {
		return fmt.Errorf("rollback: inspect restore target %q: %w", targetPath, terr)
	}

	if err := copyFile(snapshotPath, targetPath); err != nil {
		return fmt.Errorf("rollback: restore %q -> %q: %w", snapshotPath, targetPath, err)
	}
	if err := applyMeta(targetPath, fi); err != nil {
		return fmt.Errorf("rollback: restore metadata for %q: %w", targetPath, err)
	}
	return nil
}

// markAbsent 记录"源本来不存在"。顺手删掉同名的旧 .snap:留着它会让
// 之后的 Restore 看到两个互斥的物证。
func markAbsent(sourcePath, snapshotPath string) error {
	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o700); err != nil {
		return fmt.Errorf("rollback: create snapshot dir for %q: %w", snapshotPath, err)
	}
	if err := os.Remove(snapshotPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rollback: drop stale snapshot %q: %w", snapshotPath, err)
	}
	// 把源路径写进标记:运维看到孤儿标记时能立刻知道它对应哪个文件。
	if err := os.WriteFile(snapshotPath+absentSuffix, []byte(sourcePath+"\n"), 0o600); err != nil {
		return fmt.Errorf("rollback: write absent marker %q: %w", snapshotPath+absentSuffix, err)
	}
	return nil
}

// copyFile 用一次 open + io.Copy 复制内容,不 shell out 到 cp / rsync。
// 目标先以 0600 建出来,再由 applyMeta 把真实 mode 还回去 —— 中间不存在
// 一个"宽权限裸奔"的窗口。
//
// O_NOFOLLOW:目标路径若在检查之后被换成符号链接,O_TRUNC 会顺着链接写
// 到别处。把"不是链接"这个判断交给 open 本身,不留时间窗。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// applyMeta 把 fi 的属主 / mode / mtime 还原到 dst 上。
//
// 顺序不能换:chown 会清掉 setuid / setgid 位,先 chown 再 chmod 才能把
// 原 mode 原样还回去。chown 失败必须上抛 —— 非 root 下改不了属主时,
// 静默跳过会还原出一个"内容对、身份错"的文件,而没人知道。
func applyMeta(dst string, fi os.FileInfo) error {
	uid, gid, ok := ownerOf(fi)
	if !ok {
		return fmt.Errorf("cannot determine owner for %q; refusing to drop it silently", dst)
	}
	if err := os.Chown(dst, uid, gid); err != nil {
		return fmt.Errorf("preserve owner %d:%d: %w", uid, gid, err)
	}
	if err := os.Chmod(dst, fi.Mode()); err != nil {
		return fmt.Errorf("preserve mode %v: %w", fi.Mode(), err)
	}
	if err := os.Chtimes(dst, fi.ModTime(), fi.ModTime()); err != nil {
		return fmt.Errorf("preserve mtime: %w", err)
	}
	return nil
}

// ownerOf 从 stat 结果里取 uid/gid。取不到就是"无法保证属主",调用方
// 必须当错误处理,不能当作"无需保留"。
func ownerOf(fi os.FileInfo) (int, int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
