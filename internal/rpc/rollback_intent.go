// RollbackIntent handler:PLAN 出计划,APPLY 校验 token 后执行。
//
// 边界(与设计稿 D2/D3 对齐):
//
//   - PLAN **不执行任何命令**,只读查询包管理器历史 / 版本;
//   - APPLY 必须带 plan_hash + approve_token,且请求里的步骤必须能重算出
//     同一个哈希 —— 计划不能在 APPLY 时被静默替换;
//   - 计划哈希只覆盖上了 wire 的字段,所以 admin 想改命令就得重新 PLAN;
//   - 每个结局都进链:rollback.planned / applied / refused / failed。
//
// token 的语义边界(别读大):它只证明「这次 APPLY 绑定的就是那份
// plan_hash」,不证明有人看过计划 —— 人在环路里由控制面保证。
package rpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/rollback"
)

// RollbackIntent implements agentv1.AgentServiceServer.RollbackIntent.
func (s *agentService) RollbackIntent(ctx context.Context, req *agentv1.RollbackIntentRequest) (*agentv1.RollbackIntentResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "RollbackIntent: nil request")
	}
	if req.GetIntentId() == "" {
		return s.refuseRollback(req, "refused: intent_id is required"), nil
	}
	if req.GetRollbackHandle() == "" {
		return s.refuseRollback(req, "refused: rollback_handle is required"), nil
	}
	switch req.GetMode() {
	case agentv1.RollbackMode_ROLLBACK_MODE_PLAN:
		return s.rollbackPlan(ctx, req), nil
	case agentv1.RollbackMode_ROLLBACK_MODE_APPLY:
		return s.rollbackApply(ctx, req), nil
	}
	return s.refuseRollback(req, fmt.Sprintf("refused: unknown rollback mode %v", req.GetMode())), nil
}

// rollbackPlan 生成不可变计划 + plan_hash + 一次性 token。不执行任何命令。
func (s *agentService) rollbackPlan(ctx context.Context, req *agentv1.RollbackIntentRequest) *agentv1.RollbackIntentResponse {
	if s.rollbackTokens == nil {
		return s.refuseRollback(req, "refused: rollback channel is not armed")
	}
	plan, err := rollback.BuildPlan(ctx, req.GetRollbackHandle(), rollback.NewCommandAllower())
	if err != nil {
		return s.refuseRollback(req, fmt.Sprintf("refused: cannot build a rollback plan: %v", err))
	}
	// admin 改过的步骤:用与执行阶段同一套判定校验后才进计划。
	if len(req.GetSteps()) > 0 {
		steps, verr := s.validatePlanSteps(plan, req.GetSteps())
		if verr != nil {
			return s.refuseRollback(req, fmt.Sprintf("refused: %v", verr))
		}
		plan.Steps = steps
	}

	planHash := plan.Hash()
	token, err := s.rollbackTokens.Issue(planHash, req.GetIntentId(), time.Now())
	if err != nil {
		return s.refuseRollback(req, fmt.Sprintf("refused: cannot issue an approve token: %v", err))
	}

	seq := s.auditSeq.Add(1)
	s.recordOutcome(req.GetIntentId(), "rollback.planned", "planned", map[string]string{
		"handle":    plan.Handle,
		"plan_hash": planHash,
		"steps":     strconv.Itoa(len(plan.Steps)),
		"auto_ok":   strconv.FormatBool(plan.AutoOK),
	})
	return &agentv1.RollbackIntentResponse{
		Result:       agentv1.RollbackResult_ROLLBACK_RESULT_PLANNED,
		Steps:        protoSteps(plan.Steps),
		PlanHash:     planHash,
		ApproveToken: token,
		AuditSeq:     seq,
	}
}

