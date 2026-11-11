// Package snapshot owns the daedalus-agent snapshot backend
// capability probe.
//
// Detect() 恒报 SNAPSHOT_CAPABILITY_NONE:这个字段只描述宿主自带的快照
// 机制(btrfs subvolume / rsync / LVM / snapper),而 agent 的回滚点按
// kind 定义、由内建的纯 Go copy 后端落地,与宿主能力无关。btrfs /
// rsync 的探测函数保留(probeBtrfs / probeRsync),但生产路径不再调用。
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

// probeBtrfs reports whether the host has a usable btrfs
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
func probeBtrfs() (bool, error) {
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

// probeRsync reports whether the host has a usable rsync binary.
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
func probeRsync() (bool, error) {
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

// Detect 恒报 NONE:宿主自带的快照机制不再参与回滚点判定。
//
// 能力字段回答的是"宿主自带什么手段",不是"这次变更有没有还原点" —— 后者
// 按 kind 判,由内建的 copy 后端落地(见 internal/rollback 与 internal/tx)。
// 在 ext4 / xfs 的通用镜像上探测 btrfs / rsync 只会得到一个与回滚能力无关
// 的答案,却容易被读成"这台机器能回滚一切"。
//
// probeBtrfs / probeRsync 保留给扩展(btrfs / rsync 后端接回来时复用),
// 生产路径不再调用它们;这里仍打一行日志,免得操作员把 NONE 读成"什么都
// 不能回滚"。
func Detect() agentv1.SnapshotCapability {
	log.Print("agent: snapshot capability reported as NONE by design (host snapshot mechanisms are not probed); rollback points are per-kind and use the built-in copy backend")
	return agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE
}
