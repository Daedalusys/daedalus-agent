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
	"github.com/Daedalusys/daedalus-agent/internal/exec"
	"github.com/Daedalusys/daedalus-agent/internal/filespec"
	"github.com/Daedalusys/daedalus-agent/internal/rollback"
	"github.com/Daedalusys/daedalus-agent/internal/tx"
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

	point := rollback.PointForIntent(req)

	// 1. 表外 kind:既没有回滚点,也没有执行器。放行等于让控制面以为
	// agent 会做点什么,而它什么都不会做。
	if !isKnownIntentKind(req.Kind) {
		return s.refuseIntent(req, "apply_intent.refused", fmt.Sprintf("refused: unknown intent kind %q", req.Kind), nil), nil
	}

	// 2. 高危且这个 kind 连"可能产出句柄"都谈不上:拒绝。低危不因缺回滚点
	// 被拒 —— 该不该做是风险问题,不是能力问题。
	//
	// package / service 的句柄要等 apply 之后才抓得到,它们的判定放在事务
	// 之后(见第 5 步):有句柄就放行,空句柄对高危拒绝。
	if point == rollback.PointNone && req.RiskClass == agentv1.RiskClass_RISK_CLASS_HIGH && !exec.HandleCapable(req.Kind) {
		return s.refuseIntent(req, "apply_intent.refused", fmt.Sprintf("refused: high-risk intent on a kind without rollback point: kind=%s", req.Kind), nil), nil
	}

	// 3. 目录级 file intent:spec 在这里解析一次,把快照路径注入事务。
	// 校验必须发生在进事务之前 —— 越界或类型不符的 spec 不该先建快照、
	// 更不该先写文件;失败也就没有"半截快照"要收拾。
	//
	// 解析/校验规则只在 internal/filespec 一处,这里不重复判断。
	var txOpts []tx.Option
	if point == rollback.PointTree {
		spec, serr := filespec.Parse(req.SpecJson)
		if serr != nil {
			return s.refuseIntent(req, "apply_intent.refused", fmt.Sprintf("refused: invalid file spec_json: %v", serr), nil), nil
		}
		if verr := spec.Validate(req.Target); verr != nil {
			return s.refuseIntent(req, "apply_intent.refused", fmt.Sprintf("refused: directory file spec rejected: %v", verr), nil), nil
		}
		paths := spec.SnapshotPaths(req.Target)
		txOpts = append(txOpts, tx.WithSnapshotPaths(func(*agentv1.ApplyIntentRequest) ([]string, error) {
			return paths, nil
		}))
	}

	// 4. 其余放行。只有**存在回滚点**的 intent 才需要后端:file 恒走内建
	// copy/tree,卷级(v0 无 producer)才看宿主能力。低危 + 无回滚点的 intent
	// 根本不建快照,这里若也去选后端,就会把"没有回滚点"错判成"配置错误",
	// 把整条低危腿拒掉(真机回归:vma/AlmaLinux 9 上低危 package 就是这么
	// 被拒的)。
	var backend rollback.Backend
	if point != rollback.PointNone {
		var berr error
		backend, berr = s.selectedBackend(point)
		if berr != nil {
			return s.refuseIntent(req, "apply_intent.backend_unavailable", fmt.Sprintf("backend_unavailable: %v", berr), map[string]string{"error": berr.Error()}), nil
		}
	}

	// pending 只覆盖真正进入事务的 intent:准入拒绝已终结,不该把
	// "未完成工作量"数大。
	s.beginIntent(req.IntentId)
	defer s.endIntent(req.IntentId)

	tx := s.buildTx(backend, txOpts...)
	resp, err := tx.Run(ctx, req)
	if err != nil {
		return nil, err
	}

	// 5. 高危的 package / service:拿不到回滚锚点就不算数。
	//
	// 为什么放在事务之后:句柄只有执行器在 apply 之后才知道,事务之前判不了。
	// 为什么"改判"而不是"提前拒":提前拒会把高危装包全部挡在门外 —— 那正是
	// 这套回滚句柄要解决的事(高危变更本来就没有可承诺的回滚点)。
	// 低危不受影响:低危变更做不做是风险问题,不是能力问题。
	//
	// 只改判 SUCCESS:失败结局(TRANSIENT / PERSISTENT)本来就是失败,把它们
	// 再改成 REFUSED 会让控制面放弃重试一次可能成功的装包。
	if resp.GetResult() == agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS &&
		req.RiskClass == agentv1.RiskClass_RISK_CLASS_HIGH && exec.HandleCapable(req.Kind) && resp.GetRollbackHandle() == "" {
		return s.refuseIntent(req, "apply_intent.refused",
			fmt.Sprintf("refused: high-risk intent applied but produced no rollback handle: kind=%s target=%s", req.Kind, req.Target),
			map[string]string{"reason": "rollback_handle_unavailable"}), nil
	}
	return resp, nil
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
