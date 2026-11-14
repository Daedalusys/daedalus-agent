// RollbackIntent 的 RPC 行为:PLAN 只出计划、APPLY 校验 token 后执行;
// 以及 ApplyIntent 的回滚句柄回填与高危准入。
package rpc_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/audit"
	"github.com/Daedalusys/daedalus-agent/internal/config"
	"github.com/Daedalusys/daedalus-agent/internal/rollback"
	"github.com/Daedalusys/daedalus-agent/internal/rpc"
	"github.com/Daedalusys/daedalus-agent/internal/tx"
)

// fakeRollbackRunner 记录回滚执行;不碰宿主。
type fakeRollbackRunner struct {
	mu    sync.Mutex
	calls [][]rollback.Step
	err   error
}

func (f *fakeRollbackRunner) Run(_ context.Context, steps []rollback.Step) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]rollback.Step(nil), steps...))
	return f.err
}

func (f *fakeRollbackRunner) recorded() [][]rollback.Step {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]rollback.Step(nil), f.calls...)
}

// startRollbackRig 起一个注入了 fake runner 的服务;dataDir 非空时复用
// 同一个 data dir(用来模拟进程重启:token key 还是那份)。
func startRollbackRig(t *testing.T, runner rpc.RollbackRunner, dataDir string) *testRig {
	t.Helper()
	return startTestServerFull(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE,
		func(cfg *config.Config) {
			if dataDir != "" {
				cfg.DataDir = dataDir
			}
		},
		func(d *rpc.ServiceDeps) {
			d.RollbackRunner = runner
		})
}

// planDnf 发一次 PLAN 并返回响应(helper 让 APPLY 用例复用同一份计划)。
func planDnf(t *testing.T, rig *testRig, intentID string) *agentv1.RollbackIntentResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := rig.Client.RollbackIntent(ctx, &agentv1.RollbackIntentRequest{
		IntentId:       intentID,
		RollbackHandle: "dnf-history:42",
		Mode:           agentv1.RollbackMode_ROLLBACK_MODE_PLAN,
	})
	if err != nil {
		t.Fatalf("RollbackIntent(PLAN): %v", err)
	}
	return resp
}

// PLAN 只出计划:返回步骤 + plan_hash + token,一条命令都不执行。
func TestRollbackIntent_PlanDoesNotExecute(t *testing.T) {
	runner := &fakeRollbackRunner{}
	rig := startRollbackRig(t, runner, "")
	defer rig.Cleanup()

	resp := planDnf(t, rig, "i-rb-plan")
	if resp.GetResult() != agentv1.RollbackResult_ROLLBACK_RESULT_PLANNED {
		t.Fatalf("result = %v, want PLANNED (%s)", resp.GetResult(), resp.GetErrorMessage())
	}
	if resp.GetApproveToken() == "" {
		t.Error("PLAN 必须返回 approve_token")
	}
	if resp.GetPlanHash() == "" {
		t.Error("PLAN 必须返回 plan_hash")
	}
	if len(resp.GetSteps()) != 1 {
		t.Fatalf("steps = %d, want 1", len(resp.GetSteps()))
	}
	if got := resp.GetSteps()[0].GetCommand(); got != "dnf history undo 42" {
		t.Errorf("command = %q, want dnf history undo 42", got)
	}
	if calls := runner.recorded(); len(calls) != 0 {
		t.Fatalf("PLAN 执行了 %d 次回滚, want 0", len(calls))
	}
}

// APPLY 缺 token → REFUSED,且不执行任何步骤。
func TestRollbackIntent_ApplyWithoutTokenRefused(t *testing.T) {
	runner := &fakeRollbackRunner{}
	rig := startRollbackRig(t, runner, "")
	defer rig.Cleanup()

	plan := planDnf(t, rig, "i-rb-notoken")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := rig.Client.RollbackIntent(ctx, &agentv1.RollbackIntentRequest{
		IntentId:       "i-rb-notoken",
		RollbackHandle: "dnf-history:42",
		Mode:           agentv1.RollbackMode_ROLLBACK_MODE_APPLY,
		Steps:          plan.GetSteps(),
		PlanHash:       plan.GetPlanHash(),
	})
	if err != nil {
		t.Fatalf("RollbackIntent(APPLY): %v", err)
	}
	if resp.GetResult() != agentv1.RollbackResult_ROLLBACK_RESULT_REFUSED {
		t.Fatalf("result = %v, want REFUSED", resp.GetResult())
	}
	if calls := runner.recorded(); len(calls) != 0 {
		t.Fatalf("缺 token 时执行了 %d 次回滚, want 0", len(calls))
	}
}

