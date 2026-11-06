// Package rollback owns the on-host primitives the daedalus-agent
// uses to take a pre-ApplyIntent snapshot and to restore from it
// on failure.
//
// Two backends are implemented for P0.5:
//
//   - btrfs subvolume snapshot (priority 1 — copy-on-write,
//     cheapest). Wired through BtrfsBackend.
//   - rsync file-copy snapshot (priority 2 — slower, no CoW, but
//     still gives the rollback machinery something to restore
//     from). Wired through RsyncBackend.
//
// LVM-snap 与 snapper(daedalus-agent#8)分别由 lvmSnapBackend /
// snapperBackend 承担:SelectBackend 对 SNAPSHOT_CAPABILITY_LVM_SNAP 与
// SNAPSHOT_CAPABILITY_SNAPSHOT_SNAPPER 返回它们。把这两个能力探出来的
// 探测器在 internal/snapshot(NONE / UNSPECIFIED / 未知枚举仍返回
// ErrNoBackend,调用方据此尽早拒绝)。
//
// 包内沿用 hook 替换惯例:多数测试只需替换包级 `runCommand`;另有
// `runCommandOutput` 覆盖"答案在 stdout 里"的命令(snapper 会打印刚建的
// 快照号)。生产代码一律经这两个变量调 exec.Command,测试替换成录制 fake。
//
// RF3 surface (see plan §RF3 + spec §5.2):
//
//	SelectBackend(capability=NONE) returns (nil, ErrNoBackend).
//	The local tx state machine (internal/tx) treats ErrNoBackend
//	as an immediate refusal — no apply step, no fallback to
//	"best-effort", no silent success. The wording in the refusal
//	message is in internal/rpc/apply_intent.go (kept there so
//	the agent and control plane agree on the exact phrasing).
package rollback

import (
	"errors"
	"fmt"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// ErrNoBackend is returned by SelectBackend when the agent has no
// usable snapshot backend. Callers should refuse ApplyIntent /
// SnapshotRequest with the RF3 wording when they see this error.
// ErrNoBackend is matched with errors.Is — it is exported so
// internal/tx and the gRPC handlers can both check for it without
// sharing an import.
var ErrNoBackend = errors.New("rollback: no snapshot backend available (btrfs/rsync/lvm/snapper all absent)")

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
// 换言之 tx 目前算出的 <dataDir>/intent-target/<target> 只对 btrfs/rsync
// 有路径含义;把 lvm/snapper 接进来之前必须先解决快照源的选择问题(见
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

// SelectBackend 把快照能力映射到具体的回滚后端;能力是 NONE / UNSPECIFIED
// 时返回 (nil, ErrNoBackend)。未知枚举值(未来 proto 新增而本 agent 不认识
// 的)同样拒绝,不猜。
//
// 调用方**必须**把任何非 nil error 当尽早拒绝处理 —— 绝不落到 no-op
// apply,也绝不用一个 "best-effort" 包装蒙混过去(RF3)。
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
		return nil, ErrNoBackend
	default:
		// 未来 proto 新增的枚举值落到这里。拒绝而不是猜:从 agent 的
		// 视角,一个不认识的后端应当和 NONE 长得一样。
		return nil, fmt.Errorf("rollback: unsupported snapshot capability %v; refuse", capability)
	}
}
