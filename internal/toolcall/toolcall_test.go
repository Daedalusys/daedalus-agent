package toolcall

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPolicy_EmptyPathMeansNoRemoteSurface(t *testing.T) {
	p, err := LoadPolicy("")
	if err != nil {
		t.Fatalf("空路径不应报错: %v", err)
	}
	if p.Allows("daedalus.shell", "shell_exec") {
		t.Error("空策略必须不允许任何远程调用")
	}
}

func TestLoadPolicy_MissingRemoteToolsSectionMeansEmpty(t *testing.T) {
	// testdata/policy-no-remote.toml 只有 schema/mode 两行
	p, err := LoadPolicy("testdata/policy-no-remote.toml")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(p.RemoteTools) != 0 {
		t.Errorf("缺 [remote_tools] 时必须为空,得到 %v", p.RemoteTools)
	}
}

func TestLoadPolicy_MalformedSectionFailsClosed(t *testing.T) {
	// testdata/policy-bad-remote.toml 的 [remote_tools] 值不是字符串数组
	if _, err := LoadPolicy("testdata/policy-bad-remote.toml"); err == nil {
		t.Error("[remote_tools] 形状非法必须报错")
	}
}

func TestLoadPolicy_UnreadableFileIsError(t *testing.T) {
	if _, err := LoadPolicy("testdata/does-not-exist.toml"); err == nil {
		t.Error("策略文件读不到必须报错(部署事故),不能当成空策略")
	}
}

func TestLoadPolicy_ErrorIncludesPolicyPath(t *testing.T) {
	// 值班人靠报错定位是哪个文件;路径必须在错误文本里。
	cases := []string{
		"testdata/policy-bad-remote.toml",
		"testdata/does-not-exist.toml",
	}
	for _, path := range cases {
		_, err := LoadPolicy(path)
		if err == nil {
			t.Fatalf("%s: 期望报错", path)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("%s: 错误信息必须带策略路径,得到 %v", path, err)
		}
	}
}

func TestLoadPolicy_UnknownTopLevelKeysAreIgnored(t *testing.T) {
	// 策略文件由控制面下发,顶层键会演进;多一个键不能把远程面整段打掉。
	p, err := LoadPolicy("testdata/policy-unknown-keys.toml")
	if err != nil {
		t.Fatalf("顶层未知键不应报错: %v", err)
	}
	if !p.Allows("daedalus.shell", "shell_exec") {
		t.Error("[remote_tools] 必须照常解析")
	}
}

func TestLoadPolicy_EmptyToolNameIsError(t *testing.T) {
	if _, err := LoadPolicy("testdata/policy-empty-tool.toml"); err == nil {
		t.Error("[remote_tools] 里的空工具名必须报错")
	}
}

func TestPolicy_Allows(t *testing.T) {
	// testdata/policy-remote.toml:daedalus.shell → shell_exec
	p, err := LoadPolicy("testdata/policy-remote.toml")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !p.Allows("daedalus.shell", "shell_exec") {
		t.Error("白名单内的工具必须放行")
	}
	if p.Allows("daedalus.shell", "write_file") {
		t.Error("白名单外的工具必须不放行")
	}
	if p.Allows("daedalus.fs", "read_file") {
		t.Error("别的 plugin 的白名单不能串")
	}
}

func TestCaller_CallTool_DeniedWhenNotAllowlisted(t *testing.T) {
	c := newFakeCaller(t, policyWith("daedalus.shell", "shell_exec"))
	_, err := c.CallTool(context.Background(), "daedalus.shell", "write_file", `{}`)
	if !errors.Is(err, ErrDenied) {
		t.Errorf("白名单外的工具必须被拒,得到 %v", err)
	}
}

func TestCaller_CallTool_RejectsWriteToolEvenIfAllowlisted(t *testing.T) {
	// 白名单里就有 write_file,但假插件把它标成 ReadOnly=false
	c := newFakeCaller(t, policyWith("daedalus.fs", "write_file"))
	_, err := c.CallTool(context.Background(), "daedalus.fs", "write_file", `{}`)
	if !errors.Is(err, ErrDenied) {
		t.Errorf("写工具即使在白名单里也必须被拒,得到 %v", err)
	}
}

func TestCaller_CallTool_PassesThroughToolError(t *testing.T) {
	c := newFakeCaller(t, policyWith("daedalus.shell", "shell_exec"))
	res, err := c.CallTool(context.Background(), "daedalus.shell", "shell_exec", `{"boom":true}`)
	if err != nil {
		t.Fatalf("插件自报错误不是调用失败: %v", err)
	}
	if !res.IsError {
		t.Error("插件自报错误必须原样透传(IsError=true)")
	}
	if res.ErrorMessage == "" {
		t.Error("插件自报错误必须带可读原因")
	}
}

func TestCaller_CallTool_PassesContentThrough(t *testing.T) {
	c := newFakeCaller(t, policyWith("daedalus.shell", "shell_exec"))
	res, err := c.CallTool(context.Background(), "daedalus.shell", "shell_exec", `{"command":"df"}`)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Error("正常调用不应标成业务错误")
	}
	if !json.Valid([]byte(res.ContentJSON)) {
		t.Errorf("ContentJSON 必须是合法 JSON,得到 %q", res.ContentJSON)
	}
	if !strings.Contains(res.ContentJSON, "total 0") {
		t.Errorf("ContentJSON 必须带插件返回原文,得到 %q", res.ContentJSON)
	}
}

