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
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Daedalusys/daedalus-sdk/plugin"
	sdkpolicy "github.com/Daedalusys/daedalus-sdk/policy"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrDenied 表示调用被放行判定拒绝。具体是哪道闸由 DenyReason 给出;
// 插件进程起不来、协议失败等不属于放行判定,调用方据此区分
// 「节点拒了」与「节点够不着插件」。
var ErrDenied = errors.New("toolcall: 工具调用被拒")

// ErrPolicyUnavailable 表示运行期重读策略失败。调用方必须 fail-closed:
// 既不能用旧快照继续放行,也不能把它当成「白名单为空」。
var ErrPolicyUnavailable = errors.New("toolcall: 远程工具白名单不可用")

// DenyReason 说明被哪道闸拒的,写进审计供值班定位。
type DenyReason string

const (
	// DenyNotAllowlisted:plugin 或工具根本不在策略白名单里。
	DenyNotAllowlisted DenyReason = "not_allowlisted"
	// DenyNotReadOnly:工具在插件里存在,但没自报只读(写工具走这里)。
	DenyNotReadOnly DenyReason = "not_read_only"
	// DenyNotExposed:工具在白名单里,但插件当前没有暴露它。
	DenyNotExposed DenyReason = "tool_not_exposed"
)

// deniedError 让 errors.Is(err, ErrDenied) 成立,同时携带具体原因。
type deniedError struct{ reason DenyReason }

func (e *deniedError) Error() string {
	return "toolcall: 工具调用被拒: " + string(e.reason)
}

func (e *deniedError) Is(target error) bool { return target == ErrDenied }

func deny(reason DenyReason) error { return &deniedError{reason: reason} }

// DenyReasonOf 提取被拒原因;不是放行判定错误时返回空串。
func DenyReasonOf(err error) DenyReason {
	var de *deniedError
	if errors.As(err, &de) {
		return de.reason
	}
	return ""
}

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

// ListToolsResult 是一次工具集汇总的结果。
type ListToolsResult struct {
	Tools []ToolInfo
	// SkippedPlugins 是 dial / 列工具失败、被跳过的插件数。控制面据此
	// 判断这份清单是否完整。
	SkippedPlugins int
	// LastErr 是最后一次跳过的错误,只用于审计与日志定位。
	LastErr error
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
	// Policy 是静态策略快照,只在 PolicyPath 为空(测试 / 静态注入)时生效。
	Policy Policy
	// PolicyPath 非空时每次调用前重读该文件。Register 上报的 digest 也来自
	// 同一文件的当前内容,重读保证「上报的策略 = 执行的策略」,撤销不必
	// 等重启。启动时的那次读取只做 fail-fast 校验。
	PolicyPath string
	Dialer     Dialer
}

// currentPolicy 返回本次调用应执行的策略。路径配置了就现读现用:
// 读不到或解析失败一律返回 ErrPolicyUnavailable,绝不回落到旧快照。
func (c *Caller) currentPolicy() (Policy, error) {
	if c.PolicyPath == "" {
		return c.Policy, nil
	}
	pol, err := LoadPolicy(c.PolicyPath)
	if err != nil {
		return Policy{}, fmt.Errorf("%w: %w", ErrPolicyUnavailable, err)
	}
	return pol, nil
}

// ListTools 汇总所有白名单插件里可远程调用的工具(白名单 ∩ 只读)。
//
// 单个插件 dial 或列工具失败时跳过它:一个插件坏了不该让整台机器的工具集
// 变空。但「全部白名单插件都失败」必须返回错误 —— 否则控制面看到的是
// tools=[] 的成功结果,与「本来就没有可远程调用的工具」无法区分。
func (c *Caller) ListTools(ctx context.Context) (ListToolsResult, error) {
	pol, err := c.currentPolicy()
	if err != nil {
		return ListToolsResult{}, err
	}
	var result ListToolsResult
	for pluginID := range pol.RemoteTools {
		tools, err := c.listPluginTools(ctx, pluginID, pol)
		if err != nil {
			result.SkippedPlugins++
			result.LastErr = err
			continue
		}
		result.Tools = append(result.Tools, tools...)
	}
	sort.Slice(result.Tools, func(i, j int) bool {
		if result.Tools[i].PluginID != result.Tools[j].PluginID {
			return result.Tools[i].PluginID < result.Tools[j].PluginID
		}
		return result.Tools[i].Name < result.Tools[j].Name
	})
	if len(result.Tools) == 0 && result.SkippedPlugins > 0 {
		return result, fmt.Errorf("toolcall: 白名单插件全部不可用(跳过 %d 个): %w", result.SkippedPlugins, result.LastErr)
	}
	return result, nil
}

