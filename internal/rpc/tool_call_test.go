package rpc_test

import (
	"context"
	"errors"
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

// toolStubDialer 把插件 id 记下来,dial 成功/失败由 err 决定。
type toolStubDialer struct {
	session *toolStubSession
	err     error

	gotPlugin string
}

func (d *toolStubDialer) Dial(_ context.Context, pluginID string) (toolcall.Session, error) {
	d.gotPlugin = pluginID
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
	return startTestServerFull(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_RSYNC_TARGET, nil,
		func(deps *rpc.ServiceDeps) {
			deps.ToolCaller = &toolcall.Caller{Policy: pol, Dialer: dialer}
		})
}

// findAudit 在审计链里找第一条匹配 (tool, outcome) 的记录。
func findAudit(t *testing.T, chain *audit.Local, tool, outcome string) (audit.Record, bool) {
	t.Helper()
	records, _, err := chain.Since(0)
	if err != nil {
		t.Fatalf("audit Since: %v", err)
	}
	for _, rec := range records {
		if rec.Tool == tool && rec.Outcome == outcome {
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
	if _, ok := findAudit(t, rig.Audit, "tool_call", "denied"); !ok {
		t.Error("被拒的调用必须落 denied 审计")
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
	if _, ok := findAudit(t, rig.Audit, "tool_call", "denied"); !ok {
		t.Error("被拒的调用必须落 denied 审计")
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
	if _, ok := findAudit(t, rig.Audit, "tool_call", "denied"); !ok {
		t.Error("被拒的调用必须落 denied 审计")
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
	if _, ok := findAudit(t, rig.Audit, "tool_call", "success"); !ok {
		t.Error("成功调用必须落 success 审计")
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
	if _, ok := findAudit(t, rig.Audit, "tool_call", "failure"); !ok {
		t.Error("插件自报错误必须记成 failure 审计")
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
	if _, ok := findAudit(t, rig.Audit, "tool_call", "failure"); !ok {
		t.Error("插件不可达必须落 failure 审计")
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
	record, ok := findAudit(t, rig.Audit, "tool_call_list_tools", "success")
	if !ok {
		t.Fatal("ListTools 必须落 success 审计")
	}
	if record.Args["count"] != "1" {
		t.Errorf("审计 count = %q, want 1", record.Args["count"])
	}
}
