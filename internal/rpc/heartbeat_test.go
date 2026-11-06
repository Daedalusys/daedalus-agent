package rpc

import (
	"context"
	"sync/atomic"
	"testing"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/audit"
	"github.com/Daedalusys/daedalus-agent/internal/config"
	"github.com/Daedalusys/daedalus-agent/internal/rollback"
	"github.com/Daedalusys/daedalus-agent/internal/snapshot"
	"github.com/Daedalusys/daedalus-agent/internal/tx"
)

// noopBackend 让 ApplyIntent 能越过 RF3 的 backend 选择,而不碰真实宿主。
type noopBackend struct{}

func (noopBackend) CreateSnapshot(string, string) error { return nil }
func (noopBackend) Restore(string, string) error        { return nil }

// heartbeatTestService 造一个已注册的最小 agentService(Heartbeat 只读
// node_id 与 pending 集合,不需要 cert / snapshot 真值)。
func heartbeatTestService(t *testing.T) *agentService {
	t.Helper()
	svc := newAgentService(&config.Config{DataDir: t.TempDir()}, nil, snapshot.NewDetector(), nil, nil, nil, nil)
	svc.nodeID = "node-1"
	return svc
}

func heartbeatFrom(t *testing.T, svc *agentService) *agentv1.HeartbeatResponse {
	t.Helper()
	resp, err := svc.Heartbeat(context.Background(), &agentv1.HeartbeatRequest{NodeId: svc.nodeID})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	return resp
}

// 有两个未完成的 intent 时,pending_intents 必须回报 2(不再是硬编码 0)。
func TestHeartbeat_PendingReflectsQueued(t *testing.T) {
	svc := heartbeatTestService(t)
	svc.beginIntent("intent-a")
	svc.beginIntent("intent-b")

	resp := heartbeatFrom(t, svc)
	if resp.PendingIntents != 2 {
		t.Errorf("PendingIntents = %d, want 2", resp.PendingIntents)
	}
	if !resp.Ack {
		t.Errorf("Ack = false, got %v, want true", resp.Ack)
	}
}

// 空闲时是 0。
func TestHeartbeat_PendingZeroWhenIdle(t *testing.T) {
	svc := heartbeatTestService(t)
	if resp := heartbeatFrom(t, svc); resp.PendingIntents != 0 {
		t.Errorf("PendingIntents = %d, want 0", resp.PendingIntents)
	}
}

// intent 完成后计数必须回落(否则计数只增不减,等于没有意义)。
func TestHeartbeat_PendingDropsWhenIntentFinishes(t *testing.T) {
	svc := heartbeatTestService(t)
	svc.beginIntent("intent-a")
	svc.endIntent("intent-a")

	if resp := heartbeatFrom(t, svc); resp.PendingIntents != 0 {
		t.Errorf("PendingIntents = %d, want 0 after the intent finished", resp.PendingIntents)
	}
}

// recordingTx 记录 Run 被调用时 service 视角的 pending 数,并回报 SUCCESS。
type recordingTx struct {
	svc     *agentService
	seen    int
	runDone bool
}

func (r *recordingTx) Run(_ context.Context, _ *agentv1.ApplyIntentRequest) (*agentv1.ApplyIntentResponse, error) {
	r.seen = r.svc.pendingIntentCount()
	r.runDone = true
	return &agentv1.ApplyIntentResponse{
		Result: agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS,
		TxId:   "tx-test",
	}, nil
}

// ApplyIntent 必须在其事务执行期间把该 intent 计入 pending,并在返回前清掉 ——
// pending_intents 才有资格叫"当前未完成"。
func TestApplyIntent_MarksPendingDuringRun(t *testing.T) {
	det := snapshot.NewDetector()
	det.SetCapability(agentv1.SnapshotCapability_SNAPSHOT_CAPABILITY_BTRFS_SUBVOLUME)
	svc := newAgentService(&config.Config{DataDir: t.TempDir()}, nil, det, nil, noopBackend{}, nil, nil)

	rt := &recordingTx{svc: svc}
	svc.txFactory = func(rollback.Backend, string, *atomic.Uint64, *audit.Local) tx.Tx { return rt }

	resp, err := svc.ApplyIntent(context.Background(), &agentv1.ApplyIntentRequest{
		IntentId: "intent-1", Kind: "package", Target: "nginx",
	})
	if err != nil {
		t.Fatalf("ApplyIntent: %v", err)
	}
	if resp.Result != agentv1.ApplyIntentResult_APPLY_INTENT_RESULT_SUCCESS {
		t.Fatalf("Result = %v, want SUCCESS", resp.Result)
	}
	if !rt.runDone {
		t.Fatal("tx.Run was never called")
	}
	if rt.seen != 1 {
		t.Errorf("pending during tx.Run = %d, want 1", rt.seen)
	}
	if got := svc.pendingIntentCount(); got != 0 {
		t.Errorf("pending after ApplyIntent returned = %d, want 0", got)
	}
}

// 同一 intent_id 并发投递两次:两次都要计入。先结束的那次不能把仍在跑的
// 那次一起抹掉(否则 pending_intents 会少报,控制面以为节点空闲)。
func TestHeartbeat_PendingCountsConcurrentDuplicateIntent(t *testing.T) {
	svc := heartbeatTestService(t)
	svc.beginIntent("intent-a")
	svc.beginIntent("intent-a")

	if got := svc.pendingIntentCount(); got != 2 {
		t.Errorf("pending count = %d, want 2", got)
	}
	svc.endIntent("intent-a")
	if got := svc.pendingIntentCount(); got != 1 {
		t.Errorf("pending count after one finished = %d, want 1", got)
	}
	svc.endIntent("intent-a")
	if got := svc.pendingIntentCount(); got != 0 {
		t.Errorf("pending count after both finished = %d, want 0", got)
	}
}
