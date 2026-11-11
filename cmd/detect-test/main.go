//go:build detecttest

// Command detect-test is a debugging helper that calls
// snapshot.Detect() and prints the result to stdout.
//
// Detect() 恒报 NONE(宿主自带机制不再探测),所以这个工具现在只能证明
// "没有探测发生";等宿主机制接回来时它才重新有用。
//
// The `//go:build detecttest` build tag keeps this tool out of
// the production daedalus-agent binary — `go build ./...` does
// not compile it. To build, run:
//
//	go build -tags detecttest -o /tmp/detect-test ./cmd/detect-test
package main

import (
	"fmt"
	"os"

	"github.com/Daedalusys/daedalus-agent/internal/snapshot"
	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

func main() {
	got := snapshot.Detect()
	fmt.Printf("Detect() = %s (numeric=%d)\n", got.String(), int32(got))
	switch got {
	case agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME:
		fmt.Println("backend: btrfs-subvolume")
	case agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET:
		fmt.Println("backend: rsync-target")
	case agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE:
		// NONE 是设计态,不是故障:回滚点按 kind 判,copy 后端兜底。
		fmt.Println("backend: NONE — 宿主快照机制不再探测;回滚点按 kind 判,copy 后端兜底")
	default:
		fmt.Println("backend: UNSPECIFIED or unexpected")
		os.Exit(1)
	}
}
