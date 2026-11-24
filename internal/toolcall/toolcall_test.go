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
	result, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range result.Tools {
		if tool.PluginID != "daedalus.fs" {
			t.Errorf("ToolInfo.PluginID = %q, want daedalus.fs", tool.PluginID)
		}
	}
	if !hasName(result.Tools, "read_file") {
		t.Error("只读工具必须在清单里")
	}
	if hasName(result.Tools, "write_file") {
		t.Error("写工具不能出现在远程工具集里")
	}
	if result.SkippedPlugins != 0 {
		t.Errorf("SkippedPlugins = %d, want 0", result.SkippedPlugins)
	}
}

func TestCaller_ListTools_SkipsReadOnlyToolsOutsideWhitelist(t *testing.T) {
	// 插件自报只读但不在白名单内,同样不可远程调用,列出来只会误导网关。
	c := newFakeCaller(t, policyWith("daedalus.shell", "shell_exec"))
	result, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(result.Tools) != 1 || result.Tools[0].Name != "shell_exec" {
		t.Errorf("工具集 = %v, want 仅 shell_exec", result.Tools)
	}
}

func TestCaller_ListTools_PartialFailureReportsSkipped(t *testing.T) {
	pol := Policy{RemoteTools: map[string]map[string]struct{}{
		"daedalus.broken": {"read_file": {}},
		"daedalus.shell":  {"shell_exec": {}},
	}}
	c := &Caller{Policy: pol, Dialer: &selectiveDialer{broken: "daedalus.broken"}}
	result, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools 不应因为单个插件坏掉而失败: %v", err)
	}
	if len(result.Tools) != 1 || result.Tools[0].PluginID != "daedalus.shell" {
		t.Errorf("工具集 = %v, want 仅 daedalus.shell 的工具", result.Tools)
	}
	if result.SkippedPlugins != 1 {
		t.Errorf("SkippedPlugins = %d, want 1(控制面要能看出清单不完整)", result.SkippedPlugins)
	}
	if result.LastErr == nil {
		t.Error("跳过插件时必须留下最后一层错误")
	}
}

