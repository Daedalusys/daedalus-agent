// Tests for the snapshot backend capability probe.
//
// Detect() 恢复真实探测:宿主自带机制按 snapper > lvm > btrfs > rsync 的
// 优先级上报。探针之间是包级变量缝,测试替换它们即可,不碰真机。
package snapshot

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
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
// emitted.
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

// stubProbes 把四个探针换成返回常量的 fake,并返回还原函数。
func stubProbes(t *testing.T, snapper, lvm, btrfs, rsync bool) func() {
	t.Helper()
	origSnapper, origLVM, origBtrfs, origRsync := probeSnapper, probeLVM, probeBtrfs, probeRsync
	probeSnapper = func() (bool, error) { return snapper, nil }
	probeLVM = func() (bool, error) { return lvm, nil }
	probeBtrfs = func() (bool, error) { return btrfs, nil }
	probeRsync = func() (bool, error) { return rsync, nil }
	return func() {
		probeSnapper = origSnapper
		probeLVM = origLVM
		probeBtrfs = origBtrfs
		probeRsync = origRsync
	}
}

// Detect 的优先级钉死:snapper > lvm > btrfs > rsync > none。
func TestDetectPriority(t *testing.T) {
	cases := []struct {
		name                       string
		snapper, lvm, btrfs, rsync bool
		want                       agentv1.SnapshotCapability
	}{
		{"snapper 优先于 btrfs", true, false, true, true, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_SNAPSHOT_SNAPPER},
		{"lvm 优先于 btrfs", false, true, true, true, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_LVM_SNAP},
		{"只有 btrfs", false, false, true, false, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME},
		{"只有 rsync", false, false, false, true, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET},
		{"什么都没有", false, false, false, false, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			restore := stubProbes(t, c.snapper, c.lvm, c.btrfs, c.rsync)
			defer restore()
			if got := Detect(); got != c.want {
				t.Errorf("Detect() = %v, want %v", got, c.want)
			}
		})
	}
}

// Detect 仍要留一行能力日志:操作员读 journald 时要能看见上报值。
func TestDetectLogsCapability(t *testing.T) {
	restore := stubProbes(t, false, false, false, false)
	defer restore()

	var got agentv1.SnapshotCapability
	stderr := captureStderr(t, func() { got = Detect() })
	if got != agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE {
		t.Fatalf("Detect() = %v, want NONE", got)
	}
	if !strings.Contains(stderr, "snapshot capability=") {
		t.Errorf("日志 = %q, want 含 snapshot capability=", stderr)
	}
}

// btrfs 装了但根文件系统不是 btrfs:必须报"不可用",不能因为 PATH 里有
// 这个二进制就误判。
func TestProbeBtrfsNeedsBtrfsRoot(t *testing.T) {
	resetHooks(t)
	execLookPath = func(name string) (string, error) {
		if name == "btrfs" {
			return "/usr/sbin/btrfs", nil
		}
		return "", errors.New("not found")
	}
	runCommandOutput = func(name string, args ...string) ([]byte, error) {
		return []byte("ERROR: not a btrfs filesystem\n"), errors.New("exit status 1")
	}

	ok, err := probeBtrfs()
	if err != nil {
		t.Fatalf("probeBtrfs: %v", err)
	}
	if ok {
		t.Error("probeBtrfs = true, want false (btrfs-progs 在但根不是 btrfs)")
	}
}

// snapper 二进制在、但缺少配置文件:不能报 SNAPSHOT_SNAPPER,否则后端
// 建快照时才发现无配置。
func TestProbeSnapperNeedsConfig(t *testing.T) {
	resetHooks(t)
	execLookPath = func(name string) (string, error) {
		if name == "snapper" {
			return "/usr/bin/snapper", nil
		}
		return "", errors.New("not found")
	}
	origPath := snapperConfigPath
	snapperConfigPath = filepath.Join(t.TempDir(), "missing-root")
	t.Cleanup(func() { snapperConfigPath = origPath })

	ok, err := probeSnapper()
	if err != nil {
		t.Fatalf("probeSnapper: %v", err)
	}
	if ok {
		t.Error("probeSnapper = true, want false (snapper 在但缺 /etc/snapper/configs/root)")
	}
}

// 二进制与配置都在时才算可用。
func TestProbeSnapperNeedsBinaryAndConfig(t *testing.T) {
	resetHooks(t)
	execLookPath = func(name string) (string, error) {
		if name == "snapper" {
			return "/usr/bin/snapper", nil
		}
		return "", errors.New("not found")
	}
	cfg := filepath.Join(t.TempDir(), "root")
	if err := os.WriteFile(cfg, []byte("[root]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	origPath := snapperConfigPath
	snapperConfigPath = cfg
	t.Cleanup(func() { snapperConfigPath = origPath })

	ok, err := probeSnapper()
	if err != nil {
		t.Fatalf("probeSnapper: %v", err)
	}
	if !ok {
		t.Error("probeSnapper = false, want true (二进制与配置都在)")
	}
}

// LVM 只认"有卷组":vgs 输出非空。二进制缺失/命令失败是正常结局。
func TestProbeLVMReportsVolumeGroup(t *testing.T) {
	resetHooks(t)
	execLookPath = func(name string) (string, error) { return "/usr/sbin/vgs", nil }
	runCommandOutput = func(name string, args ...string) ([]byte, error) {
		return []byte("  vgtest\n  vgroot\n"), nil
	}
	ok, err := probeLVM()
	if err != nil {
		t.Fatalf("probeLVM: %v", err)
	}
	if !ok {
		t.Error("probeLVM = false, want true (vgs 输出有卷组)")
	}
}

func TestProbeLVMMissingBinary(t *testing.T) {
	resetHooks(t)
	execLookPath = func(string) (string, error) { return "", errors.New("not found") }
	runCommandOutput = func(string, ...string) ([]byte, error) {
		t.Fatal("vgs 不该在二进制缺失时被调用")
		return nil, nil
	}
	ok, err := probeLVM()
	if err != nil {
		t.Fatalf("probeLVM: %v", err)
	}
	if ok {
		t.Error("probeLVM = true, want false (缺 vgs)")
	}
}

// probeBtrfs 仍要能回答"这台机器的根文件系统是不是 btrfs"。
func TestProbeBtrfs_DetectsSubvolume(t *testing.T) {
	resetHooks(t)
	execLookPath = func(name string) (string, error) {
		if name == "btrfs" {
			return "/usr/sbin/btrfs", nil
		}
		return "", errors.New("not found")
	}
	runCommandOutput = func(name string, args ...string) ([]byte, error) {
		if name == "btrfs" && len(args) >= 2 && args[0] == "subvolume" && args[1] == "show" {
			return []byte("subvolume show response: id 256\n"), nil
		}
		return nil, errors.New("unexpected invocation")
	}

	ok, err := probeBtrfs()
	if err != nil {
		t.Fatalf("probeBtrfs: %v", err)
	}
	if !ok {
		t.Error("probeBtrfs = false, want true (根文件系统是 btrfs)")
	}
}

// probeRsync 仍要能回答"宿主的 rsync 可用吗"。
func TestProbeRsync_DetectsBinary(t *testing.T) {
	resetHooks(t)
	execLookPath = func(name string) (string, error) {
		if name == "rsync" {
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

	ok, err := probeRsync()
	if err != nil {
		t.Fatalf("probeRsync: %v", err)
	}
	if !ok {
		t.Error("probeRsync = false, want true")
	}
}
