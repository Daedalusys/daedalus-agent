// ApplyIntent handler:准入 + 本地事务 + 审计回报。
//
// 准入门按"这次变更有没有回滚点"判,而不是"宿主有没有快照机制":后者
// 回答的是用什么手段还原,与有没有还原点无关。高危且无回滚点才拒绝;
// 表外 kind 一律拒绝(fail-closed)。宿主快照机制(btrfs/lvm/snapper)不进
// MVP 判定,`snapshot_capability` 字段只回报宿主自带了什么。
//
// 审计:
//
// ApplyIntent 的每个终局都进进程级 JSONL 哈希链(internal/audit,单一写入口
// sdkAudit.LogAudit):进入事务的结局由 internal/tx 写 success / failure /
// rollback_failed / cancelled / presnapshot_failed;在 tx.Run 之前就终结的
// 两类结局(准入拒绝、后端选择失败)由本文件写 refused / backend_unavailable。
// 标准库日志行保留 —— journald 的即时排障视图与链上的取证视图是两种用途。
//
// audit_seq 与链 seq 是两条不同的序列:前者是进程内单调计数器(每次结局 +1,
// 进程重启归零),后者是链文件的行号(跨重启连续)。两者在某些路径上数值
// 碰巧相同,但不是契约:响应里的 AuditSeq 保持前者,report_status 的游标
// 走后者。
package rpc

import (
	"context"
	"fmt"
	"log"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/audit"
	"github.com/Daedalusys/daedalus-agent/internal/rollback"
)

// knownIntentKinds 是 agent 认识的资源 kind 白名单。用显式列表而不是
// "PointFor 不返回 None 就算认识":package / service 明明认识却没有回滚点,
// 两者混在一起会让"不支持这个 kind"与"支持但不承诺回滚"共用一条理由。
var knownIntentKinds = map[string]struct{}{
	"file":    {},
	"package": {},
	"service": {},
}

func isKnownIntentKind(kind string) bool {
	_, ok := knownIntentKinds[kind]
	return ok
}

// refuseIntent 记录一次准入拒绝并返回 in-body 的拒绝响应。
//
// 拒绝码只有 REFUSED_SNAPSHOT_CAPABILITY_NONE 可用(proto 没有更贴切的
// 枚举),真实原因写在 error_message 里 —— 这是已知的命名瑕疵,别让读表
// 的人把结果码当成唯一理由。拒绝是一次**成功**的 RPC:控制面需要这条
// 记录,而且 transport 错误分不清"逻辑拒绝"与"链路断了"。
func (s *agentService) refuseIntent(req *agentv1.ApplyIntentRequest, tool, outcome string, extra map[string]string) *agentv1.ApplyIntentResponse {
	auditSeq := s.auditSeq.Add(1)
	log.Printf("agent: ApplyIntent refused: intent_id=%q kind=%q target=%q audit_seq=%d — %s",
		req.IntentId, req.Kind, req.Target, auditSeq, outcome)
	args := map[string]string{"target": req.Target, "kind": req.Kind}
	for k, v := range extra {
		args[k] = v
	}
	s.recordOutcome(req.IntentId, tool, outcome, args)
	return &agentv1.ApplyIntentResponse{
		Result:       agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE,
		ErrorMessage: outcome,
		AuditSeq:     auditSeq,
	}
}

// ApplyIntent implements agentv1.AgentServiceServer.ApplyIntent.
func (s *agentService) ApplyIntent(ctx context.Context, req *agentv1.ApplyIntentRequest) (*agentv1.ApplyIntentResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "ApplyIntent: nil request")
	}

	point := rollback.PointFor(req.Kind)

	// 1. 表外 kind:既没有回滚点,也没有执行器。放行等于让控制面以为
	// agent 会做点什么,而它什么都不会做。
	if !isKnownIntentKind(req.Kind) {
		return s.refuseIntent(req, "apply_intent.refused", fmt.Sprintf("refused: unknown intent kind %q", req.Kind), nil), nil
	}

	// 2. 高危且无回滚点:拒绝。低危不因缺回滚点被拒 —— 该不该做是风险
	// 问题,不是能力问题。
	if point == rollback.PointNone && req.RiskClass == agentv1.RiskClass_RISK_CLASS_HIGH {
		return s.refuseIntent(req, "apply_intent.refused", fmt.Sprintf("refused: high-risk intent on a kind without rollback point: kind=%s", req.Kind), nil), nil
	}

	// 3. 其余放行。copy 后端是纯 Go 内建的,选不出来只可能是配置错误;
	// 带病往下跑会把一次确定的失败拖成宿主状态未知。
	backend, err := s.selectedBackend()
	if err != nil {
		return s.refuseIntent(req, "apply_intent.backend_unavailable", fmt.Sprintf("backend_unavailable: %v", err), map[string]string{"error": err.Error()}), nil
	}

	// pending 只覆盖真正进入事务的 intent:准入拒绝已终结,不该把
	// "未完成工作量"数大。
	s.beginIntent(req.IntentId)
	defer s.endIntent(req.IntentId)

	tx := s.buildTx(backend)
	return tx.Run(ctx, req)
}

// recordOutcome 把一个"在进入事务之前就已终结"的 ApplyIntent 结局写进进程级
// 哈希链(与 internal/tx 共用同一条链、同一个写入口)。
//
// 为什么在 handler 而不是 tx 里:准入拒绝与后端选择失败都在 tx.Run 之前返回,
// 根本到不了状态机;不在这里补一笔,链上就分不清"从未收到该 intent"与
// "收到但按 fail-closed 拒绝" —— 而这正是控制面要展示给审查者的区别。
//
// audit 为 nil(测试或未配置链)时直接跳过;写链失败只记日志,绝不改变
// RPC 的对外结局 —— 与 internal/tx 的处理保持一致。
func (s *agentService) recordOutcome(intentID, tool, outcome string, args map[string]string) {
	if s.audit == nil {
		return
	}
	if _, err := s.audit.Log(audit.Event{
		Identity: intentID,
		Tool:     tool,
		Args:     args,
		Outcome:  outcome,
	}); err != nil {
		log.Printf("agent: audit log (%s) failed: %v", tool, err)
	}
}
