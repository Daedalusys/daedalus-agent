package toolcall

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newFakeCaller 用进程内 MCP server 拼一个不 spawn 任何进程的 Caller。
func newFakeCaller(t *testing.T, pol Policy) *Caller {
	t.Helper()
	return &Caller{Policy: pol, Dialer: &fakeDialer{t: t}}
}

// policyWith 构造单插件白名单。
func policyWith(pluginID string, tools ...string) Policy {
	set := make(map[string]struct{}, len(tools))
	for _, name := range tools {
		set[name] = struct{}{}
	}
	return Policy{RemoteTools: map[string]map[string]struct{}{pluginID: set}}
}

// fakeDialer 每次 Dial 起一个新的进程内 MCP server。
// 真实 transport 是一次性的,所以每次都要新建一对内存 transport。
type fakeDialer struct {
	t *testing.T
}

func (d *fakeDialer) Dial(ctx context.Context, _ string) (Session, error) {
	cs, err := dialInMemory(ctx)
	if err != nil {
		return nil, err
	}
	if d.t != nil {
		d.t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, nil
}

// dialInMemory 起一个假插件并连上它,返回客户端会话。
func dialInMemory(ctx context.Context) (Session, error) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := newFakeServer()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		return nil, err
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "fake-client", Version: "0.0.0"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		return nil, err
	}
	return &mcpSession{cs: cs}, nil
}

// newFakeServer 注册三个工具:shell_exec / read_file 自报只读,
// write_file 自报可写;shell_exec 在入参含 boom=true 时返回业务错误。
func newFakeServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake-plugin", Version: "0.0.0"}, nil)
	schema := map[string]any{"type": "object"}
	srv.AddTool(&mcp.Tool{
		Name:        "shell_exec",
		Description: "执行只读命令",
		InputSchema: schema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Boom bool `json:"boom"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		if in.Boom {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "插件拒绝了这次调用"}},
			}, nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "total 0"}},
		}, nil
	})
	srv.AddTool(&mcp.Tool{
		Name:        "read_file",
		Description: "读取文件",
		InputSchema: schema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "file body"}},
		}, nil
	})
	srv.AddTool(&mcp.Tool{
		Name:        "write_file",
		Description: "写文件",
		InputSchema: schema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false},
	}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "written"}},
		}, nil
	})
	// 故意不写 Annotations:ReadOnlyHint 缺省 false,必须被拒。
	srv.AddTool(&mcp.Tool{
		Name:        "no_annotations",
		Description: "不自报只读的工具",
		InputSchema: schema,
	}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "unexpected"}},
		}, nil
	})
	return srv
}

// selectiveDialer 让指定 plugin 的 Dial 失败,其它 plugin 连进程内假插件。
type selectiveDialer struct {
	broken string
}

func (d *selectiveDialer) Dial(ctx context.Context, pluginID string) (Session, error) {
	if pluginID == d.broken {
		return nil, errors.New("插件进程起不来")
	}
	return dialInMemory(ctx)
}

// alwaysFailDialer 模拟所有白名单插件都起不来。
type alwaysFailDialer struct{}

func (alwaysFailDialer) Dial(context.Context, string) (Session, error) {
	return nil, errors.New("插件进程起不来")
}

func hasName(tools []ToolInfo, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}