// listPluginTools 列出单个插件里既在白名单内、又自报只读的工具。
func (c *Caller) listPluginTools(ctx context.Context, pluginID string, pol Policy) ([]ToolInfo, error) {
	if c.Dialer == nil {
		return nil, fmt.Errorf("toolcall: 未装配 Dialer")
	}
	sess, err := c.Dialer.Dial(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := sess.Close(); cerr != nil {
			log.Printf("toolcall: 关闭插件 %s 会话失败: %v", pluginID, cerr)
		}
	}()

	infos, err := sess.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	allowed := pol.RemoteTools[pluginID]
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

// CallTool 按固定顺序做三步判定:白名单 → 插件暴露且自报只读 → 执行。
//
// 第二步必须重新问插件要工具清单,而不是信任白名单:白名单里混进写工具
// (人工维护失误、插件升级新增写工具)也会在这里被拒,写工具因此不需要
// 额外的黑名单。
func (c *Caller) CallTool(ctx context.Context, pluginID, toolName, argsJSON string) (ToolResult, error) {
	pol, err := c.currentPolicy()
	if err != nil {
		return ToolResult{}, err
	}
	if !pol.Allows(pluginID, toolName) {
		return ToolResult{}, deny(DenyNotAllowlisted)
	}
	if c.Dialer == nil {
		return ToolResult{}, fmt.Errorf("toolcall: 未装配 Dialer")
	}
	sess, err := c.Dialer.Dial(ctx, pluginID)
	if err != nil {
		return ToolResult{}, fmt.Errorf("toolcall: 连接插件 %s: %w", pluginID, err)
	}
	defer func() {
		if cerr := sess.Close(); cerr != nil {
			log.Printf("toolcall: 关闭插件 %s 会话失败: %v", pluginID, cerr)
		}
	}()

	infos, err := sess.ListTools(ctx)
	if err != nil {
		return ToolResult{}, fmt.Errorf("toolcall: 列出插件 %s 的工具: %w", pluginID, err)
	}
	var found, readOnly bool
	for _, info := range infos {
		if info.Name == toolName {
			found, readOnly = true, info.ReadOnly
			break
		}
	}
	if !found {
		return ToolResult{}, deny(DenyNotExposed)
	}
	if !readOnly {
		return ToolResult{}, deny(DenyNotReadOnly)
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
	var cmd *exec.Cmd
	switch m.Runtime {
	case plugin.RuntimeDeno:
		args := append([]string{"run"}, m.Entrypoint...)
		args = append(args, absExec)
		cmd = exec.CommandContext(ctx, denoBinary, args...)
	case plugin.RuntimeNative:
		cmd = exec.CommandContext(ctx, absExec, m.Entrypoint...)
	default:
		return nil, fmt.Errorf("toolcall: 插件 %s 的 runtime %q 不受支持", pluginID, m.Runtime)
	}
	// 不接 stderr 等于把它丢进 /dev/null:MCP 握手失败时插件自己的崩溃
	// 原因就再也看不到了。逐块转进本进程日志,并带 plugin id 前缀。
	cmd.Stderr = &pluginStderr{pluginID: pluginID}
	cmd.Dir = dir
	cmd.Env = pluginEnv(d.root)
	return cmd, nil
}

// pluginEnv 返回插件子进程的干净环境。
//
// 不能 os.Environ() 透传:ALLOW_COMMANDS 是 REPLACE 语义,agent 环境里带着
// 它就会给插件一套不同于其 systemd 单元的命令白名单,等于从 agent 环境旁路
// 插件策略。只给运行必需项:PATH、插件根目录,以及显式配置的策略路径与
// 语言变量;ALLOW_COMMANDS / DAEDALUS_POLICY_MODE 一律不带。
func pluginEnv(root string) []string {
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"DAEDALUS_PLUGIN_DIR=" + root,
	}
	for _, key := range []string{sdkpolicy.EnvPolicyPath, "LANG", "LC_ALL"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	return env
}

// pluginStderr 把插件 stderr 转发到 agent 日志。
type pluginStderr struct{ pluginID string }

func (w *pluginStderr) Write(p []byte) (int, error) {
	if line := strings.TrimRight(string(p), "\n"); line != "" {
		log.Printf("plugin %s stderr: %s", w.pluginID, line)
	}
	return len(p), nil
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