// rollbackApply 校验 token 与计划哈希后执行步骤。
func (s *agentService) rollbackApply(ctx context.Context, req *agentv1.RollbackIntentRequest) *agentv1.RollbackIntentResponse {
	if s.rollbackTokens == nil {
		return s.refuseRollback(req, "refused: rollback channel is not armed")
	}
	if req.GetApproveToken() == "" {
		return s.refuseRollback(req, "refused: approve_token is required for APPLY")
	}
	if req.GetPlanHash() == "" {
		return s.refuseRollback(req, "refused: plan_hash is required for APPLY")
	}
	if len(req.GetSteps()) == 0 {
		return s.refuseRollback(req, "refused: steps are required for APPLY")
	}

	steps, err := stepsFromProto(req.GetSteps(), req.GetRollbackHandle())
	if err != nil {
		return s.refuseRollback(req, fmt.Sprintf("refused: %v", err))
	}
	// 步骤必须与计划一致:哈希对不上就是"计划被换过"。
	plan := rollback.Plan{Handle: req.GetRollbackHandle(), Steps: steps}
	if plan.Hash() != req.GetPlanHash() {
		return s.refuseRollback(req, "refused: plan_hash does not match the steps")
	}
	// token 绑定 plan_hash + intent_id,单次使用。放在哈希校验之后:先确认
	// 这是同一份计划,再消耗 token。
	if err := s.rollbackTokens.Verify(req.GetApproveToken(), req.GetPlanHash(), req.GetIntentId(), time.Now()); err != nil {
		return s.refuseRollback(req, fmt.Sprintf("refused: %v", err))
	}
	if err := s.attachOwnedPaths(ctx, req.GetRollbackHandle(), steps); err != nil {
		return s.refuseRollback(req, fmt.Sprintf("refused: %v", err))
	}

	if err := s.rollbackRunner.Run(ctx, steps); err != nil {
		// 执行失败与"计划没通过校验"是两种结局:前者宿主可能已被改动,
		// 后者一步都没跑。控制面看到的结果码不同。
		if errors.Is(err, rollback.ErrStepFailed) {
			return s.failedRollback(req, err)
		}
		return s.refuseRollback(req, fmt.Sprintf("refused: %v", err))
	}

	seq := s.auditSeq.Add(1)
	s.recordOutcome(req.GetIntentId(), "rollback.applied", "applied", map[string]string{
		"handle":    plan.Handle,
		"plan_hash": req.GetPlanHash(),
		"token_id":  tokenID(req.GetApproveToken()),
	})
	return &agentv1.RollbackIntentResponse{
		Result:   agentv1.RollbackResult_ROLLBACK_RESULT_APPLIED,
		Steps:    protoSteps(steps),
		PlanHash: req.GetPlanHash(),
		AuditSeq: seq,
	}
}

// validatePlanSteps 把 admin 改过的步骤并进计划,并用与执行阶段同一套判定
// 校验。
//
// 包拥有的路径清单来自 **agent 自己生成的基础计划**,不来自请求:请求方
// 没法通过"声明这些路径归我"把删除范围扩到包以外。
func (s *agentService) validatePlanSteps(base rollback.Plan, protoSteps []*agentv1.RollbackStep) ([]rollback.Step, error) {
	steps, err := stepsFromProto(protoSteps, base.Handle)
	if err != nil {
		return nil, err
	}
	owned := ownedPaths(base.Steps)
	for i := range steps {
		steps[i].OwnedPaths = owned
		if err := rollback.ValidateStep(steps[i]); err != nil {
			return nil, fmt.Errorf("step %d: %w", i, err)
		}
	}
	return steps, nil
}

// attachOwnedPaths 给含 rm 步骤的计划补包文件清单。
//
// 只对含 rm 的计划查包管理器:其余计划在 APPLY 时不该被无关查询拖挂。
// 查不到清单就拒绝 —— rm 的路径没有归属证明,不能执行。
func (s *agentService) attachOwnedPaths(ctx context.Context, handle string, steps []rollback.Step) error {
	if !hasFileDelete(steps) {
		return nil
	}
	base, err := rollback.BuildPlan(ctx, handle, rollback.NewCommandAllower())
	if err != nil {
		return fmt.Errorf("cannot re-derive the package-owned paths: %v", err)
	}
	owned := ownedPaths(base.Steps)
	for i := range steps {
		steps[i].OwnedPaths = owned
	}
	return nil
}

// hasFileDelete 报告步骤里有没有文件删除。
func hasFileDelete(steps []rollback.Step) bool {
	for _, s := range steps {
		if s.Kind == rollback.StepFileDelete {
			return true
		}
	}
	return false
}

