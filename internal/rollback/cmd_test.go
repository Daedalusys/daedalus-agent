package rollback

import "testing"

// 计划期的 `apt history-info` 也是英文键("Start time")才解析得出来;宿主
// locale 会把它翻掉,所以命令缝统一固定 C locale。
func TestNewCmdForcesCLocale(t *testing.T) {
	t.Setenv("LC_ALL", "zh_CN.UTF-8")

	out, err := newCmd("sh", "-c", `printf %s "$LC_ALL"`).Output()
	if err != nil {
		t.Fatalf("newCmd: %v", err)
	}
	if string(out) != "C" {
		t.Fatalf("LC_ALL = %q, want C", string(out))
	}
}
