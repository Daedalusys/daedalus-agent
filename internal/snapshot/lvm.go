package snapshot

import "strings"

// probeLVM 只认"有卷组":vgs 列出非空卷组名,才算宿主具备 LVM 快照能力。
//
// 为什么不用 LookPath("lvcreate") 就算数:装了 lvm2 但一个卷组都没建的
// 宿主并不少(工具随镜像预装),报 LVM_SNAP 会让控制面以为能在卷层拍快照。
// 缺工具或命令失败都是正常结局,不是需要上报的错误。
//
// 做成包级变量,理由同 probeBtrfs。
var probeLVM = func() (bool, error) {
	if _, err := execLookPath("vgs"); err != nil {
		return false, nil
	}
	out, err := runCommandOutput("vgs", "--noheadings", "-o", "vg_name")
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(string(out)) != "", nil
}
