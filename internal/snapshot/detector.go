// Package snapshot — Detector wrapper.
//
// Detect() 真实探测宿主自带的快照机制,但 RPC 层仍需要"多次调用看到同一个
// 答案",所以用 Detector 把结果缓存起来。
//
// Detector is concurrency-safe: the cache is guarded by a mutex
// so two simultaneous Register / ApplyIntent calls cannot race
// to disk-and-back and disagree. The cache is invalidated only
// by SetCapability(), which tests use to force a specific
// answer (NONE / BTRFS) without touching the host.
package snapshot

import (
	"sync"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// Detector is the agent's snapshot-capability cache. Construct
// with NewDetector(); the first Capability() call invokes
// Detect(); subsequent calls return the cached answer.
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
// Capability() call will run Detect().
func NewDetector() *Detector {
	return &Detector{}
}

// Capability returns the cached snapshot capability. If no
// detection has been performed yet, it invokes Detect() and
// caches the result. Subsequent calls return the cached answer.
//
// 懒检测的理由是启动顺序与测试成本:daemon 可以在快照子系统就绪前
// 起来,而且不调用 Capability() 的测试不必为此准备宿主环境。
func (d *Detector) Capability() agentv1.SnapshotCapability {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cached == agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_UNSPECIFIED {
		d.cached = Detect()
	}
	return d.cached
}

// SetCapability overrides the cached capability. Production code
// does not call this — it exists so tests can force NONE or any
// other backend without faking btrfs/rsync on the host.
//
// SetCapability is concurrency-safe but tests typically call it
// before the detector is shared across goroutines.
func (d *Detector) SetCapability(c agentv1.SnapshotCapability) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cached = c
}
