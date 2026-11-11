// Package rollback owns the on-host primitives the daedalus-agent
// uses to take a pre-ApplyIntent snapshot and to restore from it
// on failure.
//
// 后端分两层语义:
//
//   - copy 后端(CopyBackend)是生产路径。纯 Go 单文件副本,不依赖宿主
//     上的任何二进制;回滚点由 kind 决定(目前只有 file)。
//   - btrfs / rsync / lvm / snapper 是宿主自带机制,对应 BtrfsBackend /
//     RsyncBackend / lvmSnapBackend / snapperBackend。它们的代码保留,
//     但 MVP 不再探测、不再选中(见 daedalus-agent#8):通用云镜像的根
//     文件系统是 ext4 / xfs,btrfs subvolume snapshot 又要求源本身是子卷。
//     把它们留在 SelectBackend 里是为了扩展时能接回来。
//
// 包内沿用 hook 替换惯例:多数测试只需替换包级 `runCommand`;另有
// `runCommandOutput` 覆盖"答案在 stdout 里"的命令(snapper 会打印刚建的
// 快照号)。生产代码一律经这两个变量调 exec.Command,测试替换成录制 fake。
package rollback

import (
	"fmt"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// runCommand is the package-level command-execution seam.
//
// Production: `exec.Command(name, args...).Run()` — i.e. wait
// for completion and return the exit-code error.
//
// Tests substitute runCommand with a recording fake so assertions
// cover both the command (name + argv) the backends generate AND
// the exit-code paths (success, failure, partial failure). The
// same pattern is used by internal/snapshot/capability.go — we
// keep the convention consistent across packages.
var runCommand = func(name string, args ...string) error {
	// Use exec.Command directly (not its package-level alias)
	// here — the indirection via exec.Command ensures we stay
	// on the standard library.
	c := newCmd(name, args...)
	return c.Run()
}

// runCommandOutput 是"答案在 stdout 里"的命令的注入缝(snapper 会打印
// 刚建的快照号)。与 internal/snapshot/capability.go 的同名缝保持同形,
// 免得多套惯例并存。
var runCommandOutput = func(name string, args ...string) ([]byte, error) {
	c := newCmd(name, args...)
	return c.Output()
}

// Backend is the on-host snapshot / restore primitive the tx
// state machine consumes.
//
// 路径语义按后端而异,调用方必须知情:
//
//   - BtrfsBackend / RsyncBackend 把 <sourcePath> / <snapshotPath> /
//     <targetPath> 都当**文件系统路径**。
//   - lvmSnapBackend 按**逻辑卷**寻址:<sourcePath> / <targetPath> 是
//     LV 设备路径(如 /dev/vg0/root),<snapshotPath> 只是快照 LV 的命名
//     来源,不是路径。
//   - snapperBackend 按**配置 + 快照号**寻址:<snapshotPath> 是承载快照号
//     的 sidecar 文件,<sourcePath> 只进描述文本。
//
// 换言之 copy 后端的 sourcePath / targetPath 都是**文件路径**(被改文件
// 本体与还原目标),btrfs / rsync 的也是路径,lvm/snapper 的不是;把
// lvm/snapper 接进生产路径之前必须先解决快照源的选择问题(见
// SelectBackend 的调用方)。
type Backend interface {
	// CreateSnapshot captures <sourcePath> into <snapshotPath>.
	// The semantics depend on the backend:
	//   - btrfs: <snapshotPath> becomes a copy-on-write subvolume
	//     of <sourcePath>. The snapshot is durable; the rollback
	//     machinery survives a process crash.
	//   - rsync: <snapshotPath> becomes a file-copy of
	//     <sourcePath>. NOT atomic (a concurrent write to
	//     <sourcePath> between the rsync invocations could be
	//     partially captured); the WARN text in internal/tx
	//     surfaces this trade-off.
	CreateSnapshot(sourcePath, snapshotPath string) error

	// Restore replaces the contents of <targetPath> with
	// <snapshotPath>. The semantics depend on the backend:
	//   - btrfs: <targetPath> is deleted (if it is a subvolume)
	//     and re-snapshotted from <snapshotPath>. There is a
	//     brief window where <targetPath> is absent.
	//   - rsync: <targetPath>'s contents are overwritten with
	//     <snapshotPath>'s contents via `rsync -a --delete`,
	//     which removes files at <targetPath> not present in
	//     <snapshotPath>.
	Restore(snapshotPath, targetPath string) error
}

// SelectBackend 把快照能力映射到具体的回滚后端。
//
// NONE / UNSPECIFIED 落到内建的 copy 后端:能力字段只描述"宿主自带什么
// 机制",而回滚点是按 kind 判的 —— 宿主没装 btrfs 不等于这次变更没有
// 还原手段。宿主自带机制(BTRFS / RSYNC / LVM / SNAPPER)的分支保持原样,
// 但生产路径不再走到它们(探测已停用),留作扩展。
//
// 未知枚举值(未来 proto 新增而本 agent 不认识的)仍然拒绝,不猜:猜错会
// 拿错的后端去动宿主。
func SelectBackend(capability agentv1.SnapshotCapability) (Backend, error) {
	switch capability {
	case agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME:
		return &BtrfsBackend{}, nil
	case agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET:
		return &RsyncBackend{}, nil
	case agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_LVM_SNAP:
		return &lvmSnapBackend{}, nil
	case agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_SNAPSHOT_SNAPPER:
		return &snapperBackend{}, nil
	case agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE,
		agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_UNSPECIFIED:
		return &CopyBackend{}, nil
	default:
		return nil, fmt.Errorf("rollback: unsupported snapshot capability %v; refuse", capability)
	}
}
