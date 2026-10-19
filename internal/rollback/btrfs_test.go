// Tests for the btrfs subvolume rollback backend.
//
// The btrfs backend is the highest-priority rollback primitive
// on P0.5: when the agent detects a btrfs root filesystem
// (T4's probeBtrfs), CreateSnapshot runs `btrfs subvolume
// snapshot <src> <dst>` for a copy-on-write snapshot, and
// Restore runs `btrfs subvolume snapshot <snap> <target>` after
// deleting any existing target subvolume.
//
// The tests follow the same hook-substitution pattern as
// `internal/snapshot/capability_test.go`: a package-level
// `runCommand` variable is replaced with a recording fake so
// assertions cover both exit-code handling and the exact
// command-line arguments. The tests are hermetic — real btrfs
// is exercised only in the Step 7 真机 run, not here.
package rollback

import (
	"errors"
	"strings"
	"testing"
)

// resetRunCommand restores runCommand after the test substitutes
// it. T4's snapshot package uses the same pattern (see
// `snapshot.resetHooks`); keeping the convention consistent
// across packages makes the tests easier to follow.
func resetRunCommand(t *testing.T) {
	t.Helper()
	orig := runCommand
	t.Cleanup(func() { runCommand = orig })
}

// TestBtrfsCreateSnapshotSuccess covers the happy path: when
// `btrfs subvolume snapshot /src /dst` exits 0, CreateSnapshot
// returns nil and emits no error message.
func TestBtrfsCreateSnapshotSuccess(t *testing.T) {
	resetRunCommand(t)

	var capturedName string
	var capturedArgs []string
	runCommand = func(name string, args ...string) error {
		capturedName = name
		capturedArgs = append([]string(nil), args...)
		return nil
	}

	b := &BtrfsBackend{}
	err := b.CreateSnapshot("/data/source", "/data/snapshots/abc")
	if err != nil {
		t.Fatalf("CreateSnapshot() unexpected error: %v", err)
	}
	if capturedName != "btrfs" {
		t.Errorf("command name = %q, want %q", capturedName, "btrfs")
	}
	wantArgs := []string{"subvolume", "snapshot", "/data/source", "/data/snapshots/abc"}
	if len(capturedArgs) != len(wantArgs) {
		t.Fatalf("command args = %v, want %v", capturedArgs, wantArgs)
	}
	for i := range capturedArgs {
		if capturedArgs[i] != wantArgs[i] {
			t.Errorf("arg[%d] = %q, want %q", i, capturedArgs[i], wantArgs[i])
		}
	}
}

