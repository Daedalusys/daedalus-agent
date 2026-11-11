// ApplyIntent handler:准入 + 本地事务 + 审计回报。
//
// 准入门按"这次变更有没有回滚点"判,而不是"宿主有没有快照机制":后者
// 回答的是用什么手段还原,与有没有还原点无关。高危且无回滚点才拒绝;
// 表外 kind 一律拒绝(fail-closed)。
package rpc

import (
	"context"
	"fmt"
	"log"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

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
func (s *agentService) refuseIntent(req *agentv1.ApplyIntentRequest, reason string) *agentv1.ApplyIntentResponse {
	auditSeq := s.auditSeq.Add(1)
	log.Printf("agent: ApplyIntent refused: intent_id=%q kind=%q target=%q audit_seq=%d — %s",
		req.IntentId, req.Kind, req.Target, auditSeq, reason)
	return &agentv1.ApplyIntentResponse{
		Result:       agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE,
		ErrorMessage: reason,
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
		return s.refuseIntent(req, fmt.Sprintf("unknown intent kind %q; refusing", req.Kind)), nil
	}

	// 2. 高危且无回滚点:拒绝。低危不因缺回滚点被拒 —— 该不该做是风险
	// 问题,不是能力问题。
	if point == rollback.PointNone && req.RiskClass == agentv1.RiskClass_RISK_CLASS_HIGH {
		return s.refuseIntent(req, fmt.Sprintf("high-risk intent on a kind without rollback point: kind=%s", req.Kind)), nil
	}

	// 3. 其余放行。copy 后端是纯 Go 内建的,选不出来只可能是配置错误;
	// 带病往下跑会把一次确定的失败拖成宿主状态未知。
	backend, err := s.selectedBackend()
	if err != nil {
		return s.refuseIntent(req, fmt.Sprintf("rollback backend unavailable: %v", err)), nil
	}

	// pending 只覆盖真正进入事务的 intent:准入拒绝已终结,不该把
	// "未完成工作量"数大。
	s.beginIntent(req.IntentId)
	defer s.endIntent(req.IntentId)

	tx := s.buildTx(backend)
	return tx.Run(ctx, req)
}
