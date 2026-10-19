// Tests for the rsync rollback backend.
//
// The rsync backend is the priority-2 fallback when btrfs is
// unavailable. Unlike btrfs, rsync has no native snapshot
// primitive — we approximate one with:
//
//	CreateSnapshot: `mkdir -p <snap_dir>` followed by
//	                `rsync -a --delete <src>/ <snap_dir>/`.
//
//	Restore:        `rsync -a --delete <snap_dir>/ <target>/`.
//
// The trailing slash in the source / destination paths matters:
// it tells rsync to copy *contents* (so <snap_dir> ends up
// containing whatever <src> contains, not a subdirectory named
// after <src>'s basename). The `--delete` flag keeps the
// snapshot compact by removing files no longer present at the
// source.
//
// The tests follow the same hook-substitution pattern as
// `btrfs_test.go` — a package-level `runCommand` variable is
// replaced with a recording fake. Real rsync is exercised only
// in the Step 7 真机 run.
package rollback

import (
	"errors"
	"strings"
	"testing"
)

// TestRsyncCreateSnapshotSuccess covers the happy path: when
// both the mkdir -p and the rsync invocations exit 0,
// CreateSnapshot returns nil. We also assert that the rsync
// argv uses the trailing-slash form so the snapshot dir ends
// up with the right shape (slipping this is a common bug).
func TestRsyncCreateSnapshotSuccess(t *testing.T) {
	resetRunCommand(t)

	var recorded []recordedInvocation
	runCommand = func(name string, args ...string) error {
		recorded = append(recorded, recordedInvocation{name: name, args: append([]string(nil), args...)})
		return nil
	}

	r := &RsyncBackend{}
	if err := r.CreateSnapshot("/data/source", "/data/snapshots/abc"); err != nil {
		t.Fatalf("CreateSnapshot() unexpected error: %v", err)
	}

	if len(recorded) != 2 {
		t.Fatalf("CreateSnapshot() invoked %d commands, want 2 (mkdir + rsync); got %v", len(recorded), recorded)
	}
	// First: mkdir -p
	if recorded[0].name != "mkdir" {
		t.Errorf("first command name = %q, want %q", recorded[0].name, "mkdir")
	}
	if len(recorded[0].args) != 2 || recorded[0].args[0] != "-p" || recorded[0].args[1] != "/data/snapshots/abc" {
		t.Errorf("mkdir args = %v, want [-p /data/snapshots/abc]", recorded[0].args)
	}
	// Second: rsync -a --delete <src>/ <snap>/
	if recorded[1].name != "rsync" {
		t.Errorf("second command name = %q, want %q", recorded[1].name, "rsync")
	}
	wantRsync := []string{"-a", "--delete", "/data/source/", "/data/snapshots/abc/"}
	if len(recorded[1].args) != len(wantRsync) {
		t.Fatalf("rsync arg count = %d, want %d (%v)", len(recorded[1].args), len(wantRsync), recorded[1].args)
	}
	for i := range recorded[1].args {
		if recorded[1].args[i] != wantRsync[i] {
			t.Errorf("rsync arg[%d] = %q, want %q", i, recorded[1].args[i], wantRsync[i])
		}
	}
}

// TestRsyncCreateSnapshotMkdirFailure verifies that a failed
// mkdir (e.g. permission denied on the snapshot parent dir)
// short-circuits the rsync step. Otherwise an apply step
// downstream might believe a snapshot exists when the directory
// was never created.
func TestRsyncCreateSnapshotMkdirFailure(t *testing.T) {
	resetRunCommand(t)

	rsyncCalled := false
	runCommand = func(name string, args ...string) error {
		if name == "rsync" {
			rsyncCalled = true
			return nil
		}
		return errors.New("mkdir: cannot create directory: Permission denied")
	}

	r := &RsyncBackend{}
	err := r.CreateSnapshot("/data/source", "/data/snapshots/abc")
	if err == nil {
		t.Fatalf("CreateSnapshot() expected error from mkdir failure, got nil")
	}
	if rsyncCalled {
		t.Errorf("rsync must NOT run when mkdir fails (short-circuit)")
	}
	if !strings.Contains(err.Error(), "mkdir") {
		t.Errorf("error must reference mkdir; got %q", err.Error())
	}
}