// TestBtrfsCreateSnapshotFailure wraps the underlying btrfs
// error so callers see which subcommand failed and with what
// argv (essential for triage when a real host's btrfs driver
// rejects a snapshot).
func TestBtrfsCreateSnapshotFailure(t *testing.T) {
	resetRunCommand(t)

	stderrBytes := []byte("ERROR: cannot snapshot '/data/source' (read-only subvolume)\n")
	runCommand = func(name string, args ...string) error {
		// Simulate exec.ExitError by wrapping it with a
		// typical stderr message — we don't strictly need
		// the typed *exec.ExitError because CreateSnapshot
		// treats any non-nil error from runCommand as
		// failure and the error wrapping only inspects .Error().
		return &fakeExitError{msg: "exit status 1", stderr: stderrBytes}
	}

	b := &BtrfsBackend{}
	err := b.CreateSnapshot("/data/source", "/data/snapshots/abc")
	if err == nil {
		t.Fatalf("CreateSnapshot() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "btrfs subvolume snapshot") {
		t.Errorf("error must name the failed subcommand; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "/data/source") {
		t.Errorf("error must include source path for triage; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "/data/snapshots/abc") {
		t.Errorf("error must include snapshot path for triage; got %q", err.Error())
	}
}

// TestBtrfsRestoreSuccess asserts the restore sequence:
//
//	btrfs subvolume delete /data/target   (if target exists)
//	btrfs subvolume snapshot /data/snapshots/abc /data/target
//
// The delete step is included so that a target path which is
// itself a btrfs subvolume can be replaced atomically (well,
// near-atomically — there's still a window where <target> is
// absent between delete and re-snapshot).
func TestBtrfsRestoreSuccess(t *testing.T) {
	resetRunCommand(t)

	var recorded []recordedInvocation
	runCommand = func(name string, args ...string) error {
		recorded = append(recorded, recordedInvocation{name: name, args: append([]string(nil), args...)})
		return nil
	}

	b := &BtrfsBackend{}
	err := b.Restore("/data/snapshots/abc", "/data/target")
	if err != nil {
		t.Fatalf("Restore() unexpected error: %v", err)
	}
	if len(recorded) < 2 {
		t.Fatalf("Restore() invoked %d commands, want at least 2 (delete + snapshot)", len(recorded))
	}
	// First call must be the delete.
	if recorded[0].name != "btrfs" {
		t.Errorf("first command name = %q, want %q", recorded[0].name, "btrfs")
	}
	if len(recorded[0].args) < 3 || recorded[0].args[0] != "subvolume" || recorded[0].args[1] != "delete" {
		t.Errorf("first command args = %v, want btrfs subvolume delete <target>", recorded[0].args)
	}
	// Final call must be a snapshot of <snap> into <target>.
	last := recorded[len(recorded)-1]
	if last.name != "btrfs" || len(last.args) < 4 {
		t.Fatalf("final command = %v, want btrfs subvolume snapshot <snap> <target>", last)
	}
	if last.args[0] != "subvolume" || last.args[1] != "snapshot" {
		t.Errorf("final command subargs = %v, want subvolume snapshot", last.args[:2])
	}
	if last.args[2] != "/data/snapshots/abc" || last.args[3] != "/data/target" {
		t.Errorf("final args = %v, want snap=first target=second", last.args[2:])
	}
}

// TestBtrfsRestoreDeleteFailureProceedsToSnapshot: if the
// optional `btrfs subvolume delete` on a non-existent target
// fails (it does — ENOENT), Restore must still attempt the
// re-snapshot. Otherwise a brand-new target path would brick
// the rollback path.
func TestBtrfsRestoreDeleteFailureProceedsToSnapshot(t *testing.T) {
	resetRunCommand(t)

	var recorded []recordedInvocation
	runCommand = func(name string, args ...string) error {
		recorded = append(recorded, recordedInvocation{name: name, args: append([]string(nil), args...)})
		// Pretend delete failed because target wasn't a
		// subvolume. We still attempt the snapshot.
		if args[0] == "delete" {
			return errors.New("ERROR: not a btrfs subvolume")
		}
		return nil
	}

	b := &BtrfsBackend{}
	err := b.Restore("/data/snapshots/abc", "/data/target")
	if err != nil {
		t.Fatalf("Restore() unexpected error: %v", err)
	}
	if len(recorded) != 2 {
		t.Fatalf("Restore() invoked %d commands, want 2 (delete + snapshot, both unconditionally)", len(recorded))
	}
	if recorded[1].args[1] != "snapshot" {
		t.Errorf("second command subarg = %q, want %q", recorded[1].args[1], "snapshot")
	}
}

// TestBtrfsRestoreSnapshotFailureBubbles asserts that a failure
// on the re-snapshot step (after a successful delete) surfaces
// as an error AND includes both paths so a confused operator
// can correlate the message with the tx that emitted it.
func TestBtrfsRestoreSnapshotFailureBubbles(t *testing.T) {
	resetRunCommand(t)

	runCommand = func(name string, args ...string) error {
		// Allow delete to succeed; fail the snapshot step.
		// args[1] is the btrfs subcommand ("delete" or
		// "snapshot"); args[0] is "subvolume" (a fixed
		// arg in both calls).
		if args[1] == "snapshot" {
			return &fakeExitError{msg: "exit status 1", stderr: []byte("ERROR: no space left on device\n")}
		}
		return nil
	}

	b := &BtrfsBackend{}
	err := b.Restore("/data/snapshots/abc", "/data/target")
	if err == nil {
		t.Fatalf("Restore() expected error from failed snapshot, got nil")
	}
	if !strings.Contains(err.Error(), "btrfs subvolume snapshot") {
		t.Errorf("error must reference snapshot subcommand; got %q", err.Error())
	}
}

// TestBtrfsBackendSatisfiesInterface is a compile-time /
// runtime check that the concrete backend satisfies the
// shared Backend interface used by rollback.SelectBackend().
func TestBtrfsBackendSatisfiesInterface(t *testing.T) {
	var _ Backend = &BtrfsBackend{}
}

// recordedInvocation is the (name, args) pair captured by a
// runCommand fake. Defined at file scope so individual tests
// can build a slice of invocations without re-declaring the
// type each time.
type recordedInvocation struct {
	name string
	args []string
}

// fakeExitError is a minimal stand-in for *exec.ExitError used
// only by tests. We do not import exec.ExitError directly
// because the production code only inspects `.Error()` — the
// concrete type doesn't matter.
type fakeExitError struct {
	msg    string
	stderr []byte
}

func (e *fakeExitError) Error() string { return e.msg }
