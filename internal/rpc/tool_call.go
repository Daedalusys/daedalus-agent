// tool_call.go 实现 ListTools / CallTool 两个只读工具调用 RPC。
//
// handler 只做协议映射与留痕:白名单与 ReadOnlyHint 两道闸在
// internal/toolcall 完成,这里不重复实现策略,也不替插件做参数校验 ——
// 入参原样透传,插件自己的 shellpolicy / pathguard 仍然生效。
package rpc

import (
	"context"
	"errors"
	"log"
	"strconv"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/audit"
	"github.com/Daedalusys/daedalus-agent/internal/toolcall"
)

// 审计里的 tool 名。list 与 call 分开,控制面/运维可以直接按它们筛选。
const (
	auditToolCallListTools = "tool_call_list_tools"
	auditToolCall          = "tool_call"
)

// auditIdentityToolCall 标记这些事件由控制面的远程调用发起,不是本机操作。
const auditIdentityToolCall = "control-plane"

// ListTools 返回节点上既在白名单内、又自报只读的工具。
//
// 未配置工具调用面(ToolCaller == nil)时返回空工具集而不是错误:节点没有
// 远程面是正常形态,列出来是空集合;真正硬调工具时才会被 CallTool 拒。
func (s *agentService) ListTools(ctx context.Context, _ *agentv1.ListToolsRequest) (*agentv1.ListToolsResponse, error) {
	if s.toolCaller == nil {
		seq := s.recordToolCallEvent(auditToolCallListTools, "denied", map[string]string{
			"reason": "no_remote_surface",
		})
		return &agentv1.ListToolsResponse{AuditSeq: seq}, nil
	}

	tools, err := s.toolCaller.ListTools(ctx)
	if err != nil {
		s.recordToolCallEvent(auditToolCallListTools, "failure", map[string]string{
			"reason": "list_failed",
		})
		log.Printf("agent: ListTools failed: %v", err)
		return nil, status.Error(codes.Unavailable, "列出节点工具失败")
	}

	out := make([]*agentv1.ToolDescriptor, 0, len(tools))
	for _, tool := range tools {
		out = append(out, &agentv1.ToolDescriptor{
			PluginId:        tool.PluginID,
			ToolName:        tool.Name,
			Description:     tool.Description,
			InputSchemaJson: tool.InputSchemaJSON,
			ReadOnly:        tool.ReadOnly,
		})
	}
	seq := s.recordToolCallEvent(auditToolCallListTools, "success", map[string]string{
		"count": strconv.Itoa(len(out)),
	})
	return &agentv1.ListToolsResponse{Tools: out, AuditSeq: seq}, nil
}

// CallTool 远程调用一个只读工具。每次调用(含被拒)都落一条审计。
//
// 错误码与控制面的分类约定一一对应:
//   - FailedPrecondition:节点没配工具调用面;
//   - PermissionDenied:白名单或只读判定不通过;
//   - Unavailable:插件进程起不来 / 协议失败(节点够不着插件)。
//
// 插件自报的业务错误不是 RPC 错误:它在 IsError 里原样返回,证据原文
// 留在 ContentJson。
func (s *agentService) CallTool(ctx context.Context, req *agentv1.CallToolRequest) (*agentv1.CallToolResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "请求不能为空")
	}
	args := map[string]string{
		"plugin_id": req.GetPluginId(),
		"tool_name": req.GetToolName(),
	}

	if s.toolCaller == nil {
		args["reason"] = "no_remote_surface"
		s.recordToolCallEvent(auditToolCall, "denied", args)
		return nil, status.Error(codes.FailedPrecondition, "节点未配置只读工具调用面")
	}

	res, err := s.toolCaller.CallTool(ctx, req.GetPluginId(), req.GetToolName(), req.GetArgumentsJson())
	if errors.Is(err, toolcall.ErrDenied) {
		args["reason"] = "not_allowlisted_or_not_read_only"
		s.recordToolCallEvent(auditToolCall, "denied", args)
		return nil, status.Error(codes.PermissionDenied, "工具不在远程白名单内,或未自报只读")
	}
	if err != nil {
		args["reason"] = "plugin_unavailable"
		s.recordToolCallEvent(auditToolCall, "failure", args)
		log.Printf("agent: CallTool %s/%s failed: %v", req.GetPluginId(), req.GetToolName(), err)
		return nil, status.Error(codes.Unavailable, "调用节点工具失败")
	}

	if res.IsError {
		seq := s.recordToolCallEvent(auditToolCall, "failure", args)
		return &agentv1.CallToolResponse{
			IsError:      true,
			ContentJson:  res.ContentJSON,
			ErrorMessage: res.ErrorMessage,
			AuditSeq:     seq,
		}, nil
	}
	seq := s.recordToolCallEvent(auditToolCall, "success", args)
	return &agentv1.CallToolResponse{
		IsError:     false,
		ContentJson: res.ContentJSON,
		AuditSeq:    seq,
	}, nil
}

// recordToolCallEvent 把一次远程工具调用写进进程级哈希链,返回新 seq。
//
// audit 为 nil(测试或未配置链)时返回 0;写链失败只记日志,绝不改变 RPC
// 的对外结局 —— 与既有 handler 的处理保持一致。
func (s *agentService) recordToolCallEvent(tool, outcome string, args map[string]string) uint64 {
	if s.audit == nil {
		return 0
	}
	seq, err := s.audit.Log(audit.Event{
		Identity: auditIdentityToolCall,
		Tool:     tool,
		Args:     args,
		Outcome:  outcome,
	})
	if err != nil {
		log.Printf("agent: audit log (%s) failed: %v", tool, err)
		return 0
	}
	return seq
}