// APPLY 正常路径:token 验过 → 执行计划里的步骤 → APPLIED。
func TestRollbackIntent_ApplyHappyPath(t *testing.T) {
	runner := &fakeRollbackRunner{}
	rig := startRollbackRig(t, runner, "")
	defer rig.Cleanup()

	plan := planDnf(t, rig, "i-rb-apply")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := rig.Client.RollbackIntent(ctx, &agentv1.RollbackIntentRequest{
		IntentId:       "i-rb-apply",
		RollbackHandle: "dnf-history:42",
		Mode:           agentv1.RollbackMode_ROLLBACK_MODE_APPLY,
		Steps:          plan.GetSteps(),
		PlanHash:       plan.GetPlanHash(),
		ApproveToken:   plan.GetApproveToken(),
	})
	if err != nil {
		t.Fatalf("RollbackIntent(APPLY): %v", err)
	}
	if resp.GetResult() != agentv1.RollbackResult_ROLLBACK_RESULT_APPLIED {
		t.Fatalf("result = %v, want APPLIED (%s)", resp.GetResult(), resp.GetErrorMessage())
	}
	calls := runner.recorded()
	if len(calls) != 1 {
		t.Fatalf("回滚执行次数 = %d, want 1", len(calls))
	}
	if len(calls[0]) != 1 || calls[0][0].Command != "dnf history undo 42" {
		t.Fatalf("执行的步骤 = %+v, want dnf history undo 42", calls[0])
	}
}

// 同一个 token 第二次 APPLY 必须被拒(单次使用)。
func TestRollback_TokenSingleUse(t *testing.T) {
	runner := &fakeRollbackRunner{}
	rig := startRollbackRig(t, runner, "")
	defer rig.Cleanup()

	plan := planDnf(t, rig, "i-rb-single")
	apply := func() *agentv1.RollbackIntentResponse {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := rig.Client.RollbackIntent(ctx, &agentv1.RollbackIntentRequest{
			IntentId:       "i-rb-single",
			RollbackHandle: "dnf-history:42",
			Mode:           agentv1.RollbackMode_ROLLBACK_MODE_APPLY,
			Steps:          plan.GetSteps(),
			PlanHash:       plan.GetPlanHash(),
			ApproveToken:   plan.GetApproveToken(),
		})
		if err != nil {
			t.Fatalf("RollbackIntent(APPLY): %v", err)
		}
		return resp
	}

	if resp := apply(); resp.GetResult() != agentv1.RollbackResult_ROLLBACK_RESULT_APPLIED {
		t.Fatalf("first APPLY = %v, want APPLIED (%s)", resp.GetResult(), resp.GetErrorMessage())
	}
	if resp := apply(); resp.GetResult() != agentv1.RollbackResult_ROLLBACK_RESULT_REFUSED {
		t.Fatalf("second APPLY = %v, want REFUSED", resp.GetResult())
	}
	if calls := runner.recorded(); len(calls) != 1 {
		t.Fatalf("回滚执行次数 = %d, want 1(第二次必须被拒)", len(calls))
	}
}

// 进程重启后旧 token 失效:同一个 data dir 上的新服务不认上一个进程签的 token。
func TestRollback_TokenInvalidAfterRestart(t *testing.T) {
	dataDir := t.TempDir()

	first := startRollbackRig(t, &fakeRollbackRunner{}, dataDir)
	plan := planDnf(t, first, "i-rb-restart")
	first.Cleanup()

	runner := &fakeRollbackRunner{}
	second := startRollbackRig(t, runner, dataDir)
	defer second.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := second.Client.RollbackIntent(ctx, &agentv1.RollbackIntentRequest{
		IntentId:       "i-rb-restart",
		RollbackHandle: "dnf-history:42",
		Mode:           agentv1.RollbackMode_ROLLBACK_MODE_APPLY,
		Steps:          plan.GetSteps(),
		PlanHash:       plan.GetPlanHash(),
		ApproveToken:   plan.GetApproveToken(),
	})
	if err != nil {
		t.Fatalf("RollbackIntent(APPLY): %v", err)
	}
	if resp.GetResult() != agentv1.RollbackResult_ROLLBACK_RESULT_REFUSED {
		t.Fatalf("重启后 APPLY = %v, want REFUSED", resp.GetResult())
	}
	if calls := runner.recorded(); len(calls) != 0 {
		t.Fatalf("重启后执行了 %d 次回滚, want 0", len(calls))
	}
}

