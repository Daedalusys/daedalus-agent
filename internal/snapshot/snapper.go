package snapshot

import "os"

// snapperConfigPath 是 snapper 的默认配置(受管子卷)路径。
//
// 与 internal/rollback 的 snapperConfig 同名同值("root"),这里复制一份
// 而不是跨包导出:snapshot 是探测层,rollback 是执行层,让探测层依赖执行层
// 会把两层的边界搅浑。做成变量便于测试替换。
var snapperConfigPath = "/etc/snapper/configs/root"

// probeSnapper 要求**二进制 + 配置**都在。
//
// 只看 LookPath 会在"装了 snapper 但没初始化配置"的宿主上误报:后端真去
// 建快照时才失败,而且失败发生在已经受理 intent 之后。在这里提前判掉,
// 让能力上报与后端可用性一致。
//
// 做成包级变量,理由同 probeBtrfs。
var probeSnapper = func() (bool, error) {
	if _, err := execLookPath("snapper"); err != nil {
		return false, nil
	}
	if _, err := os.Stat(snapperConfigPath); err != nil {
		return false, nil
	}
	return true, nil
}
