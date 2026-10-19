// btrfs backend — the highest-priority rollback primitive on
// P0.5. BtrfsBackend takes a copy-on-write snapshot of a source
// path and restores the source from that snapshot by deleting
// the source and re-snapshotting the prior snapshot back into
// place.
//
// Why this is the priority backend:
//
//   - Copy-on-write means CreateSnapshot is O(1) on the host's
//     metadata cost (it does NOT copy file data; reads of
//     unmodified files stay on the source).
//   - Restore is fast (no full file copy).
//   - btrfs is widely supported on Linux dataplane hosts (the
//     ones daedalus targets in P0.5).
//
// Why this is *not* used directly in RFC-level code:
//
//   The btrfs CLI requires CAP_SYS_ADMIN to manage subvolumes
//   on most distros. The daedalus-agent runs as a non-root
//   user (the install.sh in T2 sets up a dedicated
//   daedalus-agent system user). Wrapping the btrfs calls in
//   a small sudo helper or setcap binary is out of scope for
//   P0.5 (tracked in followup). The Rust + Go backends are
//   exercised by the Step 7 真机 run, which runs as root in a
//   private container.
//
// Restoration semantics:
//
//   Restore is *near*-atomic, not atomic. There is a brief
//   window between `btrfs subvolume delete <target>` and
//   `btrfs subvolume snapshot <snap> <target>` where <target>
//   does not exist. A concurrent read during that window will
//   fail. We accept this trade-off for P0.5 — a fully atomic
//   restore would require btrfs send/receive into a fresh
//   subvolume + rename, which is meaningfully more complex and
//   not justified by the ApplyIntent failure window.
package rollback

import (
	"fmt"
	"os/exec"
)

// execCommand is an alias of exec.Command used by the
// production runCommand default. Defined as a package-level
// variable so a future test that wants to inspect the *exec.Cmd
// (rather than just run a fake function) can substitute it. T4's
// snapshot package uses the same pattern.
var execCommand = exec.Command

// newCmd is a small indirection point over exec.Command. Tests
// don't currently substitute it (runCommand is enough), but
// keeping the seam here means a future test can if needed
// without rewriting the backend.
func newCmd(name string, args ...string) *exec.Cmd {
	return execCommand(name, args...)
}

// BtrfsBackend implements Backend with `btrfs subvolume
// snapshot`. The same backend object can be reused across
// many CreateSnapshot / Restore calls — it carries no
// per-host state in P0.5 (no file handles, no persistent
// locks). Concurrency is the caller's concern: a single
// BtrfsBackend should not be invoked in parallel from
// multiple goroutines without synchronisation at the
// call-site (tx.Run() serialises its own apply state machine
// already; Restore at the btrfs level is also serialised).
type BtrfsBackend struct{}

// CreateSnapshot captures <sourcePath> into <snapshotPath> via
// `btrfs subvolume snapshot <sourcePath> <snapshotPath>`. The
// resulting <snapshotPath> is a copy-on-write snapshot: reads
// stay on the source until either side diverges.
//
// Returns an error wrapping the underlying btrfs failure
// (with both paths included so the tx layer can include them
// in the audit / error report).
func (B *BtrfsBackend) CreateSnapshot(sourcePath, snapshotPath string) error {
	if err := runCommand("btrfs", "subvolume", "snapshot", sourcePath, snapshotPath); err != nil {
		return fmt.Errorf("rollback: btrfs subvolume snapshot %q -> %q: %w", sourcePath, snapshotPath, err)
	}
	return nil
}

// Restore replaces <targetPath>'s contents with <snapshotPath>'s.
// The sequence is:
//
//  1. `btrfs subvolume delete <targetPath>` — idempotent: a
//     target that is not a subvolume causes this step to fail,
//     and we treat the failure as best-effort (the snapshot
//     step can still proceed and create the target fresh).
//  2. `btrfs subvolume snapshot <snapshotPath> <targetPath>` —
//     the snapshot becomes the new target.
//
// Both commands share the same error wrapping policy: any
// non-zero exit becomes a wrapped error carrying both paths
// for triage.
func (B *BtrfsBackend) Restore(snapshotPath, targetPath string) error {
	// Step 1: best-effort delete. We do NOT propagate the
	// error from delete to the caller — if the target wasn't
	// a subvolume (new path, regular directory, ext4 mount),
	// the snapshot step below can still succeed. P0.5 keeps
	// this best-effort: a strict-mode delete belongs to
	// followup if needed.
	if err := runCommand("btrfs", "subvolume", "delete", targetPath); err != nil {
		// Best-effort — fall through to the snapshot step.
		// We intentionally do not log here: the package
		// logger convention is one line per tx-level event,
		// not per command — the snapshot step's success or
		// failure carries the operational signal.
		_ = err
	}
	// Step 2: re-snapshot. Surface this error because the
	// rollback path is now broken if it fails.
	if err := runCommand("btrfs", "subvolume", "snapshot", snapshotPath, targetPath); err != nil {
		return fmt.Errorf("rollback: btrfs subvolume snapshot %q -> %q (restore): %w", snapshotPath, targetPath, err)
	}
	return nil
}