// 计划哈希被换掉(admin 中途改了步骤) → REFUSED。
func TestRollback_RejectsTamperedPlan(t *testing.T) {
	runner := &fakeRollbackRunner{}
	rig := startRollbackRig(t, runner, "")
	defer rig.Cleanup()

	plan := planDnf(t, rig, "i-rb-tamper")
	steps := plan.GetSteps()
	steps[0].Command = "dnf history undo 43" // 改了命令,哈希不再对得上

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := rig.Client.RollbackIntent(ctx, &agentv1.RollbackIntentRequest{
		IntentId:       "i-rb-tamper",
		RollbackHandle: "dnf-history:42",
		Mode:           agentv1.RollbackMode_ROLLBACK_MODE_APPLY,
		Steps:          steps,
		PlanHash:       plan.GetPlanHash(),
		ApproveToken:   plan.GetApproveToken(),
	})
	if err != nil {
		t.Fatalf("RollbackIntent(APPLY): %v", err)
	}
	if resp.GetResult() != agentv1.RollbackResult_ROLLBACK_RESULT_REFUSED {
		t.Fatalf("result = %v, want REFUSED", resp.GetResult())
	}
	if calls := runner.recorded(); len(calls) != 0 {
		t.Fatalf("被篡改的计划执行了 %d 次, want 0", len(calls))
	}
}

// admin 改过的计划里塞了未标人审的 rm -rf → PLAN 就拒绝。
func TestRollback_RejectsUnmarkedDestructiveStep(t *testing.T) {
	rig := startRollbackRig(t, &fakeRollbackRunner{}, "")
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := rig.Client.RollbackIntent(ctx, &agentv1.RollbackIntentRequest{
		IntentId:       "i-rb-rm",
		RollbackHandle: "dnf-history:42",
		Mode:           agentv1.RollbackMode_ROLLBACK_MODE_PLAN,
		Steps: []*agentv1.RollbackStep{{
			Kind:    agentv1.RollbackStepKind_ROLLBACK_STEP_KIND_FILE_DELETE,
			Command: "rm -rf /var/lib/nginx",
			// HumanReviewRequired 漏标
		}},
	})
	if err != nil {
		t.Fatalf("RollbackIntent(PLAN): %v", err)
	}
	if resp.GetResult() != agentv1.RollbackResult_ROLLBACK_RESULT_REFUSED {
		t.Fatalf("result = %v, want REFUSED", resp.GetResult())
	}
}

// 标了人审但路径不归包所有 → 仍然拒绝。
func TestRollback_RejectsNonPackageOwnedRm(t *testing.T) {
	rig := startRollbackRig(t, &fakeRollbackRunner{}, "")
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := rig.Client.RollbackIntent(ctx, &agentv1.RollbackIntentRequest{
		IntentId:       "i-rb-rm-owned",
		RollbackHandle: "dnf-history:42",
		Mode:           agentv1.RollbackMode_ROLLBACK_MODE_PLAN,
		Steps: []*agentv1.RollbackStep{{
			Kind:                agentv1.RollbackStepKind_ROLLBACK_STEP_KIND_FILE_DELETE,
			Command:             "rm -rf /etc/shadow",
			HumanReviewRequired: true,
		}},
	})
	if err != nil {
		t.Fatalf("RollbackIntent(PLAN): %v", err)
	}
	if resp.GetResult() != agentv1.RollbackResult_ROLLBACK_RESULT_REFUSED {
		t.Fatalf("result = %v, want REFUSED", resp.GetResult())
	}
}

// handleTxFactory 返回一个恒 SUCCESS 的事务,并回填给定句柄。
func handleTxFactory(handle string) rpc.TxFactory {
	return func(rollback.Backend, string, *atomic.Uint64, *audit.Local) tx.Tx {
		return cannedTx{onRun: func(_ context.Context, req *agentv1.ApplyIntentRequest) (*agentv1.ApplyIntentResponse, error) {
			return &agentv1.ApplyIntentResponse{
				Result:         agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS,
				TxId:           "tx-" + req.IntentId,
				RollbackHandle: handle,
			}, nil
		}}
	}
}

// 句柄捕获失败(空句柄)不得把 apply 报成失败:变更已经在宿主上了。
func TestApplyIntent_HandleCaptureFailureStillSucceeds(t *testing.T) {
	rig := startTestServerWithTx(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE, handleTxFactory(""))
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := rig.Client.ApplyIntent(ctx, &agentv1.ApplyIntentRequest{
		IntentId:  "i-cap-fail",
		Kind:      "package",
		Target:    "nginx",
		RiskClass: agentv1.RiskClass_RISK_CLASS_LOW,
	})
	if err != nil {
		t.Fatalf("ApplyIntent: %v", err)
	}
	if resp.GetResult() != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS {
		t.Fatalf("result = %v, want SUCCESS (句柄缺失不改变 apply 结局)", resp.GetResult())
	}
	if resp.GetRollbackHandle() != "" {
		t.Fatalf("rollback_handle = %q, want empty", resp.GetRollbackHandle())
	}
}

