//go:build realmachine

package exec

import (
	"context"
	"time"
)

// realmachine 构建(CI 的 package-manager-shapes job)专用出口。
//
// capture* 不导出,而真机测试必须同时调 capture(本包)与 rollback.BuildPlan
// —— rollback 反向 import 本包,测试写在包内会成 import cycle。所以只在
// realmachine 构建里开这两扇门:生产二进制没有它们,生产 API 的语义不变。
func CaptureAptHandleForRealMachine(ctx context.Context, pkg string, since time.Time) (string, error) {
	return captureAptHandle(ctx, pkg, since)
}

// CaptureDnfHandleForRealMachine 是 captureDnfHandle 的 realmachine 出口。
func CaptureDnfHandleForRealMachine(ctx context.Context, pkg string, since time.Time) (string, error) {
	return captureDnfHandle(ctx, pkg, since)
}