func TestCaller_ListTools_AllPluginsFailedReturnsError(t *testing.T) {
	pol := Policy{RemoteTools: map[string]map[string]struct{}{
		"daedalus.a": {"read_file": {}},
		"daedalus.b": {"shell_exec": {}},
	}}
	c := &Caller{Policy: pol, Dialer: &alwaysFailDialer{}}
	result, err := c.ListTools(context.Background())
	if err == nil {
		t.Fatal("全部白名单插件都起不来时必须报错,不能伪装成空工具集成功")
	}
	if len(result.Tools) != 0 {
		t.Errorf("工具集 = %v, want 空", result.Tools)
	}
	if result.SkippedPlugins != 2 {
		t.Errorf("SkippedPlugins = %d, want 2", result.SkippedPlugins)
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

func TestCaller_CallTool_DenyReasonsAreDistinct(t *testing.T) {
	cases := []struct {
		name   string
		plugin string
		tool   string
		pol    Policy
		want   DenyReason
	}{
		{
			name:   "白名单未命中",
			plugin: "daedalus.shell",
			tool:   "write_file",
			pol:    policyWith("daedalus.shell", "shell_exec"),
			want:   DenyNotAllowlisted,
		},
		{
			name:   "插件自报非只读",
			plugin: "daedalus.fs",
			tool:   "write_file",
			pol:    policyWith("daedalus.fs", "write_file"),
			want:   DenyNotReadOnly,
		},
		{
			name:   "插件未暴露该工具",
			plugin: "daedalus.shell",
			tool:   "ghost_tool",
			pol:    policyWith("daedalus.shell", "ghost_tool"),
			want:   DenyNotExposed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Caller{Policy: tc.pol, Dialer: &fakeDialer{t: t}}
			_, err := c.CallTool(context.Background(), tc.plugin, tc.tool, `{}`)
			if !errors.Is(err, ErrDenied) {
				t.Fatalf("必须被拒,得到 %v", err)
			}
			if got := DenyReasonOf(err); got != tc.want {
				t.Errorf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCaller_CallTool_DeniedWhenAnnotationsMissing(t *testing.T) {
	// 插件不自报 Annotations 时 ReadOnlyHint 缺省 false,必须按写工具拒绝。
	c := newFakeCaller(t, policyWith("daedalus.shell", "no_annotations"))
	_, err := c.CallTool(context.Background(), "daedalus.shell", "no_annotations", `{}`)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("无 Annotations 的工具必须被拒,得到 %v", err)
	}
	if got := DenyReasonOf(err); got != DenyNotReadOnly {
		t.Errorf("reason = %q, want %q", got, DenyNotReadOnly)
	}
}

func TestCaller_PolicyReloadRevokesTool(t *testing.T) {
	// Register 每次重读策略文件算 digest;执行侧也必须重读,否则撤销要等重启。
	path := filepath.Join(t.TempDir(), "policy.toml")
	writePolicy := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("写策略: %v", err)
		}
	}
	writePolicy("schema = \"daedalus.policy/v0\"\n\n[remote_tools]\n\"daedalus.shell\" = [\"shell_exec\"]\n")
	c := &Caller{PolicyPath: path, Dialer: &fakeDialer{t: t}}

	result, err := c.ListTools(context.Background())
	if err != nil || len(result.Tools) != 1 {
		t.Fatalf("撤销前应列出 shell_exec,得到 %v / %v", result.Tools, err)
	}
	if _, err := c.CallTool(context.Background(), "daedalus.shell", "shell_exec", `{}`); err != nil {
		t.Fatalf("撤销前应放行: %v", err)
	}

	// 运维从白名单删掉工具,不重启 agent。
	writePolicy("schema = \"daedalus.policy/v0\"\n")
	result, err = c.ListTools(context.Background())
	if err != nil || len(result.Tools) != 0 {
		t.Fatalf("撤销后工具集应为空,得到 %v / %v", result.Tools, err)
	}
	_, err = c.CallTool(context.Background(), "daedalus.shell", "shell_exec", `{}`)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("撤销后必须立即拒绝,得到 %v", err)
	}
	if got := DenyReasonOf(err); got != DenyNotAllowlisted {
		t.Errorf("reason = %q, want %q", got, DenyNotAllowlisted)
	}
}

func TestCaller_PolicyUnavailableFailsClosed(t *testing.T) {
	c := &Caller{
		PolicyPath: filepath.Join(t.TempDir(), "missing.toml"),
		Dialer:     &fakeDialer{t: t},
	}
	if _, err := c.ListTools(context.Background()); !errors.Is(err, ErrPolicyUnavailable) {
		t.Errorf("ListTools: 策略读不到必须 fail-closed,得到 %v", err)
	}
	if _, err := c.CallTool(context.Background(), "daedalus.shell", "shell_exec", `{}`); !errors.Is(err, ErrPolicyUnavailable) {
		t.Errorf("CallTool: 策略读不到必须 fail-closed,得到 %v", err)
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

func TestSpawnDialer_ChildEnvIsIsolated(t *testing.T) {
	root := t.TempDir()
	writePluginManifest(t, root, "daedalus.shell", `{
		"id": "daedalus.shell",
		"executable": "bin/daedalus-shell",
		"runtime": {"name": "native"}
	}`)
	t.Setenv(pluginDirEnv, root)
	// agent 环境里的 ALLOW_COMMANDS(REPLACE 语义)绝不能漏进插件。
	t.Setenv("ALLOW_COMMANDS", "rm")
	t.Setenv("DAEDALUS_POLICY_PATH", "/tmp/agent-policy.toml")

	cmd, err := NewSpawnDialer("test").(*spawnDialer).command(context.Background(), "daedalus.shell")
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	if want := filepath.Join(root, "daedalus.shell"); cmd.Dir != want {
		t.Errorf("Dir = %q, want %q", cmd.Dir, want)
	}
	env := strings.Join(cmd.Env, "\n")
	for _, want := range []string{
		"PATH=",
		"DAEDALUS_PLUGIN_DIR=" + root,
		"DAEDALUS_POLICY_PATH=/tmp/agent-policy.toml",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("子进程环境缺 %q,得到:\n%s", want, env)
		}
	}
	if strings.Contains(env, "ALLOW_COMMANDS") {
		t.Errorf("子进程环境不得继承 ALLOW_COMMANDS:\n%s", env)
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