// ownedPaths 汇总基础计划里声明的包拥有路径(去重,保持顺序)。
func ownedPaths(steps []rollback.Step) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range steps {
		for _, p := range s.OwnedPaths {
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}

// refuseRollback 记录一次回滚拒绝并返回 in-body 的拒绝响应。
//
// 拒绝是一次**成功**的 RPC:控制面需要这条记录,transport 错误分不清
// "逻辑拒绝"与"链路断了"。
func (s *agentService) refuseRollback(req *agentv1.RollbackIntentRequest, outcome string) *agentv1.RollbackIntentResponse {
	seq := s.auditSeq.Add(1)
	log.Printf("agent: RollbackIntent refused: intent_id=%q handle=%q mode=%v audit_seq=%d — %s",
		req.GetIntentId(), req.GetRollbackHandle(), req.GetMode(), seq, outcome)
	s.recordOutcome(req.GetIntentId(), "rollback.refused", outcome, map[string]string{
		"handle": req.GetRollbackHandle(),
		"reason": outcome,
	})
	return &agentv1.RollbackIntentResponse{
		Result:       agentv1.RollbackResult_ROLLBACK_RESULT_REFUSED,
		ErrorMessage: outcome,
		AuditSeq:     seq,
	}
}

// failedRollback 记录一次执行中途失败。已执行步骤不回退(不自动重跑)。
func (s *agentService) failedRollback(req *agentv1.RollbackIntentRequest, runErr error) *agentv1.RollbackIntentResponse {
	seq := s.auditSeq.Add(1)
	log.Printf("agent: RollbackIntent failed: intent_id=%q handle=%q audit_seq=%d err=%v",
		req.GetIntentId(), req.GetRollbackHandle(), seq, runErr)
	s.recordOutcome(req.GetIntentId(), "rollback.failed", fmt.Sprintf("failed: %v", runErr), map[string]string{
		"handle":    req.GetRollbackHandle(),
		"plan_hash": req.GetPlanHash(),
		"error":     runErr.Error(),
	})
	return &agentv1.RollbackIntentResponse{
		Result:       agentv1.RollbackResult_ROLLBACK_RESULT_FAILED,
		ErrorMessage: runErr.Error(),
		AuditSeq:     seq,
	}
}

// tokenID 给链上留一个 token 的指纹,而不是 token 本体。
func tokenID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// protoSteps 把内部步骤搬上 wire。
func protoSteps(steps []rollback.Step) []*agentv1.RollbackStep {
	out := make([]*agentv1.RollbackStep, 0, len(steps))
	for _, s := range steps {
		out = append(out, &agentv1.RollbackStep{
			Kind:                kindToProto(s.Kind),
			Command:             s.Command,
			Rationale:           s.Rationale,
			HumanReviewRequired: s.HumanReview,
		})
	}
	return out
}

// stepsFromProto 把 wire 上的步骤搬回内部形态。句柄由请求给(步骤本身不带)。
func stepsFromProto(steps []*agentv1.RollbackStep, handle string) ([]rollback.Step, error) {
	out := make([]rollback.Step, 0, len(steps))
	for i, s := range steps {
		if s == nil {
			return nil, fmt.Errorf("step %d is nil", i)
		}
		kind, err := kindFromProto(s.GetKind())
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", i, err)
		}
		out = append(out, rollback.Step{
			Kind:        kind,
			Command:     s.GetCommand(),
			Rationale:   s.GetRationale(),
			HumanReview: s.GetHumanReviewRequired(),
			Handle:      handle,
		})
	}
	return out, nil
}

// kindToProto 是内部步骤类型 → wire 枚举的唯一映射。
func kindToProto(k rollback.StepKind) agentv1.RollbackStepKind {
	switch k {
	case rollback.StepNativeUndo:
		return agentv1.RollbackStepKind_ROLLBACK_STEP_KIND_NATIVE_UNDO
	case rollback.StepPackageRemove:
		return agentv1.RollbackStepKind_ROLLBACK_STEP_KIND_PACKAGE_REMOVE
	case rollback.StepFileDelete:
		return agentv1.RollbackStepKind_ROLLBACK_STEP_KIND_FILE_DELETE
	case rollback.StepManual:
		return agentv1.RollbackStepKind_ROLLBACK_STEP_KIND_MANUAL
	}
	return agentv1.RollbackStepKind_ROLLBACK_STEP_KIND_UNSPECIFIED
}

// kindFromProto 是 wire 枚举 → 内部步骤类型。认不出的枚举一律拒绝
// (fail-closed,不猜)。
func kindFromProto(k agentv1.RollbackStepKind) (rollback.StepKind, error) {
	switch k {
	case agentv1.RollbackStepKind_ROLLBACK_STEP_KIND_NATIVE_UNDO:
		return rollback.StepNativeUndo, nil
	case agentv1.RollbackStepKind_ROLLBACK_STEP_KIND_PACKAGE_REMOVE:
		return rollback.StepPackageRemove, nil
	case agentv1.RollbackStepKind_ROLLBACK_STEP_KIND_FILE_DELETE:
		return rollback.StepFileDelete, nil
	case agentv1.RollbackStepKind_ROLLBACK_STEP_KIND_MANUAL:
		return rollback.StepManual, nil
	}
	return rollback.StepUnspecified, fmt.Errorf("unknown rollback step kind %v", k)
}
