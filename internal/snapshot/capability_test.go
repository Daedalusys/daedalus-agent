// Tests for the snapshot backend capability probe.
//
// Detect() 恒报 NONE:宿主自带机制(btrfs / rsync / lvm / snapper)不再参与
// 回滚点判定,copy 后端是 agent 内建的。probeBtrfs / probeRsync 保留给
// 扩展复用,所以这里直接测它们,而不是让空转的 Detect 把测试也带空。
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

// Detect 必须给出一个与宿主无关的答案:宿主装了 btrfs 或 rsync 也不改变
// 结果。能力字段只报"宿主自带机制",而 MVP 明确不依赖它们。
func TestDetect_ReportsNoneRegardlessOfHost(t *testing.T) {
	resetHooks(t)
	// 让宿主看起来"什么都有":若 Detect 还敢探测,它会拿到 btrfs 的
	// 非空输出,从而报 BTRFS —— 断言随即失败。
	execLookPath = func(string) (string, error) { return "/usr/bin/whatever", nil }
	runCommandOutput = func(string, ...string) ([]byte, error) {
		return []byte("subvolume show response\n"), nil
	}

	if got := Detect(); got != agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE {
		t.Errorf("Detect() = %v, want NONE(宿主机制不再探测)", got)
	}
}

// Detect 不得触碰宿主:一次 exec 都不该发生。这不只是性能问题 —— 探测
// 会把"宿主能力"重新变成 ApplyIntent 的隐性前提,而那正是要拆掉的耦合。
func TestDetect_DoesNotProbeHost(t *testing.T) {
	resetHooks(t)
	execLookPath = func(name string) (string, error) {
		t.Fatalf("Detect 不应探测宿主二进制: %q", name)
		return "", errors.New("unexpected")
	}
	runCommandOutput = func(name string, args ...string) ([]byte, error) {
		t.Fatalf("Detect 不应运行宿主命令: %q %v", name, args)
		return nil, errors.New("unexpected")
	}

	if got := Detect(); got != agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE {
		t.Errorf("Detect() = %v, want NONE", got)
	}
}

// Detect 仍要留下一条说明,操作员读日志时不该以为"这台机器什么都不能
// 回滚":NONE 说的是宿主没有自带机制。
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
		t.Fatalf("Detect() did not emit a notice; operators must not read NONE as 'no rollback at all'")
	}
	if !strings.Contains(stderr, "snapshot") {
		t.Errorf("notice must mention snapshot capability; got %q", stderr)
	}
	if strings.Contains(strings.ToLower(stderr), "best-effort") {
		t.Errorf("notice must not use the phrase 'best-effort' alone; got %q", stderr)
	}
}

// probeBtrfs 仍要能回答"这台机器的根文件系统是不是 btrfs" —— 扩展
// (btrfs 后端)接回来时靠它。这里只验它自己的输出判定,不经 Detect。
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

// btrfs 装了但根文件系统不是 btrfs:必须报"不可用",不能因为 PATH 里有
// 这个二进制就误判。
func TestProbeBtrfs_NotBtrfsRoot(t *testing.T) {
	resetHooks(t)
	execLookPath = func(string) (string, error) { return "/usr/sbin/btrfs", nil }
	runCommandOutput = func(string, ...string) ([]byte, error) {
		return []byte("ERROR: not a btrfs filesystem\n"), errors.New("exit status 1")
	}

	ok, err := probeBtrfs()
	if err != nil {
		t.Fatalf("probeBtrfs: %v", err)
	}
	if ok {
		t.Error("probeBtrfs = true, want false (根文件系统不是 btrfs)")
	}
}

// probeRsync 仍要能回答"宿主的 rsync 可用吗"。用它自己的 --version
// 存活探测,不经 Detect。
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
