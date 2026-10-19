// Tests for the rollback package's SelectBackend function.
//
// SelectBackend is the boundary that maps an agent's detected
// snapshot_capability (from internal/snapshot.Detector) onto a
// concrete on-host Backend. The RF3 invariant — "ApplyIntent
// refuses on NONE capability" — is enforced at this boundary:
// the handler calls SelectBackend; on NONE/UNSPECIFIED/
// deferred-capabilities, SelectBackend returns ErrNoBackend
// and the handler translates that to
// APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE without
// ever constructing a tx / invoking a backend / calling apply.
package rollback

import (
	"errors"
	"strings"
	"testing"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// TestSelectBackend_Btrfs_ReturnsBtrfsBackend covers the
// priority-1 capability: btrfs subvolume snapshot.
func TestSelectBackend_Btrfs_ReturnsBtrfsBackend(t *testing.T) {
	b, err := SelectBackend(agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME)
	if err != nil {
		t.Fatalf("SelectBackend: %v", err)
	}
	if _, ok := b.(*BtrfsBackend); !ok {
		t.Errorf("backend type = %T, want *BtrfsBackend", b)
	}
}

// TestSelectBackend_Rsync_ReturnsRsyncBackend covers the
// priority-2 fallback capability: rsync file-copy.
func TestSelectBackend_Rsync_ReturnsRsyncBackend(t *testing.T) {
	b, err := SelectBackend(agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET)
	if err != nil {
		t.Fatalf("SelectBackend: %v", err)
	}
	if _, ok := b.(*RsyncBackend); !ok {
		t.Errorf("backend type = %T, want *RsyncBackend", b)
	}
}

// TestSelectBackend_None_ReturnsErrNoBackend — the RF3 surface.
// NONE must return ErrNoBackend so the handler's
// errors.Is(err, ErrNoBackend) check fires and translates
// the failure into
// APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE with
// the RF3 wording (in internal/rpc/apply_intent.go).
//
// The error message must mention the missing backends so a
// reviewer reading the failure can immediately tell why no
// snapshot was taken.
func TestSelectBackend_None_ReturnsErrNoBackend(t *testing.T) {
	b, err := SelectBackend(agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE)
	if !errors.Is(err, ErrNoBackend) {
		t.Errorf("err = %v, want ErrNoBackend", err)
	}
	if b != nil {
		t.Errorf("backend = %v, want nil (no backend on NONE)", b)
	}
	if !strings.Contains(err.Error(), "btrfs") {
		t.Errorf("err message must reference btrfs for triage; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "rsync") {
		t.Errorf("err message must reference rsync for triage; got %q", err.Error())
	}
}

// TestSelectBackend_Unspecified_ReturnsErrNoBackend — UNSPECIFIED
// is the zero value of the capability enum (an init-time /
// never-detected state). Treating it as NONE is fail-safe:
// refusing an intent whose own snapshot_capability is unset is
// safer than guessing a backend.
func TestSelectBackend_Unspecified_ReturnsErrNoBackend(t *testing.T) {
	b, err := SelectBackend(agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_UNSPECIFIED)
	if !errors.Is(err, ErrNoBackend) {
		t.Errorf("err = %v, want ErrNoBackend (UNSPECIFIED must look like NONE)", err)
	}
	if b != nil {
		t.Errorf("backend = %v, want nil (UNSPECIFIED must not select a backend)", b)
	}
}

// TestSelectBackend_DeferredCapabilities_ReturnsError covers
// the LVM / snapper enum values (and any future proto addition
// the agent doesn't know). The agent MUST refuse these too —
// silently picking no backend for an unsupported capability
// would look identical to NONE on the wire, but we want the
// audit / control-plane UI to be able to distinguish "we know
// about this but deferred to P0.6+" from "we probed and found
// nothing" so the upgrade signal is preserved.
func TestSelectBackend_DeferredCapabilities_ReturnsError(t *testing.T) {
	// The current proto doesn't define LVM / snapper values
	// (deferred to issue #10); use a high-numbered unknown
	// enum value to exercise the default branch.
	unknown := agentv1.SnapshotCapability(99)
	b, err := SelectBackend(unknown)
	if err == nil {
		t.Fatal("SelectBackend(unknown): nil err; want error")
	}
	if b != nil {
		t.Errorf("backend = %v, want nil", b)
	}
	if errors.Is(err, ErrNoBackend) {
		// Defensive — unknown capabilities should fall
		// through to a generic refusal (not ErrNoBackend,
		// so handlers can distinguish via errors.Is).
		t.Logf("unknown capability returned ErrNoBackend; acceptable as a refusal but loses the deferred/upgraded signal")
	}
}
