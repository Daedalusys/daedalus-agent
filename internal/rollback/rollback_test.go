// Tests for the rollback package's SelectBackend function.
//
// SelectBackend 有两个输入:回滚点形态(Point)与宿主能力
// (snapshot_capability)。回滚点决定"这次变更有没有还原手段",宿主能力只在
// PointVolume 时决定"用哪种手段",PointFile 恒走内建 copy。
package rollback

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// TestSelectBackendByPointAndCapability 钉住 Review Focus 前两条:
// file 在 LVM 宿主上仍走 copy;PointNone 一律拒绝。
func TestSelectBackendByPointAndCapability(t *testing.T) {
	cases := []struct {
		name  string
		point Point
		cap   agentv1.SnapshotCapability
		want  any
		err   bool
	}{
		{"file 在 LVM 宿主上仍走 copy", PointFile, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_LVM_SNAP, &CopyBackend{}, false},
		{"file 在无能力宿主上走 copy", PointFile, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE, &CopyBackend{}, false},
		{"file 忽略未知能力仍走 copy", PointFile, agentv1.SnapshotCapability(99), &CopyBackend{}, false},
		{"volume 在 LVM 宿主上走 lvm", PointVolume, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_LVM_SNAP, &lvmSnapBackend{}, false},
		{"volume 在 snapper 宿主上走 snapper", PointVolume, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_SNAPSHOT_SNAPPER, &snapperBackend{}, false},
		{"volume 在 btrfs 宿主上走 btrfs", PointVolume, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME, &BtrfsBackend{}, false},
		{"volume 在无能力宿主上拒绝", PointVolume, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE, nil, true},
		{"volume 在 rsync 宿主上拒绝", PointVolume, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET, nil, true},
		{"none 一律拒绝", PointNone, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := SelectBackend(c.point, c.cap)
			if c.err {
				if err == nil {
					t.Fatalf("SelectBackend(%v,%v) 期望报错, got %T", c.point, c.cap, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SelectBackend(%v,%v) 意外报错: %v", c.point, c.cap, err)
			}
			if reflect.TypeOf(got) != reflect.TypeOf(c.want) {
				t.Errorf("后端类型 = %T, want %T", got, c.want)
			}
		})
	}
}

// TestSelectBackend_Btrfs_ReturnsBtrfsBackend covers the
// priority-1 capability: btrfs subvolume snapshot.
func TestSelectBackend_Btrfs_ReturnsBtrfsBackend(t *testing.T) {
	b, err := SelectBackend(PointVolume, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME)
	if err != nil {
		t.Fatalf("SelectBackend: %v", err)
	}
	if _, ok := b.(*BtrfsBackend); !ok {
		t.Errorf("backend type = %T, want *BtrfsBackend", b)
	}
}

// rsync 只能复制文件,覆盖不了卷级状态:PointVolume + rsync 必须拒绝,
// 不能把 RSYNC_TARGET 当成包/卷的回滚点。
func TestSelectBackend_RsyncNotUsableForVolume(t *testing.T) {
	b, err := SelectBackend(PointVolume, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET)
	if err == nil {
		t.Fatalf("SelectBackend(PointVolume, RSYNC_TARGET) = %T; want refusal", b)
	}
	if b != nil {
		t.Errorf("backend = %v, want nil", b)
	}
}

// NONE 是"宿主没有自带快照机制",不是"这次变更没有还原手段":file 的
// copy 后端是 agent 内建的,与宿主能力无关。
func TestSelectBackend_NoneYieldsCopyBackendForFile(t *testing.T) {
	b, err := SelectBackend(PointFile, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE)
	if err != nil {
		t.Fatalf("NONE 应给出 copy 后端: %v", err)
	}
	if _, ok := b.(*CopyBackend); !ok {
		t.Errorf("后端类型 = %T, want *CopyBackend", b)
	}
}

// UNSPECIFIED 是能力枚举的零值(从未探测过)。file 仍走 copy。
func TestSelectBackend_UnspecifiedYieldsCopyBackendForFile(t *testing.T) {
	b, err := SelectBackend(PointFile, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_UNSPECIFIED)
	if err != nil {
		t.Fatalf("UNSPECIFIED 应给出 copy 后端: %v", err)
	}
	if _, ok := b.(*CopyBackend); !ok {
		t.Errorf("后端类型 = %T, want *CopyBackend", b)
	}
}

// 未知枚举值不能猜:PointVolume 上不认识的能力必须拒绝,拿错后端会动错宿主。
func TestSelectBackend_DeferredCapabilities_ReturnsError(t *testing.T) {
	unknown := agentv1.SnapshotCapability(99)
	b, err := SelectBackend(PointVolume, unknown)
	if err == nil {
		t.Fatal("SelectBackend(volume, unknown): nil err; want error")
	}
	if b != nil {
		t.Errorf("backend = %v, want nil", b)
	}
}

// --- LVM-snap / snapper 后端(Task 8)----------------------------------------

// 探测链顺序:System.Detector 报哪个能力,PointVolume 就选哪个后端。
func TestSelectBackend_PrefersSnapperWhenPresent(t *testing.T) {
	cases := []struct {
		name string
		cap  agentv1.SnapshotCapability
		want any
	}{
		{"btrfs", agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME, &BtrfsBackend{}},
		{"snapper", agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_SNAPSHOT_SNAPPER, &snapperBackend{}},
		{"lvm-snap", agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_LVM_SNAP, &lvmSnapBackend{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := SelectBackend(PointVolume, tc.cap)
			if err != nil {
				t.Fatalf("SelectBackend(%v): unexpected err: %v", tc.cap, err)
			}
			if reflect.TypeOf(b) != reflect.TypeOf(tc.want) {
				t.Errorf("backend type = %T, got %T, want %T", b, b, tc.want)
			}
		})
	}
}

// 宿主有 LVM 卷组(能力探测为 LVM_SNAP)时,卷级回滚点选中 lvm-snap 后端。
func TestSelectBackend_LVMSnapWhenThinPoolPresent(t *testing.T) {
	b, err := SelectBackend(PointVolume, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_LVM_SNAP)
	if err != nil {
		t.Fatalf("SelectBackend(LVM_SNAP): unexpected err: %v", err)
	}
	if _, ok := b.(*lvmSnapBackend); !ok {
		t.Errorf("backend type = %T, got %T, want *lvmSnapBackend", b, b)
	}
}

// 未知枚举仍拒绝:NONE 对 file 有内建后端兜底,但"未来 proto 新增而本
// agent 不认识的能力"不能猜 —— 猜错会拿错的后端去动宿主。
func TestSelectBackend_RefusesUnknownCapability(t *testing.T) {
	b, err := SelectBackend(PointVolume, agentv1.SnapshotCapability(99))
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

// TestLVMSnapshotRequiresDevicePath 是 lvm-snap 的形状守卫:sourcePath 不是
// /dev/ 设备路径时必须当场报错,且 lvcreate 一次都不执行 —— 把文件路径发给
// lvcreate 会把错误推迟到半路才暴露。
func TestLVMSnapshotRequiresDevicePath(t *testing.T) {
	resetRunCommand(t)
	called := 0
	runCommand = func(string, ...string) error { called++; return nil }

	err := (&lvmSnapBackend{}).CreateSnapshot("/etc/hosts", filepath.Join(t.TempDir(), "i-1.snap"))
	if err == nil {
		t.Fatal("CreateSnapshot: nil err; want shape-guard rejection")
	}
	if !strings.Contains(err.Error(), "logical-volume device path") {
		t.Errorf("err = %q, want 含 logical-volume device path", err)
	}
	if called != 0 {
		t.Errorf("lvcreate invoked %d times, want 0", called)
	}
}

// 目录级 file intent 走内建 tree 后端,与宿主能力无关(与 PointFile 同一条
// 理由:内建后端不依赖宿主有没有快照机制)。
func TestSelectBackend_PointTree(t *testing.T) {
	b, err := SelectBackend(PointTree, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE)
	if err != nil {
		t.Fatalf("SelectBackend(PointTree): %v", err)
	}
	if _, ok := b.(*TreeBackend); !ok {
		t.Errorf("后端类型 = %T, want *TreeBackend", b)
	}
	if _, ok := b.(PathSetBackend); !ok {
		t.Errorf("tree 后端必须实现 PathSetBackend, got %T", b)
	}
}

// SelectBackend 拿不到 dataDir;生产必须用 SelectBackendFor 把数据目录带进来,
// 否则 GC 找不到快照集合(CreateSnapshotSet 会 fail-closed)。
func TestSelectBackendFor_PointTreeCarriesDataDir(t *testing.T) {
	b, err := SelectBackendFor(PointTree, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE, "/var/lib/daedalus-agent")
	if err != nil {
		t.Fatalf("SelectBackendFor(PointTree): %v", err)
	}
	tb, ok := b.(*TreeBackend)
	if !ok {
		t.Fatalf("后端类型 = %T, want *TreeBackend", b)
	}
	if tb.dataDir != "/var/lib/daedalus-agent" {
		t.Errorf("dataDir = %q, want /var/lib/daedalus-agent", tb.dataDir)
	}
}

// SelectBackendFor 对其他点的行为与 SelectBackend 一致。
func TestSelectBackendFor_NonTreeDelegates(t *testing.T) {
	b, err := SelectBackendFor(PointFile, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE, "/var/lib/x")
	if err != nil {
		t.Fatalf("SelectBackendFor(PointFile): %v", err)
	}
	if _, ok := b.(*CopyBackend); !ok {
		t.Errorf("后端类型 = %T, want *CopyBackend", b)
	}
	if _, err := SelectBackendFor(PointNone, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE, "/var/lib/x"); err == nil {
		t.Fatal("PointNone 仍应拒绝")
	}
}
