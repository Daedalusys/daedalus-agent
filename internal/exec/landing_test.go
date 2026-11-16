package exec

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// aptHost / dnfHost 是落点体检用例里的宿主描述。
var (
	aptHost = hostManager{name: "apt", bin: "/usr/bin/apt-get"}
	dnfHost = hostManager{name: "dnf", bin: "/usr/bin/dnf"}
)

// 已装包走本地清单(dpkg-query -L),绝不联网:升级路径不能因为仓库不可达
// 就被判成"查不出落点"。
func TestLandingPaths_AptInstalledUsesLocalList(t *testing.T) {
	h := &fakeHost{manager: "apt", installed: map[string]bool{"nginx": true}}
	h.replies = map[string]fakeReply{
		"dpkg-query -L nginx": {out: ".\n/usr/sbin/nginx\n/etc/nginx/nginx.conf\n"},
	}
	withFakeHost(t, h)

	got, err := landingPaths(context.Background(), aptHost, "nginx", true)
	if err != nil {
		t.Fatalf("landingPaths: %v", err)
	}
	if got.skipped != "" {
		t.Errorf("skipped = %q, want 空(这次体检过了)", got.skipped)
	}
	want := []string{"/etc/nginx/nginx.conf", "/usr/sbin/nginx"}
	if strings.Join(got.paths, "|") != strings.Join(want, "|") {
		t.Fatalf("paths = %v, want %v", got.paths, want)
	}
	// 已装包绝不许走网络
	for _, c := range h.calls {
		if len(c) > 1 && c[1] == "download" {
			t.Fatalf("已装包不该下载: %v", c)
		}
	}
}

// 未装包:下载到临时目录再列内容。下载必须经 runCmdInDir 落在临时目录里,
// 清单必须来自那个 .deb(dpkg-deb -c)。
func TestLandingPaths_AptNotInstalledDownloads(t *testing.T) {
	h := &fakeHost{manager: "apt", installed: map[string]bool{}}
	withFakeHost(t, h)

	got, err := landingPaths(context.Background(), aptHost, "nginx", false)
	if err != nil {
		t.Fatalf("landingPaths: %v", err)
	}
	if len(h.dirCalls) == 0 {
		t.Fatalf("没有任何经 runCmdInDir 的调用:下载必须指定工作目录")
	}
	dc := h.dirCalls[0]
	if dc.name != "/usr/bin/apt-get" || len(dc.args) == 0 || dc.args[0] != "download" {
		t.Fatalf("dirCall = %+v, want apt-get download", dc)
	}
	if !filepath.IsAbs(dc.dir) {
		t.Fatalf("下载目录 = %q, want 临时目录的绝对路径", dc.dir)
	}

	var sawDebC bool
	for _, c := range h.calls {
		if c[0] != "dpkg-deb" || len(c) < 3 || c[1] != "-c" {
			continue
		}
		sawDebC = true
		if !strings.HasSuffix(c[2], ".deb") {
			t.Errorf("dpkg-deb -c 参数 = %q, want 一个 .deb", c[2])
		}
		if !strings.HasPrefix(c[2], dc.dir) {
			t.Errorf("dpkg-deb -c 参数 = %q, want 落在 %q 里", c[2], dc.dir)
		}
	}
	if !sawDebC {
		t.Fatalf("没有调用 dpkg-deb -c")
	}
	if len(got.paths) == 0 {
		t.Fatalf("paths = %v, want 非空", got.paths)
	}
}

// dnf 已装:rpm -ql 本地清单。
func TestLandingPaths_DnfInstalledUsesRpmQl(t *testing.T) {
	h := &fakeHost{manager: "dnf", installed: map[string]bool{"nginx": true}}
	h.replies = map[string]fakeReply{
		"rpm -ql nginx": {out: "/usr/sbin/nginx\n/etc/nginx/nginx.conf\n"},
	}
	withFakeHost(t, h)

	got, err := landingPaths(context.Background(), dnfHost, "nginx", true)
	if err != nil {
		t.Fatalf("landingPaths: %v", err)
	}
	if got.skipped != "" {
		t.Errorf("skipped = %q, want 空", got.skipped)
	}
	want := []string{"/etc/nginx/nginx.conf", "/usr/sbin/nginx"}
	if strings.Join(got.paths, "|") != strings.Join(want, "|") {
		t.Fatalf("paths = %v, want %v", got.paths, want)
	}
	for _, c := range h.calls {
		if len(c) > 1 && c[1] == "download" {
			t.Fatalf("已装包不该下载: %v", c)
		}
	}
}