// TestRsyncCreateSnapshotRsyncFailure covers the case where
// mkdir succeeds but rsync fails (e.g. source unreadable).
// The error must wrap both paths and the rsync command name.
func TestRsyncCreateSnapshotRsyncFailure(t *testing.T) {
	resetRunCommand(t)

	runCommand = func(name string, args ...string) error {
		if name == "rsync" {
			return &fakeExitError{msg: "exit status 12", stderr: []byte("rsync: read errors mapping source\n")}
		}
		return nil
	}

	r := &RsyncBackend{}
	err := r.CreateSnapshot("/data/source", "/data/snapshots/abc")
	if err == nil {
		t.Fatalf("CreateSnapshot() expected error from rsync failure, got nil")
	}
	if !strings.Contains(err.Error(), "rsync") {
		t.Errorf("error must reference rsync; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "/data/source") {
		t.Errorf("error must include source path; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "/data/snapshots/abc") {
		t.Errorf("error must include snapshot path; got %q", err.Error())
	}
}

// TestRsyncRestoreSuccess covers the happy path: rsync -a --delete
// <snap>/ <target>/. The trailing-slash invariant is just as
// important on restore as on capture.
func TestRsyncRestoreSuccess(t *testing.T) {
	resetRunCommand(t)

	var recorded []recordedInvocation
	runCommand = func(name string, args ...string) error {
		recorded = append(recorded, recordedInvocation{name: name, args: append([]string(nil), args...)})
		return nil
	}

	r := &RsyncBackend{}
	if err := r.Restore("/data/snapshots/abc", "/data/target"); err != nil {
		t.Fatalf("Restore() unexpected error: %v", err)
	}

	if len(recorded) != 1 {
		t.Fatalf("Restore() invoked %d commands, want 1; got %v", len(recorded), recorded)
	}
	if recorded[0].name != "rsync" {
		t.Errorf("command name = %q, want %q", recorded[0].name, "rsync")
	}
	wantArgs := []string{"-a", "--delete", "/data/snapshots/abc/", "/data/target/"}
	if len(recorded[0].args) != len(wantArgs) {
		t.Fatalf("rsync args = %v, want %v", recorded[0].args, wantArgs)
	}
	for i := range recorded[0].args {
		if recorded[0].args[i] != wantArgs[i] {
			t.Errorf("rsync arg[%d] = %q, want %q", i, recorded[0].args[i], wantArgs[i])
		}
	}
}

// TestRsyncRestoreFailureBubbles asserts that a non-zero exit
// from rsync during restore surfaces as an error referencing
// both paths. Without this the tx layer's "rollback failed"
// branch would have no diagnostic to emit.
func TestRsyncRestoreFailureBubbles(t *testing.T) {
	resetRunCommand(t)

	runCommand = func(name string, args ...string) error {
		return &fakeExitError{msg: "exit status 23", stderr: []byte("rsync: some files could not be transferred\n")}
	}

	r := &RsyncBackend{}
	err := r.Restore("/data/snapshots/abc", "/data/target")
	if err == nil {
		t.Fatalf("Restore() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "rsync") {
		t.Errorf("error must reference rsync; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "/data/snapshots/abc") {
		t.Errorf("error must include snapshot path; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "/data/target") {
		t.Errorf("error must include target path; got %q", err.Error())
	}
}

// TestRsyncBackendSatisfiesInterface is the compile-time /
// runtime check that the concrete backend satisfies the shared
// Backend interface used by SelectBackend.
func TestRsyncBackendSatisfiesInterface(t *testing.T) {
	var _ Backend = &RsyncBackend{}
}