func TestCaller_CallTool_EmptyArgumentsAreAllowed(t *testing.T) {
	c := newFakeCaller(t, policyWith("daedalus.shell", "shell_exec"))
	res, err := c.CallTool(context.Background(), "daedalus.shell", "shell_exec", "")
	if err != nil {
		t.Fatalf("空入参应按 {} 处理: %v", err)
	}
	if res.IsError {
		t.Error("空入参不是业务错误")
	}
}

func TestCaller_ListTools_OnlyReturnsReadOnly(t *testing.T) {
	c := newFakeCaller(t, policyWith("daedalus.fs", "read_file", "write_file"))
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range tools {
		if tool.PluginID != "daedalus.fs" {
			t.Errorf("ToolInfo.PluginID = %q, want daedalus.fs", tool.PluginID)
		}
	}
	if !hasName(tools, "read_file") {
		t.Error("只读工具必须在清单里")
	}
	if hasName(tools, "write_file") {
		t.Error("写工具不能出现在远程工具集里")
	}
}

func TestCaller_ListTools_SkipsReadOnlyToolsOutsideWhitelist(t *testing.T) {
	// 插件自报只读但不在白名单内,同样不可远程调用,列出来只会误导网关。
	c := newFakeCaller(t, policyWith("daedalus.shell", "shell_exec"))
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "shell_exec" {
		t.Errorf("工具集 = %v, want 仅 shell_exec", tools)
	}
}

func TestCaller_ListTools_SkipsUnreachablePlugin(t *testing.T) {
	pol := Policy{RemoteTools: map[string]map[string]struct{}{
		"daedalus.broken": {"read_file": {}},
		"daedalus.shell":  {"shell_exec": {}},
	}}
	c := &Caller{Policy: pol, Dialer: &selectiveDialer{broken: "daedalus.broken"}}
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools 不应因为单个插件坏掉而失败: %v", err)
	}
	if len(tools) != 1 || tools[0].PluginID != "daedalus.shell" {
		t.Errorf("工具集 = %v, want 仅 daedalus.shell 的工具", tools)
	}
}

func TestCaller_CallTool_DialFailureIsNotDenied(t *testing.T) {
	c := &Caller{Policy: policyWith("daedalus.shell", "shell_exec"), Dialer: &selectiveDialer{broken: "daedalus.shell"}}
	_, err := c.CallTool(context.Background(), "daedalus.shell", "shell_exec", `{}`)
	if err == nil {
		t.Fatal("插件起不来必须报错")
	}
	if errors.Is(err, ErrDenied) {
		t.Error("插件不可达不是放行判定被拒,错误类型必须能区分")
	}
}

func TestSpawnDialer_CommandComesFromManifest(t *testing.T) {
	root := t.TempDir()
	writePluginManifest(t, root, "daedalus.shell", `{
		"id": "daedalus.shell",
		"executable": "bin/daedalus-shell",
		"runtime": {"name": "native"},
		"entrypoint": ["--stdio"]
	}`)
	t.Setenv(pluginDirEnv, root)

	d := NewSpawnDialer("test")
	cmd, err := d.(*spawnDialer).command(context.Background(), "daedalus.shell")
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	wantPath := filepath.Join(root, "daedalus.shell", "bin", "daedalus-shell")
	if cmd.Path != wantPath {
		t.Errorf("Path = %q, want %q", cmd.Path, wantPath)
	}
	if len(cmd.Args) != 2 || cmd.Args[1] != "--stdio" {
		t.Errorf("Args = %v, want [binary --stdio]", cmd.Args)
	}
}

func TestSpawnDialer_DenoRuntimePrependsRun(t *testing.T) {
	root := t.TempDir()
	writePluginManifest(t, root, "daedalus.deno", `{
		"id": "daedalus.deno",
		"executable": "server.ts",
		"runtime": {"name": "deno"},
		"entrypoint": ["--allow-read"]
	}`)
	t.Setenv(pluginDirEnv, root)

	d := NewSpawnDialer("test")
	cmd, err := d.(*spawnDialer).command(context.Background(), "daedalus.deno")
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	if cmd.Path != denoBinary {
		t.Errorf("Path = %q, want %q", cmd.Path, denoBinary)
	}
	wantTail := filepath.Join(root, "daedalus.deno", "server.ts")
	if len(cmd.Args) != 4 || cmd.Args[1] != "run" || cmd.Args[2] != "--allow-read" || cmd.Args[3] != wantTail {
		t.Errorf("Args = %v, want [deno run --allow-read %s]", cmd.Args, wantTail)
	}
}

func TestSpawnDialer_RejectsPathEscapes(t *testing.T) {
	root := t.TempDir()
	writePluginManifest(t, root, "daedalus.evil", `{
		"id": "daedalus.evil",
		"executable": "../escape",
		"runtime": {"name": "native"}
	}`)
	t.Setenv(pluginDirEnv, root)

	d := NewSpawnDialer("test")
	if _, err := d.(*spawnDialer).command(context.Background(), "daedalus.evil"); err == nil {
		t.Error("executable 逃出插件目录必须报错")
	}
	if _, err := d.(*spawnDialer).command(context.Background(), "../daedalus.evil"); err == nil {
		t.Error("非法 plugin id 必须报错")
	}
}

func writePluginManifest(t *testing.T, root, pluginID, body string) {
	t.Helper()
	dir := filepath.Join(root, pluginID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "daedalus.plugin.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}
