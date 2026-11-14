package main

import "testing"

// TestVersionString 钉住版本串的三种形态。
//
// 为什么要表驱动钉住:发布构建靠 -ldflags 注入 version/commit/buildDate,
// 一旦注入没生效(或三个变量名与 workflow 里的 -X 对不上),机器上装完会
// 自报 "0.1.0-dev" —— 用户会以为自己装的是发布版。三种形态分别是:
//   - dev 构建(无 ldflags):只输出 version,不带空的 commit=/built= 尾巴;
//   - 发布构建:version + commit + built 三段都要出现;
//   - 半填充(只有 commit 没有 buildDate):不得打印 "built=" 这种空字段。
func TestVersionString(t *testing.T) {
	cases := []struct{ name, ver, sha, date, want string }{
		{"dev 构建", "0.1.0-dev", "", "", "0.1.0-dev"},
		{"发布构建", "0.1.0", "abc1234", "2026-11-15T04:00:00Z", "0.1.0 commit=abc1234 built=2026-11-15T04:00:00Z"},
		{"缺 buildDate 时不打印空字段", "0.1.0", "abc1234", "", "0.1.0 commit=abc1234"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 每个用例临时改写三个包级变量,结束时还原,避免用例之间串味。
			origVersion, origCommit, origBuildDate := version, commit, buildDate
			defer func() {
				version, commit, buildDate = origVersion, origCommit, origBuildDate
			}()
			version, commit, buildDate = tc.ver, tc.sha, tc.date

			if got := versionString(); got != tc.want {
				t.Errorf("versionString() = %q, want %q", got, tc.want)
			}
		})
	}
}
