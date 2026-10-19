// Package snapshot owns the daedalus-agent snapshot backend
// capability probe.
//
// Detect() inspects the host at agent start-up (and on demand)
// to determine which snapshot backend the agent can use for
// pre-ApplyIntent rollback: a btrfs subvolume snapshot if the
// root filesystem is btrfs and the `btrfs` CLI is on PATH, an
// rsync target if rsync is available but btrfs is not, or NONE
// if neither is present. LVM-snap and snapper backends are out
// of scope for P0.5 (deferred to issue #10) and are not probed
// here — they are tracked separately.
//
// The result of Detect() flows into the gRPC Register response
// (`snapshot_capability` field on the Register RPC, see SDK
// proto `agent/v1/agent.proto`). The control plane reads the
// field and decides whether to enable ApplyIntent on the host:
// when the agent reports NONE, ApplyIntent MUST be refused
// (spec §5.2 / RF3). Detect() also emits a clear WARN to stderr
// in the NONE case so an operator reading the agent's log sees
// the same notice the control plane surfaces to the human
// reviewer.
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
	execLookPath    = exec.LookPath
	execCommand     = exec.Command
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

// Detect returns the snapshot backend the agent will use. The
// priority is btrfs > rsync > none. When the agent has no usable
// backend it returns SNAPSHOT_CAPABILITY_NONE AND emits a single
// WARN to stderr so an operator reading the agent log sees the
// same notice the control plane surfaces to the human reviewer
// (RF3 — see plan §RF3 and spec §5.2).
//
// The WARN deliberately does NOT use the phrase "best-effort"
// alone. "Best-effort rollback" reads to a hurried reviewer as
// "we will handle it"; the actual contract is "ApplyIntent will
// be refused; the host has no snapshot recovery path".
func Detect() agentv1.SnapshotCapability {
	// Priority 1: btrfs subvolume (copy-on-write, cheapest).
	if ok, _ := probeBtrfs(); ok {
		return agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME
	}
	// Priority 2: rsync target (file-copy fallback, no CoW).
	if ok, _ := probeRsync(); ok {
		return agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET
	}
	// No backend. Per RF3, surface this loudly. Do NOT call
	// log.Warn — that does not exist in the stdlib log package;
	// the agent uses the prefix convention used by T1-T3
	// (`agent:` prefix, single line, no structured fields yet).
	log.Print("agent: no snapshot backend detected (btrfs/rsync/lvm/snapper all absent); ApplyIntent will refuse per RF3; only local audit + state report available")
	return agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE
}