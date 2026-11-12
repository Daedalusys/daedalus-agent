// Package snapshot owns the daedalus-agent snapshot backend
// capability probe.
//
// Detect() 上报宿主自带的快照机制,优先级 snapper > lvm > btrfs > rsync > none。
// 这个字段只描述"宿主自带什么手段",不是"这次变更有没有还原点" —— 后者按
// kind 判,由内建的 copy 后端落地(见 internal/rollback 与 internal/tx)。
//
// The result of Detect() flows into the gRPC Register response
// (`snapshot_capability` field on the Register RPC, see SDK
// proto `agent/v1/agent.proto`). The control plane reads the
// field as "what host mechanism does this agent have"; it must
// not read it as "can this agent roll anything back".
package snapshot

import (
	"errors"
	"log"
	"os/exec"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// errNotFound is the sentinel produced by exec.LookPath when the
// binary is not present. We match against its message rather than
// using a typed pointer so tests that substitute a plain
// errors.New("not found") (no wrapping) still exercise the
// "binary is not installed" branch.
var errNotFound = exec.ErrNotFound

// execLookPath, execCommand and runCommandOutput are package-level
// indirection points so tests can substitute a deterministic
// binary lookup / command runner without touching the real host.
// In production these resolve to os/exec.LookPath, exec.Command
// and Command.Output respectively.
var (
	execLookPath     = exec.LookPath
	execCommand      = exec.Command
	runCommandOutput = func(name string, args ...string) ([]byte, error) {
		return execCommand(name, args...).Output()
	}
)

// probeBtrfs 是包级变量而非函数:测试把它整个换掉,就能在没有 btrfs 的
// 机器上验证优先级分派。
var probeBtrfs = detectBtrfs

// detectBtrfs reports whether the host has a usable btrfs
// subvolume snapshot path. It returns (true, nil) only when both
// (a) the `btrfs` binary is on PATH and (b) `btrfs subvolume
// show /` exits 0 with non-empty output — i.e. the root
// filesystem really is btrfs, not just a system that has the
// `btrfs-progs` package installed for tooling reasons.
//
// The second check is important: many hosts ship btrfs-progs for
// admin convenience without actually formatting any volume with
// btrfs. A naive PATH check would false-positive in that case and
// later mislead the control plane into issuing ApplyIntents
// against an unwritable snapshot backend.
func detectBtrfs() (bool, error) {
	if _, err := execLookPath("btrfs"); err != nil {
		// Not installed — that is a normal outcome, not an error.
		if errors.Is(err, errNotFound) {
			return false, nil
		}
		return false, nil
	}
	out, err := runCommandOutput("btrfs", "subvolume", "show", "/")
	if err != nil {
		// btrfs is installed but the root is not btrfs (or the
		// command is being denied by an LSM). Either way, no
		// usable subvolume — not an error worth surfacing.
		return false, nil
	}
	if len(out) == 0 {
		return false, nil
	}
	return true, nil
}

// probeRsync 是包级变量,理由同 probeBtrfs。
var probeRsync = detectRsync

// detectRsync reports whether the host has a usable rsync binary.
// The agent can use rsync as a file-copy snapshot fallback when
// btrfs is unavailable: slower than btrfs subvolume snapshots
// (no copy-on-write) but at least gives the rollback machinery
// something to restore from.
//
// `rsync --version` is used as the liveness probe because it is
// the cheapest way to confirm rsync is functional without
// requiring an actual remote / target. We do NOT use --version's
// exit code alone because a wrapper script could exit 0 while
// the underlying rsync binary is broken.
func detectRsync() (bool, error) {
	if _, err := execLookPath("rsync"); err != nil {
		if errors.Is(err, errNotFound) {
			return false, nil
		}
		return false, nil
	}
	out, err := runCommandOutput("rsync", "--version")
	if err != nil {
		return false, nil
	}
	if len(out) == 0 {
		return false, nil
	}
	return true, nil
}

// Detect 按 snapper > lvm > btrfs > rsync > none 的顺序上报宿主自带的
// 快照机制。
//
// 顺序是产品裁定而不是实现细节:snapper 已经在一个受管子卷上工作了,
// 再报 lvm 只会让控制面以为能从卷层整体还原;lvm 又比 btrfs 覆盖面广。
// 探测失败一律落到下一档,缺工具是正常结局,不是需要上报的错误。
func Detect() agentv1.SnapshotCapability {
	c := detect()
	log.Printf("agent: snapshot capability=%s", c)
	return c
}

func detect() agentv1.SnapshotCapability {
	if ok, _ := probeSnapper(); ok {
		return agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_SNAPSHOT_SNAPPER
	}
	if ok, _ := probeLVM(); ok {
		return agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_LVM_SNAP
	}
	if ok, _ := probeBtrfs(); ok {
		return agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME
	}
	if ok, _ := probeRsync(); ok {
		return agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET
	}
	return agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE
}
