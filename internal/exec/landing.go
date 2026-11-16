package exec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 落点体检:在装包**之前**先看这个包会往哪落文件,命中要害路径就拒绝。
//
// 为什么要在 agent 侧做:包文件在目标机上,控制面拿不到。控制面现有的
// "包名前缀表"只是快路径(离线、零成本),换个发行版换个包名就漏判 ——
// 而漏判的后果正是高危变更被当成低危、失败还被自动重试。所以这里是
// 权威兜底,与前缀表叠加,不取代它。
//
// 落点来源分两种,差别只在"要不要联网":
//
//   - 已装:本机清单(dpkg-query -L / rpm -ql)。升级路径不该因为仓库不可达
//     就被判成"查不出"。
//   - 未装:下载到临时目录再列(dpkg-deb -c / rpm -qlp)。这里必须联网,
//     所以网络不通就是"查不到",由调用方按 fail-closed 拒绝。

// landingResult 是"这个包会往哪落文件"的体检结果。
//
// paths 与 skipped 都不是"空即是安全",调用方必须分清三种形状:
//
//   - paths 非空:落点清单(去重、排序,断言与日志都稳定)。
//   - skipped 非空:**工具缺失**,本次没有体检(宿主环境问题)。调用方放行
//     并留痕 —— 否则整个发行版都装不了包。
//   - 两者都空:**查不到**(工具在,但这次没查出来)。调用方必须拒绝
//     (fail-closed):判不出 ≠ 安全。
type landingResult struct {
	paths   []string
	skipped string
}

// landingPaths 取包的落点清单。
//
// installed 决定走哪条路:已装走本机清单,未装走下载。调用方(如
// packageExecutor.Apply)已经把"是否已装"查过一次,这里直接复用,不再查第二遍。
func landingPaths(ctx context.Context, mgr hostManager, name string, installed bool) (landingResult, error) {
	switch mgr.name {
	case "apt":
		if installed {
			return aptInstalledPaths(ctx, name)
		}
		return aptDownloadPaths(ctx, mgr, name)
	case "dnf":
		if installed {
			return dnfInstalledPaths(ctx, name)
		}
		return dnfDownloadPaths(ctx, mgr, name)
	}
	return landingResult{}, fmt.Errorf("%w: unsupported host manager %q", ErrPersistent, mgr.name)
}

// aptInstalledPaths 用 dpkg-query -L 列已装包的落点。不联网。
func aptInstalledPaths(ctx context.Context, name string) (landingResult, error) {
	out, err := runCmd(ctx, "dpkg-query", "-L", name)
	if err != nil {
		return landingResult{}, fmt.Errorf("%w: dpkg-query -L %s failed: %s (%v)", ErrPersistent, name, firstLine(out), err)
	}
	return landingResult{paths: normalizePathList(out)}, nil
}

// aptDownloadPaths 下载 .deb 到临时目录,再用 dpkg-deb -c 列内容。
func aptDownloadPaths(ctx context.Context, mgr hostManager, name string) (landingResult, error) {
	tmp, err := os.MkdirTemp("", "daedalus-landing-*")
	if err != nil {
		return landingResult{}, fmt.Errorf("%w: cannot create temp dir for landing check: %v", ErrPersistent, err)
	}
	// 临时目录必须清理 —— 下载下来的包文件留在宿主上是脏数据。
	defer os.RemoveAll(tmp)

	// apt-get download 把文件下到**当前工作目录**,所以必须用 runCmdInDir
	// 把工作目录钉在临时目录里。
	out, err := runCmdInDir(ctx, tmp, mgr.bin, "download", name)
	if err != nil {
		if isToolMissing(out) {
			return landingResult{skipped: fmt.Sprintf("apt-get download 不可用: %s", firstLine(out))}, nil
		}
		return landingResult{}, fmt.Errorf("%w: apt-get download %s failed: %s (%v)", ErrPersistent, name, firstLine(out), err)
	}

	deb, err := singlePackageFile(tmp, ".deb")
	if err != nil {
		return landingResult{}, err
	}
	listOut, err := runCmd(ctx, "dpkg-deb", "-c", deb)
	if err != nil {
		return landingResult{}, fmt.Errorf("%w: dpkg-deb -c %s failed: %s (%v)", ErrPersistent, deb, firstLine(listOut), err)
	}
	return landingResult{paths: normalizeDebContents(listOut)}, nil
}

// dnfInstalledPaths 用 rpm -ql 列已装包的落点。不联网。
func dnfInstalledPaths(ctx context.Context, name string) (landingResult, error) {
	out, err := runCmd(ctx, "rpm", "-ql", name)
	if err != nil {
		return landingResult{}, fmt.Errorf("%w: rpm -ql %s failed: %s (%v)", ErrPersistent, name, firstLine(out), err)
	}
	return landingResult{paths: normalizePathList(out)}, nil
}

// dnfDownloadPaths 下载 .rpm 到临时目录,再用 rpm -qlp 列内容。
//
// 工具缺失在这里最常见:dnf download 是 dnf-plugins-core 提供的,最小化
// 安装的宿主上常常没有 —— 那是**宿主环境**问题,不该让整台机器装不了包,
// 故跳过体检并留痕,而不是拒绝。
func dnfDownloadPaths(ctx context.Context, mgr hostManager, name string) (landingResult, error) {
	tmp, err := os.MkdirTemp("", "daedalus-landing-*")
	if err != nil {
		return landingResult{}, fmt.Errorf("%w: cannot create temp dir for landing check: %v", ErrPersistent, err)
	}
	defer os.RemoveAll(tmp)

	out, err := runCmdInDir(ctx, tmp, mgr.bin, "download", "--destdir", tmp, name)
	if err != nil {
		if isToolMissing(out) {
			return landingResult{skipped: fmt.Sprintf("dnf download 不可用(dnf-plugins-core 未装): %s", firstLine(out))}, nil
		}
		return landingResult{}, fmt.Errorf("%w: dnf download %s failed: %s (%v)", ErrPersistent, name, firstLine(out), err)
	}

	rpmFile, err := singlePackageFile(tmp, ".rpm")
	if err != nil {
		return landingResult{}, err
	}
	listOut, err := runCmd(ctx, "rpm", "-qlp", rpmFile)
	if err != nil {
		return landingResult{}, fmt.Errorf("%w: rpm -qlp %s failed: %s (%v)", ErrPersistent, rpmFile, firstLine(listOut), err)
	}
	return landingResult{paths: normalizePathList(listOut)}, nil
}