// 高危 + 有句柄 → 放行,并把句柄带回控制面。
func TestApplyIntent_HighRiskWithHandleAllowed(t *testing.T) {
	rig := startTestServerWithTx(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE, handleTxFactory("dnf-history:42"))
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := rig.Client.ApplyIntent(ctx, &agentv1.ApplyIntentRequest{
		IntentId:  "i-high-handle",
		Kind:      "package",
		Target:    "kernel-core",
		RiskClass: agentv1.RiskClass_RISK_CLASS_HIGH,
	})
	if err != nil {
		t.Fatalf("ApplyIntent: %v", err)
	}
	if resp.GetResult() != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS {
		t.Fatalf("result = %v, want SUCCESS (%s)", resp.GetResult(), resp.GetErrorMessage())
	}
	if resp.GetRollbackHandle() != "dnf-history:42" {
		t.Fatalf("rollback_handle = %q, want dnf-history:42", resp.GetRollbackHandle())
	}
}

// 高危 + package/service + 空句柄 → 改判拒绝(低危不受影响)。
func TestApplyIntent_HighRiskWithoutHandleRefused(t *testing.T) {
	rig := startTestServerWithTx(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE, handleTxFactory(""))
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := rig.Client.ApplyIntent(ctx, &agentv1.ApplyIntentRequest{
		IntentId:  "i-high-nohandle",
		Kind:      "service",
		Target:    "nginx.service",
		RiskClass: agentv1.RiskClass_RISK_CLASS_HIGH,
	})
	if err != nil {
		t.Fatalf("ApplyIntent: %v", err)
	}
	if resp.GetResult() != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_REFUSED_SNAPSHOT_CAPABILITY_NONE {
		t.Fatalf("result = %v, want REFUSED", resp.GetResult())
	}
	if !strings.Contains(resp.GetErrorMessage(), "rollback handle") {
		t.Errorf("文案 = %q, want 含 rollback handle", resp.GetErrorMessage())
	}
}

// 高危 package 的失败结局不能被"没有句柄"这一条改写:可重试的失败必须仍是
// TRANSIENT_FAILURE,否则控制面不会重试一次本来能成功的装包。
func TestApplyIntent_HighRiskTransientFailureNotRejudged(t *testing.T) {
	rig := startTestServerWithTx(t, agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_NONE,
		func(rollback.Backend, string, *atomic.Uint64, *audit.Local) tx.Tx {
			return cannedTx{onRun: func(_ context.Context, req *agentv1.ApplyIntentRequest) (*agentv1.ApplyIntentResponse, error) {
				return &agentv1.ApplyIntentResponse{
					Result:       agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_TRANSIENT_FAILURE,
					ErrorMessage: "package manager lock held",
				}, nil
			}}
		})
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := rig.Client.ApplyIntent(ctx, &agentv1.ApplyIntentRequest{
		IntentId:  "i-high-transient",
		Kind:      "package",
		Target:    "kernel-core",
		RiskClass: agentv1.RiskClass_RISK_CLASS_HIGH,
	})
	if err != nil {
		t.Fatalf("ApplyIntent: %v", err)
	}
	if resp.GetResult() != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_TRANSIENT_FAILURE {
		t.Fatalf("result = %v, want TRANSIENT_FAILURE(失败结局不该被改判)", resp.GetResult())
	}
}

// 计划里只要有一step要求人审,整份计划就不能标成可自动执行 —— 否则 admin
// 往自动计划里塞一条人审步骤,控制面会照 auto_ok 无人值守地跑掉。
func TestRollback_PlanWithHumanReviewIsNotAuto(t *testing.T) {
	rig := startRollbackRig(t, &fakeRollbackRunner{}, "")
	defer rig.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := rig.Client.RollbackIntent(ctx, &agentv1.RollbackIntentRequest{
		IntentId:       "i-rb-review",
		RollbackHandle: "dnf-history:42",
		Mode:           agentv1.RollbackMode_ROLLBACK_MODE_PLAN,
		Steps: []*agentv1.RollbackStep{{
			Kind:                agentv1.RollbackStepKind_ROLLBACK_STEP_KIND_NATIVE_UNDO,
			Command:             "dnf history undo 42",
			Rationale:           "admin 改过的步骤,要求人审",
			HumanReviewRequired: true,
		}},
	})
	if err != nil {
		t.Fatalf("RollbackIntent(PLAN): %v", err)
	}
	if resp.GetResult() != agentv1.RollbackResult_ROLLBACK_RESULT_PLANNED {
		t.Fatalf("result = %v, want PLANNED (%s)", resp.GetResult(), resp.GetErrorMessage())
	}

	events, _, err := rig.Audit.Since(0)
	if err != nil {
		t.Fatalf("audit.Since: %v", err)
	}
	var autoOK string
	for _, ev := range events {
		if ev.Tool == "rollback.planned" && ev.Identity == "i-rb-review" {
			autoOK = ev.Args["auto_ok"]
		}
	}
	if autoOK != "false" {
		t.Fatalf("链上 auto_ok = %q, want false(含人审步骤的计划不得自动执行)", autoOK)
	}
}
