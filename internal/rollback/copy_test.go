package rollback

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCopyBackend_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "f.conf")
	if err := os.WriteFile(src, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(dir, "s.snap")
	b := CopyBackend{}
	if err := b.CreateSnapshot(src, snap); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if err := os.WriteFile(src, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.Restore(snap, src); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v1" {
		t.Errorf("内容 = %q, want %q", got, "v1")
	}
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestCopyBackend_AbsentSourceRestoresToAbsent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "f.conf")
	snap := filepath.Join(dir, "s.snap")
	b := CopyBackend{}
	if err := b.CreateSnapshot(src, snap); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if err := os.WriteFile(src, []byte("later"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.Restore(snap, src); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("源本来不存在,还原后应仍不存在,stat err = %v", err)
	}
}

func TestCopyBackend_RejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	b := CopyBackend{}
	if err := b.CreateSnapshot(dir, filepath.Join(dir, "s.snap")); err == nil {
		t.Error("目录应被拒绝")
	}
}

// 符号链接不是普通文件:快照它等于给了一条指向别处的路径,还原时会把
// 内容写到链接指向的目标上。与 file 执行器同款 fail-closed(D6)。
func TestCopyBackend_RejectsSymlinkSource(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.conf")
	if err := os.WriteFile(real, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.conf")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	b := CopyBackend{}
	if err := b.CreateSnapshot(link, filepath.Join(dir, "s.snap")); err == nil {
		t.Error("符号链接应被拒绝")
	}
}

// 还原目标若已被换成符号链接,写回去会穿透链接改到别处的文件上 ——
// 快照与还原之间的时间窗足够换掉一个链接。拒绝,不做穿透写。
func TestCopyBackend_RestoreRejectsSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "f.conf")
	if err := os.WriteFile(src, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(dir, "s.snap")
	b := CopyBackend{}
	if err := b.CreateSnapshot(src, snap); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	elsewhere := filepath.Join(dir, "elsewhere.conf")
	if err := os.WriteFile(elsewhere, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, src); err != nil {
		t.Fatal(err)
	}

	if err := b.Restore(snap, src); err == nil {
		t.Error("符号链接目标应被拒绝")
	}
	got, err := os.ReadFile(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "do-not-touch" {
		t.Errorf("链接指向的文件被改了: %q", got)
	}
}

// 属主与 mtime 是还原的一部分:内容对了但属主变了,等于换了个用户身份
// 在跑这份配置。chown 失败必须报错(非 root 下改不了属主),不得静默跳过。
func TestCopyBackend_PreservesOwnerAndMtime(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "f.conf")
	if err := os.WriteFile(src, []byte("v1"), 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	wantUID, wantGID, ok := ownerOf(before)
	if !ok {
		t.Skip("取不到 uid/gid")
	}
	stamp := time.Unix(1600000000, 0)
	if err := os.Chtimes(src, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	snap := filepath.Join(dir, "s.snap")
	b := CopyBackend{}
	if err := b.CreateSnapshot(src, snap); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	snapInfo, err := os.Stat(snap)
	if err != nil {
		t.Fatal(err)
	}
	gotUID, gotGID, ok := ownerOf(snapInfo)
	if !ok {
		t.Skip("取不到 uid/gid")
	}
	if gotUID != wantUID || gotGID != wantGID {
		t.Errorf("快照属主 = %d:%d, want %d:%d", gotUID, gotGID, wantUID, wantGID)
	}
	if !snapInfo.ModTime().Equal(stamp) {
		t.Errorf("快照 mtime = %v, want %v", snapInfo.ModTime(), stamp)
	}

	// 还原后属主与 mtime 同样要跟着回去。
	if err := os.WriteFile(src, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.Restore(snap, src); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	after, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	afterUID, afterGID, ok := ownerOf(after)
	if !ok {
		t.Skip("取不到 uid/gid")
	}
	if afterUID != wantUID || afterGID != wantGID {
		t.Errorf("还原后属主 = %d:%d, want %d:%d", afterUID, afterGID, wantUID, wantGID)
	}
	if !after.ModTime().Equal(stamp) {
		t.Errorf("还原后 mtime = %v, want %v", after.ModTime(), stamp)
	}
}
