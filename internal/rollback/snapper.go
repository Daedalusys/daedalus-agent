package rollback

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// snapperConfig 是 snapper 的配置名(一个受管子卷)。默认 "root" —— 绝大多数
// 宿主的主子卷就叫这个。做成包级变量,便于按宿主实际配置调整。
var snapperConfig = "root"

// snapperBackend 用宿主已有的 snapper 建 / 回滚快照。
//
// 形状差异:snapper 按"配置"(受管子卷)组织快照,并用**编号**寻址,而
// Backend 接口只给路径。本后端把 <snapshotPath> 当 sidecar 文件用:
// CreateSnapshot 把 snapper 返回的快照号写进去,Restore 再读回该号执行
// undochange —— 这样接口不变,编号也能跨步骤可靠传递(比"按路径反推编号"
// 的猜测稳)。
type snapperBackend struct{}

// CreateSnapshot 建一个 snapper pre 快照,并把快照号落到 <snapshotPath>。
func (s *snapperBackend) CreateSnapshot(sourcePath, snapshotPath string) error {
	out, err := runCommandOutput("snapper", "-c", snapperConfig, "create", "--type", "pre",
		"--description", "daedalus pre-apply: "+sourcePath)
	if err != nil {
		return fmt.Errorf("rollback: snapper -c %s create (%s): %w", snapperConfig, sourcePath, err)
	}
	num := parseSnapperNumber(string(out))
	if num == "" {
		// 拿不到编号就绝不猜:sidecar 写进错误的号等于回滚时打错目标。
		return fmt.Errorf("rollback: snapper create returned no snapshot number (output %q)", strings.TrimSpace(string(out)))
	}
	if werr := os.WriteFile(snapshotPath, []byte(num+"\n"), 0o600); werr != nil {
		return fmt.Errorf("rollback: write snapper sidecar %q: %w", snapshotPath, werr)
	}
	return nil
}

// Restore 读 sidecar 里的快照号,用 `undochange <num>..0` 把配置回滚到建
// 快照时的状态。
func (s *snapperBackend) Restore(snapshotPath, targetPath string) error {
	raw, err := os.ReadFile(snapshotPath)
	if err != nil {
		return fmt.Errorf("rollback: read snapper sidecar %q: %w", snapshotPath, err)
	}
	num := strings.TrimSpace(string(raw))
	if _, perr := strconv.Atoi(num); perr != nil {
		return fmt.Errorf("rollback: snapper sidecar %q does not hold a snapshot number (%q)", snapshotPath, num)
	}
	rangeArg := num + "..0"
	if rerr := runCommand("snapper", "-c", snapperConfig, "undochange", rangeArg); rerr != nil {
		return fmt.Errorf("rollback: snapper -c %s undochange %s (target %s): %w", snapperConfig, rangeArg, targetPath, rerr)
	}
	return nil
}

// parseSnapperNumber 从 snapper create 的输出里取快照号。正常只打印一个
// 十进制数;带噪声行时取最后一个纯数字行。
func parseSnapperNumber(out string) string {
	num := ""
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if _, err := strconv.Atoi(t); err == nil {
			num = t
		}
	}
	return num
}
