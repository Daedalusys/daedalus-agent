package rpc_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/audit"
	"github.com/Daedalusys/daedalus-agent/internal/rpc"
	"github.com/Daedalusys/daedalus-agent/internal/toolcall"
)

// toolStubSession 是 Caller 下游的假插件会话,只回预置结果。
type toolStubSession struct {
	tools  []toolcall.ToolInfo
	result toolcall.ToolResult
	err    error

	gotName string
	gotArgs string
	closed  bool
}

func (s *toolStubSession) ListTools(context.Context) ([]toolcall.ToolInfo, error) {
	return s.tools, nil
}

func (s *toolStubSession) CallTool(_ context.Context, name, argsJSON string) (toolcall.ToolResult, error) {
	s.gotName = name
	s.gotArgs = argsJSON
	return s.result, s.err
}

func (s *toolStubSession) Close() error {
	s.closed = true
	return nil
}

// toolStubDialer 把插件 id 记下来;failPlugins 里的插件 dial 失败。
type toolStubDialer struct {
	session *toolStubSession
	err     error
	// failPlugins 是 dial 失败的插件 id 集合,用来模拟部分插件起不来。
	failPlugins map[string]bool

	gotPlugin string
}

func (d *toolStubDialer) Dial(_ context.Context, pluginID string) (toolcall.Session, error) {
	d.gotPlugin = pluginID
	if d.failPlugins[pluginID] {
		return nil, errors.New("插件进程起不来")
	}
	if d.err != nil {
		return nil, d.err
	}
	return d.session, nil
}

func remotePolicy(pluginID string, tools ...string) toolcall.Policy {
	set := make(map[string]struct{}, len(tools))
	for _, name := range tools {
		set[name] = struct{}{}
	}
	return toolcall.Policy{RemoteTools: map[string]map[string]struct{}{pluginID: set}}
}

// startToolRig 起一个注入了 ToolCaller 的 rig。
func startToolRig(t *testing.T, pol toolcall.Policy, dialer toolcall.Dialer) *testRig {
	t.Helper()
	return startToolCallerRig(t, &toolcall.Caller{Policy: pol, Dialer: dialer})
}

// startToolCallerRig 起一个注入了指定 Caller 的 rig。
func startToolCallerRig(t *testing.T, caller *toolcall.Caller) *testRig {
	t.Helper()
	return startTestServerFull(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET, nil,
		func(deps *rpc.ServiceDeps) {
			deps.ToolCaller = caller
		})
}

// findAudit 在审计链里找第一条 tool 相同、outcome 以 want 开头的记录。
// outcome 带前缀匹配:既有词表里失败结局都带 ": <detail>"。
func findAudit(t *testing.T, chain *audit.Local, tool, want string) (audit.Record, bool) {
	t.Helper()
	records, _, err := chain.Since(0)
	if err != nil {
		t.Fatalf("audit Since: %v", err)
	}
	for _, rec := range records {
		if rec.Tool == tool && strings.HasPrefix(rec.Outcome, want) {
			return rec, true
		}
	}
	return audit.Record{}, false
}

func TestCallTool_DeniedWhenNoRemoteSurface(t *testing.T) {
	rig := startTestServerWithSnapshot(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := rig.Client.CallTool(ctx, &agentv1.CallToolRequest{PluginId: "daedalus.shell", ToolName: "shell_exec"})
	if err == nil {
		t.Fatal("未配置工具调用面时 CallTool 必须被拒")
	}
	if code := status.Code(err); code != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", code)
	}
	if _, ok := findAudit(t, rig.Audit, "tool_call", "refused: no_remote_surface"); !ok {
		t.Error("被拒的调用必须落 refused 审计")
	}
}

func TestCallTool_PermissionDeniedWhenNotAllowlisted(t *testing.T) {
	dialer := &toolStubDialer{session: &toolStubSession{}}
	rig := startToolRig(t, toolcall.Policy{}, dialer)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := rig.Client.CallTool(ctx, &agentv1.CallToolRequest{PluginId: "daedalus.shell", ToolName: "shell_exec"})
	if err == nil {
		t.Fatal("空白名单时 CallTool 必须被拒")
	}
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", code)
	}
	if dialer.gotPlugin != "" {
		t.Error("白名单判定不通过时不应该去 dial 插件")
	}
	record, ok := findAudit(t, rig.Audit, "tool_call", "refused: ")
	if !ok {
		t.Fatal("被拒的调用必须落 refused 审计")
	}
	if record.Args["reason"] != "not_allowlisted" {
		t.Errorf("审计 reason = %q, want not_allowlisted", record.Args["reason"])
	}
}

