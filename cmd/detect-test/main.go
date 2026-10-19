//go:build detecttest

// Command detect-test is a T4-only debugging helper that calls
// snapshot.Detect() in a clean environment and prints the result
// to stdout. It exists to support the T4 真机 validation runs
// (Ubuntu 22.04 容器, scenarios: rsync-only, no-rsync).
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
		fmt.Println("backend: NONE — ApplyIntent will be refused per RF3")
		os.Exit(2)
	default:
		fmt.Println("backend: UNSPECIFIED or unexpected")
		os.Exit(1)
	}
}