// dnf 未装:dnf download --destdir <tmp> 之后 rpm -qlp <tmp>/*.rpm。
func TestLandingPaths_DnfNotInstalledDownloads(t *testing.T) {
	h := &fakeHost{manager: "dnf", installed: map[string]bool{}}
	withFakeHost(t, h)

	got, err := landingPaths(context.Background(), dnfHost, "nginx", false)
	if err != nil {
		t.Fatalf("landingPaths: %v", err)
	}
	if len(h.dirCalls) == 0 {
		t.Fatalf("没有任何经 runCmdInDir 的调用")
	}
	dc := h.dirCalls[0]
	if dc.name != "/usr/bin/dnf" || len(dc.args) < 2 || dc.args[0] != "download" {
		t.Fatalf("dirCall = %+v, want dnf download", dc)
	}
	if !containsArgPair(dc.args, "--destdir", dc.dir) {
		t.Errorf("argv = %v, want --destdir %s", dc.args, dc.dir)
	}
	var sawQlp bool
	for _, c := range h.calls {
		if c[0] == "rpm" && len(c) >= 3 && c[1] == "-qlp" {
			sawQlp = true
			if !strings.HasSuffix(c[2], ".rpm") || !strings.HasPrefix(c[2], dc.dir) {
				t.Errorf("rpm -qlp 参数 = %q, want %q 下的 .rpm", c[2], dc.dir)
			}
		}
	}
	if !sawQlp {
		t.Fatalf("没有调用 rpm -qlp")
	}
	if len(got.paths) == 0 {
		t.Fatalf("paths = %v, want 非空", got.paths)
	}
}

// 工具缺失(dnf-plugins-core 没装 → dnf 报 No such command)→ 不报错、skipped
// 非空:这是宿主环境问题,不该让整个发行版不能装包。
func TestLandingPaths_ToolMissingSkips(t *testing.T) {
	h := &fakeHost{manager: "dnf", installed: map[string]bool{}}
	h.replies = map[string]fakeReply{
		"/usr/bin/dnf download": {out: "Error: No such command: download\n", err: errors.New("exit status 1")},
	}
	withFakeHost(t, h)

	got, err := landingPaths(context.Background(), dnfHost, "nginx", false)
	if err != nil {
		t.Fatalf("工具缺失不该报错: %v", err)
	}
	if got.skipped == "" {
		t.Fatalf("skipped = 空, want 说明本次未体检")
	}
	if len(got.paths) != 0 {
		t.Errorf("paths = %v, want 空", got.paths)
	}
}

// 工具在但这次查不到(包不存在)→ 报错(调用方据此拒绝),且不得伪装成
// "工具缺失"。
func TestLandingPaths_DownloadFailsIsError(t *testing.T) {
	h := &fakeHost{manager: "apt", installed: map[string]bool{}}
	h.replies = map[string]fakeReply{
		"/usr/bin/apt-get download": {out: "E: Unable to locate package nope\n", err: errors.New("exit status 100")},
	}
	withFakeHost(t, h)

	got, err := landingPaths(context.Background(), aptHost, "nope", false)
	if err == nil {
		t.Fatalf("下载失败必须报错")
	}
	if !errors.Is(err, ErrPersistent) {
		t.Errorf("err = %v, want ErrPersistent", err)
	}
	if got.skipped != "" {
		t.Errorf("skipped = %q, want 空(这是查不到,不是工具缺失)", got.skipped)
	}
}