func TestCallTool_PermissionDeniedForWriteToolEvenIfAllowlisted(t *testing.T) {
	dialer := &toolStubDialer{session: &toolStubSession{tools: []toolcall.ToolInfo{
		{Name: "write_file", ReadOnly: false},
	}}}
	rig := startToolRig(t, remotePolicy("daedalus.fs", "write_file"), dialer)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := rig.Client.CallTool(ctx, &agentv1.CallToolRequest{PluginId: "daedalus.fs", ToolName: "write_file"})
	if err == nil {
		t.Fatal("写工具即使进了白名单也必须被拒")
	}
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", code)
	}
	record, ok := findAudit(t, rig.Audit, "tool_call", "refused: ")
	if !ok {
		t.Fatal("被拒的调用必须落 refused 审计")
	}
	if record.Args["reason"] != "not_read_only" {
		t.Errorf("审计 reason = %q, want not_read_only", record.Args["reason"])
	}
}

func TestCallTool_PermissionDeniedWhenToolNotExposed(t *testing.T) {
	// 白名单里有,但插件当前没有暴露这个工具:与「写工具」必须是不同 reason。
	dialer := &toolStubDialer{session: &toolStubSession{tools: []toolcall.ToolInfo{
		{Name: "read_file", ReadOnly: true},
	}}}
	rig := startToolRig(t, remotePolicy("daedalus.shell", "ghost_tool"), dialer)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := rig.Client.CallTool(ctx, &agentv1.CallToolRequest{PluginId: "daedalus.shell", ToolName: "ghost_tool"})
	if err == nil {
		t.Fatal("插件没暴露的工具必须被拒")
	}
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", code)
	}
	record, ok := findAudit(t, rig.Audit, "tool_call", "refused: ")
	if !ok {
		t.Fatal("被拒的调用必须落 refused 审计")
	}
	if record.Args["reason"] != "tool_not_exposed" {
		t.Errorf("审计 reason = %q, want tool_not_exposed", record.Args["reason"])
	}
}

func TestCallTool_FailedPreconditionWhenPolicyUnavailable(t *testing.T) {
	// 策略文件读不到 = fail-closed,不能用旧快照继续放行。
	caller := &toolcall.Caller{PolicyPath: t.TempDir() + "/missing.toml"}
	rig := startToolCallerRig(t, caller)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := rig.Client.CallTool(ctx, &agentv1.CallToolRequest{PluginId: "daedalus.shell", ToolName: "shell_exec"})
	if err == nil {
		t.Fatal("策略不可用时必须拒绝")
	}
	if code := status.Code(err); code != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", code)
	}
	if _, ok := findAudit(t, rig.Audit, "tool_call", "refused: policy_unavailable"); !ok {
		t.Error("策略不可用必须落 refused 审计")
	}
}

