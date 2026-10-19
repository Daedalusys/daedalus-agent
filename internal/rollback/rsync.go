// rsync backend — the priority-2 rollback primitive on P0.5.
// RsyncBackend produces a file-copy "snapshot" by rsyncing the
// source directory into a target snapshot directory, and
// restores from such a snapshot by rsyncing back.
//
// Why this is the fallback (not the primary):
//
//   - File-copy is O(data) on CreateSnapshot — slower than
//     btrfs's O(metadata) copy-on-write.
//   - The snapshot is NOT atomic. A concurrent write to the
//     source between the mkdir and the rsync could be
//     partially captured. The tx layer surfaces this trade-off
//     in its WARN text (internal/tx/local.go) so an operator
//     sees "best-effort, not atomic" rather than "guaranteed
//     safe".
//   - Restore is also non-atomic: rsync removes files at the
//     target not present in the snapshot via --delete, which
//     means a partially-applied state is possible if rsync is
//     interrupted mid-transfer.
//
// Why we still ship it:
//
//   - Most Linux dataplane hosts don't run btrfs (they run
//     ext4 or xfs). Without rsync fallback, the agent has no
//     rollback path on those hosts and ApplyIntent must be
//     refused (RF3). The fallback lets us serve a much larger
//     surface area.
//   - File-copy snapshots are a well-understood operational
//     pattern — the warn surface makes the trade-off visible
//     to reviewers; a stricter atomic alternative (lvm-snap /
//     snapper) is tracked in daedalus-agent#10.
//
// Trailing-slash invariant:
//
//   Both CreateSnapshot and Restore use trailing-slash paths
//   (`<src>/` and `<snap>/`). Slipping this would copy
//   <src> as a subdirectory of <snap> rather than copying
//   <src>'s contents into <snap>. The tests in
//   rsync_test.go assert the trailing-slash form so a future
//   refactor that drops it gets caught.
package rollback

import (
	"fmt"
)

// RsyncBackend implements Backend with mkdir + `rsync -a
// --delete`. The same backend object can be reused across
// many CreateSnapshot / Restore calls; it carries no per-host
// state (file handles, persistent locks) in P0.5.
//
// Concurrency: RsyncBackend is *not* safe for concurrent use
// from multiple goroutines on the same host without external
// synchronisation — rsync uses temporary files (".~tmp~")
// inside the destination, and concurrent rsync invocations
// targeting the same <snap> or <target> directory will
// collide. The tx state machine serialises its own operations
// (Run is called once per ApplyIntent), and the gRPC server
// processes one ApplyIntent at a time per stream — so the
// backend does not need to be re-entrant in P0.5.
type RsyncBackend struct{}

// CreateSnapshot captures <sourcePath>'s contents into
// <snapshotPath> via:
//
//	mkdir -p <snapshotPath>
//	rsync -a --delete <sourcePath>/ <snapshotPath>/
//
// The trailing slash on the source / destination matters: with
// the slash, rsync copies the *contents* of <sourcePath> into
// <snapshotPath> (rather than copying <sourcePath> as a
// subdirectory of <snapshotPath>).
//
// --delete keeps the snapshot compact — files removed at the
// source between snapshots do not accumulate at the snapshot
// dir.
//
// Returns an error wrapping the first failing command (mkdir
// or rsync). On mkdir failure we do NOT invoke rsync: the
// snapshot dir is the rsync destination and must exist before
// rsync can write into it.
func (R *RsyncBackend) CreateSnapshot(sourcePath, snapshotPath string) error {
	// Step 1: ensure snapshot directory exists. mkdir -p is
	// idempotent (returns 0 if the directory already exists).
	if err := runCommand("mkdir", "-p", snapshotPath); err != nil {
		return fmt.Errorf("rollback: rsync setup mkdir -p %q: %w", snapshotPath, err)
	}
	// Step 2: copy source contents into snapshot dir.
	//
	// rsync uses trailing slashes on the source and dest
	// paths to mean "copy contents in" rather than "copy as
	// subdirectory".
	if err := runCommand("rsync", "-a", "--delete", sourcePath+"/", snapshotPath+"/"); err != nil {
		return fmt.Errorf("rollback: rsync -a --delete %q/ -> %q/: %w", sourcePath, snapshotPath, err)
	}
	return nil
}

// Restore replaces <targetPath>'s contents with <snapshotPath>'s
// contents via:
//
//	rsync -a --delete <snapshotPath>/ <targetPath>/
//
// --delete ensures files at <targetPath> that are absent from
// the snapshot are removed (i.e. partial state changes that
// the apply step introduced are unwound).
//
// We do NOT mkdir -p <targetPath> here: Restore assumes the
// target directory already exists (it always does in the tx
// state machine — applyStub ran in <targetPath> before the
// rollback fires). If a future caller passes a missing target,
// rsync will fail with a clear error and we wrap it.
//
// Returns an error wrapping the rsync exit status with both
// paths included for triage.
func (R *RsyncBackend) Restore(snapshotPath, targetPath string) error {
	if err := runCommand("rsync", "-a", "--delete", snapshotPath+"/", targetPath+"/"); err != nil {
		return fmt.Errorf("rollback: rsync -a --delete %q/ -> %q/ (restore): %w", snapshotPath, targetPath, err)
	}
	return nil
}