// 下载目录里没有包文件 / 多于一个包文件 → 查不到(报错)。
func TestSinglePackageFile_AmbiguousOrMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := singlePackageFile(dir, ".deb"); err == nil {
		t.Fatalf("空目录应当报错(查不到)")
	}
	if err := os.WriteFile(filepath.Join(dir, "a.deb"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := singlePackageFile(dir, ".deb"); err != nil {
		t.Fatalf("单个 .deb 应当成功: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.deb"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := singlePackageFile(dir, ".deb"); err == nil {
		t.Fatalf("两个 .deb 应当报错(多于一个算查不到)")
	}
}

// containsArgPair 报告 argv 里有没有 "--destdir <dir>" 这样的相邻参数对。
func containsArgPair(args []string, key, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == val {
			return true
		}
	}
	return false
}

// runCmdInDirProduction 必须真的把命令的工作目录钉在 dir 上 —— 这是
// "下载不许掉在 agent 工作目录里"的唯一保证。
func TestRunCmdInDirProduction_RunsInDir(t *testing.T) {
	if _, err := exec.LookPath("pwd"); err != nil {
		t.Skip("宿主没有 pwd,跳过")
	}
	dir := t.TempDir()
	out, err := runCmdInDirProduction(context.Background(), dir, "pwd")
	if err != nil {
		t.Fatalf("runCmdInDirProduction: %v", err)
	}
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(out))
	want, _ := filepath.EvalSymlinks(dir)
	if got != want {
		t.Errorf("pwd = %q, want %q", got, want)
	}
}

// 要害路径判定:命中 /boot、内核模块、固件、init;普通 unit 文件与
// 普通路径**不**命中(误报会把常规安装全灭)。
func TestCriticalLandingHit(t *testing.T) {
	cases := []struct {
		name, path string
		want       bool
	}{
		{"内核", "/boot/vmlinuz-6.8.0-49-generic", true},
		{"引导器", "/boot/efi/EFI/redhat/grubx64.efi", true},
		{"/boot 本身", "/boot", true},
		{"内核模块", "/usr/lib/modules/6.8.0-49-generic/kernel/fs/ext4/ext4.ko", true},
		{"内核模块(lib)", "/lib/modules/6.8.0-49-generic/vmlinuz", true},
		{"固件", "/usr/lib/firmware/iwlwifi-9000-pu-b0-jf-b0-46.ucode", true},
		{"init", "/usr/lib/systemd/systemd", true},
		{"init(lib)", "/lib/systemd/systemd", true},
		// 负例:路径分量边界与误报
		{"边界-bootleg", "/bootleg/x", false},
		{"边界-modulesX", "/lib/modulesX/foo", false},
		{"边界-firmwareX", "/usr/lib/firmwareX/foo", false},
		{"普通 unit", "/usr/lib/systemd/system/nginx.service", false},
		{"普通路径", "/usr/sbin/nginx", false},
		{"init 的邻居", "/usr/lib/systemd/systemd-journald", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit, ok := criticalLandingHit([]string{tc.path})
			if ok != tc.want {
				t.Fatalf("criticalLandingHit(%q) = (%q, %v), want ok=%v", tc.path, hit, ok, tc.want)
			}
			if ok && hit != tc.path {
				t.Errorf("hit = %q, want %q", hit, tc.path)
			}
		})
	}
}

// 多个路径时返回**第一个**命中的(文案与审计要用它)。
func TestCriticalLandingHit_ReturnsFirstHit(t *testing.T) {
	hit, ok := criticalLandingHit([]string{
		"/usr/sbin/nginx",
		"/etc/nginx/nginx.conf",
		"/boot/vmlinuz-6.8.0-49-generic",
		"/lib/modules/6.8.0-49-generic/vmlinuz",
	})
	if !ok {
		t.Fatalf("criticalLandingHit: 应当命中")
	}
	if hit != "/boot/vmlinuz-6.8.0-49-generic" {
		t.Errorf("hit = %q, want %q", hit, "/boot/vmlinuz-6.8.0-49-generic")
	}
}

// 空清单不命中(不是"命中",也不是错误)。
func TestCriticalLandingHit_EmptyIsNoHit(t *testing.T) {
	if hit, ok := criticalLandingHit(nil); ok {
		t.Errorf("hit = %q, ok = true, want 不命中", hit)
	}
}