func TestCallTool_SuccessReturnsContentAndAudits(t *testing.T) {
	session := &toolStubSession{
		tools:  []toolcall.ToolInfo{{Name: "shell_exec", ReadOnly: true}},
		result: toolcall.ToolResult{ContentJSON: `[{"type":"text","text":"total 0"}]`},
	}
	dialer := &toolStubDialer{session: session}
	rig := startToolRig(t, remotePolicy("daedalus.shell", "shell_exec"), dialer)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := rig.Client.CallTool(ctx, &agentv1.CallToolRequest{
		PluginId:      "daedalus.shell",
		ToolName:      "shell_exec",
		ArgumentsJson: `{"command":"df"}`,
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if resp.GetIsError() {
		t.Error("正常调用不应标成业务错误")
	}
	if resp.GetContentJson() != `[{"type":"text","text":"total 0"}]` {
		t.Errorf("ContentJson = %q", resp.GetContentJson())
	}
	if resp.GetAuditSeq() == 0 {
		t.Error("成功调用必须返回审计 seq")
	}
	if dialer.gotPlugin != "daedalus.shell" {
		t.Errorf("dial plugin = %q", dialer.gotPlugin)
	}
	if session.gotName != "shell_exec" || session.gotArgs != `{"command":"df"}` {
		t.Errorf("透传给插件的调用 = %q/%q", session.gotName, session.gotArgs)
	}
	if !session.closed {
		t.Error("调用结束后必须关闭插件会话")
	}
	if _, ok := findAudit(t, rig.Audit, "tool_call", "ok"); !ok {
		t.Error("成功调用必须落 ok 审计")
	}
}

func TestCallTool_PluginErrorIsNotRPCError(t *testing.T) {
	dialer := &toolStubDialer{session: &toolStubSession{
		tools: []toolcall.ToolInfo{{Name: "shell_exec", ReadOnly: true}},
		result: toolcall.ToolResult{
			IsError:      true,
			ContentJSON:  `[{"type":"text","text":"权限不足"}]`,
			ErrorMessage: "权限不足",
		},
	}}
	rig := startToolRig(t, remotePolicy("daedalus.shell", "shell_exec"), dialer)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := rig.Client.CallTool(ctx, &agentv1.CallToolRequest{PluginId: "daedalus.shell", ToolName: "shell_exec"})
	if err != nil {
		t.Fatalf("插件自报错误不是 RPC 错误: %v", err)
	}
	if !resp.GetIsError() {
		t.Error("插件自报错误必须透传 IsError")
	}
	if resp.GetErrorMessage() != "权限不足" {
		t.Errorf("ErrorMessage = %q", resp.GetErrorMessage())
	}
	if resp.GetAuditSeq() == 0 {
		t.Error("失败的调用同样要落审计并返回 seq")
	}
	if _, ok := findAudit(t, rig.Audit, "tool_call", "tool_failed: "); !ok {
		t.Error("插件自报错误必须记成 tool_failed 审计")
	}
}

func TestCallTool_UnavailableWhenPluginCannotStart(t *testing.T) {
	dialer := &toolStubDialer{err: errors.New("插件进程起不来")}
	rig := startToolRig(t, remotePolicy("daedalus.shell", "shell_exec"), dialer)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := rig.Client.CallTool(ctx, &agentv1.CallToolRequest{PluginId: "daedalus.shell", ToolName: "shell_exec"})
	if err == nil {
		t.Fatal("插件起不来必须报错")
	}
	if code := status.Code(err); code != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", code)
	}
	if _, ok := findAudit(t, rig.Audit, "tool_call", "plugin_unavailable: "); !ok {
		t.Error("插件不可达必须落 plugin_unavailable 审计")
	}
}

func TestListTools_ReturnsEmptyWhenNoRemoteSurface(t *testing.T) {
	rig := startTestServerWithSnapshot(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := rig.Client.ListTools(ctx, &agentv1.ListToolsRequest{})
	if err != nil {
		t.Fatalf("未配置工具调用面时 ListTools 不应报错: %v", err)
	}
	if len(resp.GetTools()) != 0 {
		t.Errorf("tools = %v, want 空", resp.GetTools())
	}
	// RPC 成功,审计就不能记成拒绝事件。
	record, ok := findAudit(t, rig.Audit, "tool_call_list_tools", "ok")
	if !ok {
		t.Fatal("无远程面时 ListTools 必须落 ok 审计")
	}
	if record.Args["reason"] != "no_remote_surface" {
		t.Errorf("审计 reason = %q, want no_remote_surface", record.Args["reason"])
	}
}

func TestListTools_UnavailableWhenAllPluginsFail(t *testing.T) {
	dialer := &toolStubDialer{err: errors.New("插件进程起不来")}
	pol := toolcall.Policy{RemoteTools: map[string]map[string]struct{}{
		"daedalus.shell": {"shell_exec": {}},
	}}
	rig := startToolRig(t, pol, dialer)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := rig.Client.ListTools(ctx, &agentv1.ListToolsRequest{})
	if err == nil {
		t.Fatal("全部插件不可用时 ListTools 必须报错,不能伪装成空工具集")
	}
	if code := status.Code(err); code != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", code)
	}
	record, ok := findAudit(t, rig.Audit, "tool_call_list_tools", "list_failed: ")
	if !ok {
		t.Fatal("全部插件失败必须落 list_failed 审计")
	}
	if record.Args["skipped"] != "1" {
		t.Errorf("审计 skipped = %q, want 1", record.Args["skipped"])
	}
}

func TestListTools_PartialFailureReportsSkipped(t *testing.T) {
	dialer := &toolStubDialer{
		session:     &toolStubSession{tools: []toolcall.ToolInfo{{Name: "shell_exec", ReadOnly: true}}},
		failPlugins: map[string]bool{"daedalus.broken": true},
	}
	pol := toolcall.Policy{RemoteTools: map[string]map[string]struct{}{
		"daedalus.broken": {"shell_exec": {}},
		"daedalus.shell":  {"shell_exec": {}},
	}}
	rig := startToolRig(t, pol, dialer)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := rig.Client.ListTools(ctx, &agentv1.ListToolsRequest{})
	if err != nil {
		t.Fatalf("部分插件失败时 ListTools 仍应成功: %v", err)
	}
	if len(resp.GetTools()) != 1 {
		t.Errorf("tools = %v, want 1 条", resp.GetTools())
	}
	record, ok := findAudit(t, rig.Audit, "tool_call_list_tools", "ok")
	if !ok {
		t.Fatal("部分失败仍应落 ok 审计")
	}
	if record.Args["skipped"] != "1" {
		t.Errorf("审计 skipped = %q, want 1", record.Args["skipped"])
	}
	if record.Args["count"] != "1" {
		t.Errorf("审计 count = %q, want 1", record.Args["count"])
	}
}

func TestListTools_FailedPreconditionWhenPolicyUnavailable(t *testing.T) {
	caller := &toolcall.Caller{PolicyPath: t.TempDir() + "/missing.toml"}
	rig := startToolCallerRig(t, caller)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := rig.Client.ListTools(ctx, &agentv1.ListToolsRequest{})
	if err == nil {
		t.Fatal("策略不可用时 ListTools 必须失败")
	}
	if code := status.Code(err); code != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", code)
	}
	if _, ok := findAudit(t, rig.Audit, "tool_call_list_tools", "policy_unavailable: "); !ok {
		t.Error("策略不可用必须落 policy_unavailable 审计")
	}
}

