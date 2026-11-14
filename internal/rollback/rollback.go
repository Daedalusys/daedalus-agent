// Package rollback owns the on-host primitives the daedalus-agent
// uses to take a pre-ApplyIntent snapshot and to restore from it
// on failure.
//
// 后端分两层语义:
//
//   - copy 后端(CopyBackend)是生产路径。纯 Go 单文件副本,不依赖宿主
//     上的任何二进制;回滚点由 kind 决定(目前只有 file)。
//   - btrfs / rsync / lvm / snapper 是宿主自带机制,对应 BtrfsBackend /
//     RsyncBackend / lvmSnapBackend / snapperBackend。snapshot 包会探测并
//     上报它们,但只有 PointVolume 才把它们选进回滚路径;PointVolume 本期
//     没有 producer(卷级还原归 atomic 侧,见 daedalus-agent#8)。rsync
//     只复制文件,即便 PointVolume 也不选它。
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

// PathSetBackend 能一次留一组路径的副本。它是 Backend 之外的可选能力:
// 单路径后端(CopyBackend)不实现它,tx 因此可以按类型断言选分支,而不是
// 给现有接口硬塞一个用不上的方法。
type PathSetBackend interface {
	Backend
	CreateSnapshotSet(paths []string, snapshotPath string) error
	RestoreSet(snapshotPath string, paths []string) error
}

var _ PathSetBackend = (*TreeBackend)(nil)

// SelectBackend 把「回滚点形态 + 宿主能力」映射到具体的回滚后端。
//
// 两个输入的分工:
//
//   - Point 决定"这次变更有没有还原手段"。PointFile / PointTree 恒走内建
//     后端,与宿主能力无关 —— 把文件路径喂给 lvcreate 会被 lvm.go 的
//     /dev/ 守卫拒绝,进而让 LVM 宿主上的所有 file intent 直接坏掉。
//   - capability 只在 PointVolume 时有发言权:它回答"用哪种宿主手段还原"。
//
// PointNone 一律拒绝;PointVolume 上不认识的能力也拒绝,不猜:猜错会拿
// 错的后端去动宿主。rsync 只能复制文件,覆盖不了卷级状态,因此也不接受。
//
// tree 后端需要数据目录来定位快照集合;这里没有 dataDir 可传,生产路径必须
// 用 SelectBackendFor。本函数对 PointTree 仍返回一个 dataDir 为空的
// TreeBackend,它在真正建快照时会 fail-closed,而不是悄悄写到相对路径。
func SelectBackend(point Point, capability agentv1.SnapshotCapability) (Backend, error) {
	return SelectBackendFor(point, capability, "")
}

// SelectBackendFor 是 SelectBackend 的带数据目录版本:生产路径用这个,
// 让 tree 后端知道 GC 该扫哪里。
func SelectBackendFor(point Point, capability agentv1.SnapshotCapability, dataDir string) (Backend, error) {
	switch point {
	case PointFile:
		return &CopyBackend{}, nil
	case PointTree:
		return NewTreeBackend(dataDir), nil
	case PointVolume:
		switch capability {
		case agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_LVM_SNAP:
			return &lvmSnapBackend{}, nil
		case agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_SNAPSHOT_SNAPPER:
			return &snapperBackend{}, nil
		case agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME:
			return &BtrfsBackend{}, nil
		default:
			return nil, fmt.Errorf("rollback: volume rollback point needs a volume-capable host snapshot mechanism, got %v; refuse", capability)
		}
	case PointNone:
		return nil, fmt.Errorf("rollback: this kind has no rollback point; refuse")
	default:
		return nil, fmt.Errorf("rollback: unknown rollback point %v; refuse", point)
	}
}
