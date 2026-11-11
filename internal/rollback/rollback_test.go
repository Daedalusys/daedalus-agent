// Tests for the rollback package's SelectBackend function.
//
// SelectBackend is the boundary that maps an agent's detected
// snapshot_capability (from internal/snapshot.Detector) onto a
// concrete on-host Backend.
//
// 能力与回滚点是两件事:能力回答"宿主自带什么手段",回滚点回答"这次
// 变更有没有还原手段"。MVP 里回滚点按 kind 判(file 才有),宿主能力
// 一律 NONE —— copy 后端是 agent 内建的,所以 NONE 也选得出后端。
package rollback

import (
	"os"
	"path/filepath"
	"reflect"
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

// NONE 是"宿主没有自带快照机制",不是"这次变更没有还原手段":copy
// 后端是 agent 内建的,应当兜底选出来,而不是把 ApplyIntent 一并拒掉。
func TestSelectBackend_NoneYieldsCopyBackend(t *testing.T) {
	b, err := SelectBackend(agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE)
	if err != nil {
		t.Fatalf("NONE 应给出 copy 后端: %v", err)
	}
	if _, ok := b.(*CopyBackend); !ok {
		t.Errorf("后端类型 = %T, want *CopyBackend", b)
	}
}

// UNSPECIFIED 是能力枚举的零值(从未探测过)。既然宿主能力不再决定
// 回滚点,零值同样落到内建的 copy 后端。
func TestSelectBackend_UnspecifiedYieldsCopyBackend(t *testing.T) {
	b, err := SelectBackend(agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_UNSPECIFIED)
	if err != nil {
		t.Fatalf("UNSPECIFIED 应给出 copy 后端: %v", err)
	}
	if _, ok := b.(*CopyBackend); !ok {
		t.Errorf("后端类型 = %T, want *CopyBackend", b)
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
	// (deferred to daedalus-agent#8); use a high-numbered unknown
	// enum value to exercise the default branch.
	unknown := agentv1.SnapshotCapability(99)
	b, err := SelectBackend(unknown)
	if err == nil {
		t.Fatal("SelectBackend(unknown): nil err; want error")
	}
	if b != nil {
		t.Errorf("backend = %v, want nil", b)
	}
}

// --- LVM-snap / snapper 后端(Task 8)----------------------------------------

// 探测链顺序:System.Detector 报哪个能力,SelectBackend 就选哪个后端。
// "snapper present" 指能力已被探测为 SNAPSHOT_SNAPPER —— 该分支必须存在,
// 且不能被 deferred 兜底吞掉。
func TestSelectBackend_PrefersSnapperWhenPresent(t *testing.T) {
	cases := []struct {
		name string
		cap  agentv1.SnapshotCapability
		want any
	}{
		{"btrfs", agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME, &BtrfsBackend{}},
		{"snapper", agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_SNAPSHOT_SNAPPER, &snapperBackend{}},
		{"lvm-snap", agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_LVM_SNAP, &lvmSnapBackend{}},
		{"rsync", agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET, &RsyncBackend{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := SelectBackend(tc.cap)
			if err != nil {
				t.Fatalf("SelectBackend(%v): unexpected err: %v", tc.cap, err)
			}
			if reflect.TypeOf(b) != reflect.TypeOf(tc.want) {
				t.Errorf("backend type = %T, got %T, want %T", b, b, tc.want)
			}
		})
	}
}

// 宿主有 LVM thin-pool(能力探测为 LVM_SNAP)时选中 lvm-snap 后端。
func TestSelectBackend_LVMSnapWhenThinPoolPresent(t *testing.T) {
	b, err := SelectBackend(agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_LVM_SNAP)
	if err != nil {
		t.Fatalf("SelectBackend(LVM_SNAP): unexpected err: %v", err)
	}
	if _, ok := b.(*lvmSnapBackend); !ok {
		t.Errorf("backend type = %T, got %T, want *lvmSnapBackend", b, b)
	}
}

// 未知枚举仍拒绝:NONE / UNSPECIFIED 有内建后端兜底,但"未来 proto 新增
// 而本 agent 不认识的能力"不能猜 —— 猜错会拿错的后端去动宿主。
func TestSelectBackend_RefusesUnknownCapability(t *testing.T) {
	b, err := SelectBackend(agentv1.SnapshotCapability(99))
	if err == nil {
		t.Fatalf("SelectBackend(unknown): nil err; want refusal")
	}
	if b != nil {
		t.Errorf("backend = %v, got %v, want nil", b, b)
	}
}

// snapper 后端:CreateSnapshot 走 `snapper -c <cfg> create --type pre ...`,
// 把新快照号落到 sidecar;Restore 读号后 `snapper -c <cfg> undochange <n>..0`。
func TestSnapperBackend_CreateThenRestore(t *testing.T) {
	resetRunCommand(t)
	resetRunCommandOutput(t)

	var mutating []recordedInvocation
	runCommand = func(name string, args ...string) error {
		mutating = append(mutating, recordedInvocation{name: name, args: append([]string(nil), args...)})
		return nil
	}
	var queries []recordedInvocation
	runCommandOutput = func(name string, args ...string) ([]byte, error) {
		queries = append(queries, recordedInvocation{name: name, args: append([]string(nil), args...)})
		return []byte("42\n"), nil
	}

	sidecar := filepath.Join(t.TempDir(), "snap.num")
	b := &snapperBackend{}
	if err := b.CreateSnapshot("/data", sidecar); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if len(queries) != 1 || queries[0].name != "snapper" {
		t.Fatalf("create invocation = %v, got %v, want a single snapper call", queries, queries)
	}
	if !containsToken(queries[0].args, "create") || !containsToken(queries[0].args, "--type") {
		t.Errorf("snapper create args = %v, want create --type pre", queries[0].args)
	}
	got, rerr := os.ReadFile(sidecar)
	if rerr != nil {
		t.Fatalf("read sidecar: %v", rerr)
	}
	if strings.TrimSpace(string(got)) != "42" {
		t.Errorf("sidecar = %q, got %q, want %q", string(got), string(got), "42")
	}

	if err := b.Restore(sidecar, "/data"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(mutating) != 1 {
		t.Fatalf("restore invocations = %v, got %v, want one", mutating, mutating)
	}
	if mutating[0].name != "snapper" || !containsToken(mutating[0].args, "undochange") || !containsToken(mutating[0].args, "42..0") {
		t.Errorf("undochange args = %v, want snapper ... undochange 42..0", mutating[0].args)
	}
}

// lvm-snap 后端:CreateSnapshot 走 `lvcreate --snapshot`,Restore 走
// `lvconvert --merge`(merge 是 best-effort,需 LV 不再被占用才生效 ——
// 与"best-effort rollback, not bootc"的产品叙事一致)。
func TestLVMSnapBackend_CreateThenRestore(t *testing.T) {
	resetRunCommand(t)

	var recorded []recordedInvocation
	runCommand = func(name string, args ...string) error {
		recorded = append(recorded, recordedInvocation{name: name, args: append([]string(nil), args...)})
		return nil
	}

	b := &lvmSnapBackend{}
	if err := b.CreateSnapshot("/dev/vg0/root", "/var/lib/daedalus-agent/snapshots/i-1.snap"); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if len(recorded) != 1 || recorded[0].name != "lvcreate" || !containsToken(recorded[0].args, "--snapshot") {
		t.Fatalf("create invocation = %v, got %v, want lvcreate --snapshot ...", recorded, recorded)
	}
	if !containsToken(recorded[0].args, "/dev/vg0/root") {
		t.Errorf("lvcreate args = %v, must name the source LV", recorded[0].args)
	}

	if err := b.Restore("/var/lib/daedalus-agent/snapshots/i-1.snap", "/dev/vg0/root"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(recorded) != 2 || recorded[1].name != "lvconvert" || !containsToken(recorded[1].args, "--merge") {
		t.Fatalf("restore invocation = %v, got %v, want lvconvert --merge ...", recorded, recorded)
	}
}

// resetRunCommandOutput 还原 stdout 变体缝。
func resetRunCommandOutput(t *testing.T) {
	t.Helper()
	orig := runCommandOutput
	t.Cleanup(func() { runCommandOutput = orig })
}

// containsToken 报告 tokens 里是否有精确等于 want 的元素。
func containsToken(tokens []string, want string) bool {
	for _, tk := range tokens {
		if tk == want {
			return true
		}
	}
	return false
}

// lvm-snap 后端的参数形状守卫:它按**逻辑卷**寻址,传进来的却是目录时
// 必须当场报错,而不是把目录当 LV 名发给 lvcreate 后失败在半路。
func TestLVMSnapBackend_RejectsNonDeviceSource(t *testing.T) {
	resetRunCommand(t)
	called := 0
	runCommand = func(string, ...string) error { called++; return nil }

	b := &lvmSnapBackend{}
	err := b.CreateSnapshot("/var/lib/daedalus-agent/data/app.conf", "/var/lib/daedalus-agent/snapshots/i-1.snap")
	if err == nil {
		t.Fatal("CreateSnapshot: nil err; want a shape-guard rejection")
	}
	if called != 0 {
		t.Errorf("lvcreate invoked %d times, want 0 (guard must reject before running anything)", called)
	}
}
