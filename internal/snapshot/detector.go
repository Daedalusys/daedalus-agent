// Package snapshot — Detector wrapper.
//
// The package-level Detect() function probes the host exactly
// once per call. For the gRPC server we need a stable answer
// across many RPC invocations — the agent's "what snapshot
// backend can I use?" does not change between two Heartbeats
// arriving 50ms apart — so we wrap Detect() in a small Detector
// that caches the result.
//
// Detector is concurrency-safe: the cache is guarded by a mutex
// so two simultaneous Register / ApplyIntent calls cannot race
// to disk-and-back and disagree. The cache is invalidated only
// by SetCapability(), which tests use to force a specific
// answer (NONE for the RF3 refusal branch, BTRFS for the
// happy path).
package snapshot

import (
	"sync"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// Detector is the agent's snapshot-capability cache. Construct
// with NewDetector(); the first Capability() call invokes
// Detect() (probing the host); subsequent calls return the
// cached answer.
//
// Tests can override the cached answer with SetCapability()
// to simulate any backend state without touching the host
// filesystem.
type Detector struct {
	mu sync.Mutex
	// cached is the most recently observed capability. The zero
	// value (UNSPECIFIED) means "no detection has run yet" —
	// Capability() runs Detect() lazily in that case.
	cached agentv1.SnapshotCapability
}

// NewDetector returns a Detector with an empty cache. The first
// Capability() call will probe the host (running Detect()).
func NewDetector() *Detector {
	return &Detector{}
}

// Capability returns the cached snapshot capability. If no
// detection has been performed yet, it invokes Detect() and
// caches the result. Subsequent calls return the cached answer
// without re-probing.
//
// The implementation prefers lazy detection over eager: the
// daemon can boot before the snapshot subsystem is ready (e.g.
// in tests where the cert paths are missing); calling Detect()
// at boot would force tests to build a fake btrfs/rsync
// environment. Lazy means tests that don't call Capability()
// don't pay for it.
func (d *Detector) Capability() agentv1.SnapshotCapability {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cached == agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_UNSPECIFIED {
		d.cached = Detect()
	}
	return d.cached
}

// SetCapability overrides the cached capability. Production code
// does not call this — it exists so tests can force NONE (for
// the RF3 refusal branch) or any other backend (for the
// happy-path tests) without faking btrfs/rsync on the host.
//
// SetCapability is concurrency-safe but tests typically call it
// before the detector is shared across goroutines.
func (d *Detector) SetCapability(c agentv1.SnapshotCapability) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cached = c
}