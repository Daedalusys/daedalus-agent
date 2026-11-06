package rollback

import (
	"fmt"
	"strings"
)

// lvmSnapshotSize 是快照 LV 的大小。LVM 快照要为写时复制预留空间;
// 1G 对配置文件级别的回滚够用,运维可按宿主实际卷大小调整。
var lvmSnapshotSize = "1G"

// lvmSnapBackend 用 LVM 快照做回滚点。
//
// 形状差异:LVM 快照按**逻辑卷**寻址,不是文件系统目录。所以这里的
// <sourcePath> / <targetPath> 是源 LV 的设备路径(如 /dev/vg0/root),
// <snapshotPath> 只是快照 LV 的命名来源(取 basename 净化成合法 LV 名)。
//
// 回滚时效提醒:`lvconvert --merge` 要等快照 LV 不再被占用才真正生效
// (通常需要卸载或重启)。这与 agent 的 "best-effort rollback, not bootc"
// 叙事一致 —— 它给的是尽力而为的恢复点,不是原子回滚。
type lvmSnapBackend struct{}

// CreateSnapshot 对源 LV 建一个 LVM 快照。
func (l *lvmSnapBackend) CreateSnapshot(sourcePath, snapshotPath string) error {
	name := lvmSnapshotName(snapshotPath)
	if err := runCommand("lvcreate", "--snapshot", "--name", name, "--size", lvmSnapshotSize, sourcePath); err != nil {
		return fmt.Errorf("rollback: lvcreate --snapshot %s -> %s: %w", sourcePath, name, err)
	}
	return nil
}

// Restore 把快照 LV 合并回源 LV。
func (l *lvmSnapBackend) Restore(snapshotPath, targetPath string) error {
	name := lvmSnapshotName(snapshotPath)
	if err := runCommand("lvconvert", "--merge", name); err != nil {
		return fmt.Errorf("rollback: lvconvert --merge %s (target %s): %w", name, targetPath, err)
	}
	return nil
}

// lvmSnapshotName 从 <snapshotPath> 派生一个合法的 LV 名:取 basename,
// 把 LVM 不接受的字符换成 '-',再加前缀避免与宿主机既有 LV 撞名。
func lvmSnapshotName(snapshotPath string) string {
	base := snapshotPath
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	base = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, base)
	if base == "" {
		return "daedalus-snap"
	}
	return "daedalus-" + base
}
