// toolcall.go 实现节点侧的只读工具调用面。
//
// 放行条件是两道闸的「与」:策略白名单管「这台机器允许远程调什么」,
// 插件自报的 ReadOnlyHint 管「这个工具本身有没有副作用」。参数原样透传给
// 插件,插件自己的 shellpolicy / pathguard 仍然生效 —— 远程调用不绕过任何
// 既有校验,也不在这里另造一套策略。
package toolcall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Daedalusys/daedalus-sdk/plugin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrDenied 表示调用不在白名单内,或目标工具没有自报只读。
// 其它错误(插件进程起不来、协议失败)不是放行判定,调用方据此区分
// 「节点拒了」与「节点够不着插件」。
var ErrDenied = errors.New("toolcall: 工具调用被拒")

// ToolInfo 是一条工具元信息。PluginID 由 Caller.ListTools 在汇总时填上;
// Session.ListTools 只填插件侧字段。
type ToolInfo struct {
	PluginID        string
	Name            string
	Description     string
	InputSchemaJSON string
	ReadOnly        bool
}

// ToolResult 是一次工具调用的结果。
// IsError 是插件自报的业务错误,不是传输失败 —— 后者走 error 返回值。
type ToolResult struct {
	IsError      bool
	ContentJSON  string
	ErrorMessage string
}

// Session 是一条已建立的插件 MCP 会话。
type Session interface {
	ListTools(ctx context.Context) ([]ToolInfo, error)
	CallTool(ctx context.Context, name, argsJSON string) (ToolResult, error)
	Close() error
}

// Dialer 建立到某个插件 MCP 服务器的会话。
type Dialer interface {
	Dial(ctx context.Context, pluginID string) (Session, error)
}

// Caller 是叠加白名单与只读判定后的工具调用入口。
type Caller struct {
	Policy Policy
	Dialer Dialer
}

// ListTools 汇总所有白名单插件里可远程调用的工具(白名单 ∩ 只读)。
//
// 单个插件 dial 或列工具失败时跳过它:一个插件坏了不该让整台机器的工具集
// 变空,更不能因此让巡检整体失败。排序只为让输出稳定(便于控制面 diff)。
func (c *Caller) ListTools(ctx context.Context) ([]ToolInfo, error) {
	var all []ToolInfo
	for pluginID := range c.Policy.RemoteTools {
		tools, err := c.listPluginTools(ctx, pluginID)
		if err != nil {
			continue
		}
		all = append(all, tools...)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].PluginID != all[j].PluginID {
			return all[i].PluginID < all[j].PluginID
		}
		return all[i].Name < all[j].Name
	})
	return all, nil
}

// listPluginTools 列出单个插件里既在白名单内、又自报只读的工具。
func (c *Caller) listPluginTools(ctx context.Context, pluginID string) ([]ToolInfo, error) {
	if c.Dialer == nil {
		return nil, fmt.Errorf("toolcall: 未装配 Dialer")
	}
	sess, err := c.Dialer.Dial(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()

	infos, err := sess.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	allowed := c.Policy.RemoteTools[pluginID]
	out := make([]ToolInfo, 0, len(infos))
	for _, info := range infos {
		if !info.ReadOnly {
			continue
		}
		if _, ok := allowed[info.Name]; !ok {
			continue
		}
		info.PluginID = pluginID
		out = append(out, info)
	}
	return out, nil
}

// CallTool 按固定顺序做三步判定:白名单 → 插件自报只读 → 执行。
//
// 第二步必须重新问插件要工具清单,而不是信任白名单:白名单里混进写工具
// (人工维护失误、插件升级新增写工具)也会在这里被拒,写工具因此不需要
// 额外的黑名单。
func (c *Caller) CallTool(ctx context.Context, pluginID, toolName, argsJSON string) (ToolResult, error) {
	if !c.Policy.Allows(pluginID, toolName) {
		return ToolResult{}, ErrDenied
	}
	if c.Dialer == nil {
		return ToolResult{}, fmt.Errorf("toolcall: 未装配 Dialer")
	}
	sess, err := c.Dialer.Dial(ctx, pluginID)
	if err != nil {
		return ToolResult{}, fmt.Errorf("toolcall: 连接插件 %s: %w", pluginID, err)
	}
	defer func() { _ = sess.Close() }()

	infos, err := sess.ListTools(ctx)
	if err != nil {
		return ToolResult{}, fmt.Errorf("toolcall: 列出插件 %s 的工具: %w", pluginID, err)
	}
	readOnly := false
	for _, info := range infos {
		if info.Name == toolName {
			readOnly = info.ReadOnly
			break
		}
	}
	if !readOnly {
		return ToolResult{}, ErrDenied
	}

	res, err := sess.CallTool(ctx, toolName, argsJSON)
	if err != nil {
		return ToolResult{}, fmt.Errorf("toolcall: 调用 %s/%s: %w", pluginID, toolName, err)
	}
	return res, nil
}

// 插件根目录与 daedalus-host 共用同一个环境变量:镜像级只配一处。
const (
	defaultPluginDir = "/opt/daedalus/plugins"
	pluginDirEnv     = "DAEDALUS_PLUGIN_DIR"
	// denoBinary 与宿主渲染 systemd 单元时用的路径一致;deno 插件由
	// entrypoint 携带权限旗标,脚本路径放在参数尾部。
	denoBinary = "/usr/local/bin/deno"
)

// spawnDialer 是生产 Dialer:按插件 manifest 现起一个 MCP stdio 服务器。
//
// 生命周期取「每次调用 spawn + 用完退出」:节点上的只读调用是低频巡检,
// 常驻连接要多养一套进程监管与重启逻辑,崩溃面更大;代价只是每次一次
// MCP 握手。
type spawnDialer struct {
	root    string
	version string
}

// NewSpawnDialer 构造生产 Dialer。插件根目录可用 DAEDALUS_PLUGIN_DIR
// 覆盖(与 daedalus-host 同一个变量),version 进 MCP 客户端自报的实现信息。
func NewSpawnDialer(version string) Dialer {
	root := os.Getenv(pluginDirEnv)
	if root == "" {
		root = defaultPluginDir
	}
	return &spawnDialer{root: root, version: version}
}

func (d *spawnDialer) Dial(ctx context.Context, pluginID string) (Session, error) {
	cmd, err := d.command(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "daedalus-agent",
		Version: d.version,
	}, nil)
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("toolcall: MCP 握手失败(%s): %w", pluginID, err)
	}
	return &mcpSession{cs: cs}, nil
}

