// Tests for the snapshot backend capability probe.
//
// The Detect() function inspects the host to decide which
// snapshot backend (btrfs subvolume / rsync target / none) the
// daedalus-agent can use for pre-ApplyIntent rollback. The tests
// mock the exec hook to keep the suite hermetic — true host
// capability is verified by the T4 真机 (Ubuntu 22.04 容器) run.
package snapshot

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// resetHooks restores the package-level exec hooks to their
// production bindings after a test has overridden them. Tests
// that mutate the hooks should call this in a cleanup.
func resetHooks(t *testing.T) {
	t.Helper()
	origLookPath := execLookPath
	origCommand := execCommand
	origRun := runCommandOutput
	t.Cleanup(func() {
		execLookPath = origLookPath
		execCommand = origCommand
		runCommandOutput = origRun
	})
}

// captureStderr swaps log's default writer with an in-memory
// buffer for the duration of fn, then returns whatever the agent
// emitted. The agent uses the standard log package (per T1-T3);
// rerouting the package writer is the least invasive way to
// assert that the RF3 WARN was emitted.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	origOut := log.Writer()
	origPrefix := log.Prefix()
	origFlags := log.Flags()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetPrefix("")
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOut)
		log.SetPrefix(origPrefix)
		log.SetFlags(origFlags)
	}()
	fn()
	return buf.String()
}

// TestDetectBtrfsPriority covers the happy path where btrfs is
// available and the agent picks the btrfs backend over rsync.
// btrfs wins over rsync because btrfs subvolume snapshots are
// copy-on-write and far cheaper than rsync file copies.
func TestDetectBtrfsPriority(t *testing.T) {
	resetHooks(t)
	execLookPath = func(name string) (string, error) {
		switch name {
		case "btrfs":
			return "/usr/sbin/btrfs", nil
		case "rsync":
			return "/usr/bin/rsync", nil
		}
		return "", errors.New("not found")
	}
	runCommandOutput = func(name string, args ...string) ([]byte, error) {
		if name == "btrfs" && len(args) >= 2 && args[0] == "subvolume" && args[1] == "show" {
			// Fake a non-empty "subvolume exists" output.
			return []byte("subvolume show response: id 256\n"), nil
		}
		return nil, errors.New("unexpected invocation")
	}

	if got := Detect(); got != agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME {
		t.Fatalf("Detect() = %v, want SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME", got)
	}
}

// TestDetectRsyncFallback covers the case where btrfs is not
// installed but rsync is. The agent should fall through to
// SNAPSHOT_CAPABILITY_RSYNC_TARGET.
func TestDetectRsyncFallback(t *testing.T) {
	resetHooks(t)
	execLookPath = func(name string) (string, error) {
		switch name {
		case "btrfs":
			return "", errors.New("btrfs not installed")
		case "rsync":
			return "/usr/bin/rsync", nil
		}
		return "", errors.New("not found")
	}
	runCommandOutput = func(name string, args ...string) ([]byte, error) {
		if name == "rsync" && len(args) >= 1 && args[0] == "--version" {
			return []byte("rsync version 3.2.7\n"), nil
		}
		return nil, errors.New("unexpected invocation")
	}

	if got := Detect(); got != agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET {
		t.Fatalf("Detect() = %v, want SNAPSHOT_CAPABILITY_RSYNC_TARGET", got)
	}
}

// TestDetectNoneEmitsRF3Warn covers the case where neither btrfs
// nor rsync is available. Detect() must return NONE AND emit a
// clear WARN (RF3 — see plan §RF3 and spec §5.2) explaining that
// this machine has no snapshot backend. The WARN must NOT use the
// phrase "best-effort" alone, which a human reviewer can misread
// as "we will handle it".
func TestDetectNoneEmitsRF3Warn(t *testing.T) {
	resetHooks(t)
	execLookPath = func(string) (string, error) { return "", errors.New("not found") }
	runCommandOutput = func(string, ...string) ([]byte, error) {
		t.Fatalf("runCommandOutput should not be called when no binary is on PATH")
		return nil, nil
	}

	var got agentv1.SnapshotCapability
	stderr := captureStderr(t, func() {
		got = Detect()
	})

	if got != agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE {
		t.Fatalf("Detect() = %v, want SNAPSHOT_CAPABILITY_NONE", got)
	}
	if stderr == "" {
		t.Fatalf("Detect() did not emit a WARN; RF3 requires a clear notice when no backend is available")
	}
	if !strings.Contains(stderr, "snapshot") {
		t.Errorf("WARN must mention snapshot backends; got %q", stderr)
	}
	if strings.Contains(strings.ToLower(stderr), "best-effort") {
		t.Errorf("WARN must not use the phrase 'best-effort' alone (RF3); got %q", stderr)
	}
}

// TestDetectBtrfsProbeErrorFallsThrough covers the case where the
// btrfs binary IS on PATH but `btrfs subvolume show /` exits
// non-zero (e.g. root filesystem is not btrfs, only /home is).
// probeBtrfs must report "unavailable" so Detect() can fall
// through to rsync instead of incorrectly claiming btrfs.
func TestDetectBtrfsProbeErrorFallsThrough(t *testing.T) {
	resetHooks(t)
	execLookPath = func(name string) (string, error) {
		switch name {
		case "btrfs":
			return "/usr/sbin/btrfs", nil
		case "rsync":
			return "/usr/bin/rsync", nil
		}
		return "", errors.New("not found")
	}
	runCommandOutput = func(name string, args ...string) ([]byte, error) {
		if name == "btrfs" {
			// btrfs binary present but root fs is not btrfs.
			return []byte("ERROR: not a btrfs filesystem\n"), errors.New("exit status 1")
		}
		if name == "rsync" && len(args) >= 1 && args[0] == "--version" {
			return []byte("rsync version 3.2.7\n"), nil
		}
		return nil, errors.New("unexpected invocation")
	}

	if got := Detect(); got != agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET {
		t.Fatalf("Detect() = %v, want SNAPSHOT_CAPABILITY_RSYNC_TARGET (btrfs probe failed)", got)
	}
}

// TestDetectPriorityBtrfsOverRsyncWhenBothPresent asserts the
// priority rule independently of TestDetectBtrfsPriority (which
// also covers btrfs-wins). This test deliberately installs a
// counter on the rsync probe so any accidental rsync invocation
// in the btrfs path is caught — a regression test for the
// "detect btrfs first, then check rsync only if btrfs probe
// failed" ordering.
func TestDetectPriorityBtrfsOverRsyncWhenBothPresent(t *testing.T) {
	resetHooks(t)
	execLookPath = func(name string) (string, error) {
		switch name {
		case "btrfs":
			return "/usr/sbin/btrfs", nil
		case "rsync":
			return "/usr/bin/rsync", nil
		}
		return "", errors.New("not found")
	}
	rsyncCalled := false
	runCommandOutput = func(name string, args ...string) ([]byte, error) {
		switch name {
		case "btrfs":
			return []byte("subvolume show response\n"), nil
		case "rsync":
			rsyncCalled = true
			return []byte("rsync version 3.2.7\n"), nil
		}
		return nil, errors.New("unexpected invocation")
	}

	if got := Detect(); got != agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME {
		t.Fatalf("Detect() = %v, want SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME", got)
	}
	if rsyncCalled {
		t.Fatalf("rsync must NOT be probed when btrfs is available (priority violation)")
	}
}