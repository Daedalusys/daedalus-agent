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
// LVM-snap and snapper are out of scope for P0.5 (tracked in
// `daedalus-agent#10`). SelectBackend only returns a backend for
// SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME and
// SNAPSHOT_CAPABILITY_RSYNC_TARGET; other capabilities (NONE,
// UNSPECIFIED, the deferred LVM / snapper values) yield
// ErrNoBackend so callers can refuse early with a clear error.
//
// The package follows T4's hook-substitution convention: a
// package-level `runCommand` variable is the only seam tests
// need. Production code calls exec.Command directly through
// this variable; tests override it with a recording fake.
//
// RF3 surface (see plan §RF3 + spec §5.2):
//
//   SelectBackend(capability=NONE) returns (nil, ErrNoBackend).
//   The local tx state machine (internal/tx) treats ErrNoBackend
//   as an immediate refusal — no apply step, no fallback to
//   "best-effort", no silent success. The wording in the refusal
//   message is in internal/rpc/apply_intent.go (kept there so
//   the agent and control plane agree on the exact phrasing).
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

// Backend is the on-host snapshot / restore primitive the tx
// state machine consumes. The two P0.5 implementations are
// BtrfsBackend (subvolume-level, copy-on-write) and RsyncBackend
// (file-copy fallback). A future lvm-snap or snapper backend
// also satisfies this contract.
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

// SelectBackend returns the rollback Backend corresponding to a
// snapshot capability, or (nil, ErrNoBackend) if the capability
// is NONE / UNSPECIFIED / one of the deferred P0.6+ capabilities
// (LVM, SNAPPER).
//
// Callers MUST treat ErrNoBackend as an early-exit refusal —
// never fall through to a no-op apply, never substitute a
// "best-effort" wrapper (RF3).
func SelectBackend(capability agentv1.SnapshotCapability) (Backend, error) {
	switch capability {
	case agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME:
		return &BtrfsBackend{}, nil
	case agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET:
		return &RsyncBackend{}, nil
	case agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE,
		agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_UNSPECIFIED:
		return nil, ErrNoBackend
	default:
		// Deferred P0.6+ capabilities (lvm-snap, snapper) and
		// any future proto enum value fall into here. We refuse
		// rather than guess — an unknown backend should look
		// identical to NONE from the agent's perspective.
		return nil, fmt.Errorf("rollback: unsupported snapshot capability %v (deferred to P0.6+); refuse", capability)
	}
}