// singlePackageFile 在 dir 里找唯一一个后缀为 ext 的包文件。
//
// 找不到、或找到多于一个,都算"查不到":多于一个说明下载命令拉回来的东西
// 不唯一,继续猜就是把猜测当事实。
func singlePackageFile(dir, ext string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("%w: cannot read temp dir %s: %v", ErrPersistent, dir, err)
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ext) {
			continue
		}
		found = append(found, filepath.Join(dir, e.Name()))
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("%w: no %s package file found in %s", ErrPersistent, ext, dir)
	default:
		return "", fmt.Errorf("%w: %d %s package files found in %s (expected exactly one)", ErrPersistent, len(found), ext, dir)
	}
}

// normalizePathList 把清单文本归一成落点路径:去空行、去 "."、只留以 / 开头的
// 绝对路径,去重并排序。
func normalizePathList(out string) []string {
	seen := map[string]bool{}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		p := strings.TrimSpace(line)
		if p == "" || p == "." || !strings.HasPrefix(p, "/") {
			continue
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// normalizeDebContents 解析 dpkg-deb -c 的输出。
//
// 普通行形如 "drwxr-xr-x root/root 0 2024-01-01 00:00 ./usr/sbin/" —— 路径从
// **第 6 列**开始(按空格拼到底,文件名可以带空格),且带前导 "./"。
// 符号链接行多一段 "-> <目标>":目标是别人家的位置,不是本包的落点,必须切掉。
func normalizeDebContents(out string) []string {
	var raw []string
	for _, line := range strings.Split(out, "\n") {
		p, ok := debEntryPath(line)
		if !ok {
			continue
		}
		raw = append(raw, p)
	}
	return normalizePathList(strings.Join(raw, "\n"))
}

// debEntryPath 从一行 dpkg-deb -c 里取出该条目的落点路径。
//
// 两种行都要能吃:普通文件/目录(最后一列就是路径)与符号链接(最后一个
// 路径样式的列是目标)。分不清的目标宁可切掉 —— 把链接目标当落点会误拒,
// 把链接自己的位置丢掉会漏判。
func debEntryPath(line string) (string, bool) {
	if i := strings.LastIndex(line, " -> "); i >= 0 {
		line = line[:i]
	}
	fields := strings.Fields(line)
	// 前五列是权限、属主、大小、日期、时间。
	if len(fields) < 6 {
		return "", false
	}
	p := strings.Join(fields[5:], " ")
	if !strings.HasPrefix(p, "./") {
		return "", false
	}
	return "/" + strings.TrimPrefix(p, "./"), true
}

// criticalLandingPrefixes / criticalLandingExact 是要害路径集(包级常量,
// 不进 policy.toml:agent 侧根本不读 policy.toml,而那一节的
// high_risk_path_prefixes 是 file.set 的**目标路径**,与包的落点不是一回事)。
//
// 集合刻意窄:多收一条就会把常规安装全灭(典型反例:/usr/lib/systemd/system
// 里全是普通 unit 文件,收进来 nginx 这类包会被判高危)。
var (
	// 前缀项按**路径分量**匹配:/boot 命中 /boot/vmlinuz,不命中 /bootleg/x。
	criticalLandingPrefixes = []string{
		"/boot",             // 内核与引导器
		"/lib/modules",      // 内核模块
		"/usr/lib/modules",  // 内核模块(Debian/Ubuntu 布局)
		"/usr/lib/firmware", // 固件
	}
	// 精确项只认路径本身。
	criticalLandingExact = []string{
		"/lib/systemd/systemd",     // init
		"/usr/lib/systemd/systemd", // init(Debian/Ubuntu 布局)
	}
)

// criticalLandingHit 判定落点清单里有没有要害路径,返回**第一个**命中的
// 路径(用于拒绝文案与审计)与是否命中。
//
// 匹配按路径分量,不是 strings.HasPrefix:/boot 是目录,不是 /bootleg 的
// 前缀。包里的普通数据文件(sysctl 片段、unit 文件、可执行文件)一律放行。
func criticalLandingHit(paths []string) (string, bool) {
	for _, p := range paths {
		for _, exact := range criticalLandingExact {
			if p == exact {
				return p, true
			}
		}
		for _, prefix := range criticalLandingPrefixes {
			if p == prefix || strings.HasPrefix(p, prefix+"/") {
				return p, true
			}
		}
	}
	return "", false
}

// isToolMissing 报告命令输出是不是"这台机器上根本没有这个子命令"。
//
// 这类失败是**宿主环境**问题(dnf download 来自 dnf-plugins-core),与
// "这次变更查不到落点"必须分开处置:前者跳过体检,后者拒绝。
func isToolMissing(out string) bool {
	low := strings.ToLower(out)
	for _, p := range []string{
		"no such command",
		"unknown command",
		"command not found",
	} {
		if strings.Contains(low, p) {
			return true
		}
	}
	return false
}