func TestListTools_MapsOnlyReadOnlyAllowlistedTools(t *testing.T) {
	dialer := &toolStubDialer{session: &toolStubSession{tools: []toolcall.ToolInfo{
		{Name: "shell_exec", Description: "执行只读命令", InputSchemaJSON: `{"type":"object"}`, ReadOnly: true},
		{Name: "write_file", Description: "写文件", InputSchemaJSON: `{"type":"object"}`, ReadOnly: false},
		{Name: "read_file", Description: "读文件", InputSchemaJSON: `{"type":"object"}`, ReadOnly: true},
	}}}
	rig := startToolRig(t, remotePolicy("daedalus.shell", "shell_exec"), dialer)
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := rig.Client.ListTools(ctx, &agentv1.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(resp.GetTools()) != 1 {
		t.Fatalf("tools = %v, want 仅 1 条", resp.GetTools())
	}
	tool := resp.GetTools()[0]
	if tool.GetPluginId() != "daedalus.shell" || tool.GetToolName() != "shell_exec" {
		t.Errorf("descriptor = %v", tool)
	}
	if !tool.GetReadOnly() {
		t.Error("read_only 必须来自插件自报")
	}
	if tool.GetDescription() != "执行只读命令" || tool.GetInputSchemaJson() != `{"type":"object"}` {
		t.Errorf("描述/入参 schema 未透传: %v", tool)
	}
	if resp.GetAuditSeq() == 0 {
		t.Error("列出工具同样要落审计并返回 seq")
	}
	record, ok := findAudit(t, rig.Audit, "tool_call_list_tools", "ok")
	if !ok {
		t.Fatal("ListTools 必须落 ok 审计")
	}
	if record.Args["count"] != "1" {
		t.Errorf("审计 count = %q, want 1", record.Args["count"])
	}
	if record.Args["skipped"] != "0" {
		t.Errorf("审计 skipped = %q, want 0", record.Args["skipped"])
	}
}