// command 从插件目录的 manifest 构造启动命令。
//
// executable 由 manifest 给出,不写死在代码里;plugin id 与 executable 都
// 要过 SDK 的路径校验,避免 manifest 里的 ../ 把命令指到插件目录外。
func (d *spawnDialer) command(ctx context.Context, pluginID string) (*exec.Cmd, error) {
	if !plugin.ValidID(pluginID) {
		return nil, fmt.Errorf("toolcall: 非法 plugin id %q", pluginID)
	}
	dir := filepath.Join(d.root, pluginID)
	manifestPath := filepath.Join(dir, plugin.ManifestFileName)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("toolcall: 读取插件清单 %s: %w", manifestPath, err)
	}
	m, err := plugin.ParseManifest(data)
	if err != nil {
		return nil, fmt.Errorf("toolcall: 插件 %s: %w", pluginID, err)
	}
	if err := plugin.ValidateRelativePath("executable", m.Executable); err != nil {
		return nil, fmt.Errorf("toolcall: 插件 %s: %w", pluginID, err)
	}
	absExec := filepath.Join(dir, m.Executable)
	switch m.Runtime {
	case plugin.RuntimeDeno:
		args := append([]string{"run"}, m.Entrypoint...)
		args = append(args, absExec)
		return exec.CommandContext(ctx, denoBinary, args...), nil
	case plugin.RuntimeNative:
		return exec.CommandContext(ctx, absExec, m.Entrypoint...), nil
	default:
		return nil, fmt.Errorf("toolcall: 插件 %s 的 runtime %q 不受支持", pluginID, m.Runtime)
	}
}

// mcpSession 把 SDK 的 *mcp.ClientSession 适配成本包的 Session。
type mcpSession struct {
	cs *mcp.ClientSession
}

func (s *mcpSession) ListTools(ctx context.Context) ([]ToolInfo, error) {
	res, err := s.cs.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return nil, err
	}
	out := make([]ToolInfo, 0, len(res.Tools))
	for _, tool := range res.Tools {
		if tool == nil {
			continue
		}
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("toolcall: 序列化 %s 的 input schema: %w", tool.Name, err)
		}
		info := ToolInfo{
			Name:            tool.Name,
			Description:     tool.Description,
			InputSchemaJSON: string(schema),
		}
		if tool.Annotations != nil {
			info.ReadOnly = tool.Annotations.ReadOnlyHint
		}
		out = append(out, info)
	}
	return out, nil
}

func (s *mcpSession) CallTool(ctx context.Context, name, argsJSON string) (ToolResult, error) {
	// 空入参按空对象发:插件普遍按 object 解析参数,发空字符串会让
	// JSON-RPC 序列化在客户端就失败,错误看起来像传输问题。
	args := json.RawMessage(argsJSON)
	if strings.TrimSpace(argsJSON) == "" {
		args = json.RawMessage("{}")
	}
	res, err := s.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return ToolResult{}, err
	}
	content, err := json.Marshal(res.Content)
	if err != nil {
		return ToolResult{}, fmt.Errorf("toolcall: 序列化 %s 的返回内容: %w", name, err)
	}
	out := ToolResult{IsError: res.IsError, ContentJSON: string(content)}
	if res.Content == nil {
		out.ContentJSON = "[]"
	}
	if res.IsError {
		out.ErrorMessage = textOf(res.Content)
		if out.ErrorMessage == "" {
			out.ErrorMessage = out.ContentJSON
		}
	}
	return out, nil
}

func (s *mcpSession) Close() error { return s.cs.Close() }

// textOf 拼接返回内容里的文本片段,用作业务错误的可读原因。
func textOf(content []mcp.Content) string {
	var b strings.Builder
	for _, item := range content {
		text, ok := item.(*mcp.TextContent)
		if !ok {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(text.Text)
	}
	return b.String()
}
