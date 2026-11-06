package exec_test

import (
	"context"
	"errors"
	"testing"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/exec"
)

// fakeExecutor 记录分派是否真的落到它身上;用于验证 Apply 的 kind 查表,
// 而不是验证任何真实执行语义(那是 package/file/service 各自测试的事)。
type fakeExecutor struct {
	calls      int
	lastIntent *agentv1.ApplyIntentRequest
	err        error
}

func (f *fakeExecutor) Apply(_ context.Context, intent *agentv1.ApplyIntentRequest) error {
	f.calls++
	f.lastIntent = intent
	return f.err
}

// 计划表外(或拼错)的 kind 必须 fail-closed:不得猜、不得静默成功。
func TestApply_UnknownKindIsUnsupported(t *testing.T) {
	err := exec.Apply(context.Background(), &agentv1.ApplyIntentRequest{Kind: "bogus.set"})
	if !errors.Is(err, exec.ErrUnsupportedKind) {
		t.Fatalf("got %v, want %v", err, exec.ErrUnsupportedKind)
	}
}

// 命中的 kind 必须原样转发给注册的执行器,且请求对象不被复制/篡改。
func TestApply_DispatchesToRegisteredKind(t *testing.T) {
	fake := &fakeExecutor{}
	exec.Register("fake.set", fake)
	intent := &agentv1.ApplyIntentRequest{IntentId: "i-1", Kind: "fake.set", Target: "x"}

	if err := exec.Apply(context.Background(), intent); err != nil {
		t.Fatalf("Apply: unexpected err: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("fake calls = %d, want 1", fake.calls)
	}
	if fake.lastIntent != intent {
		t.Errorf("dispatched intent = %v, want the original request", fake.lastIntent)
	}
}

// 空 kind 与未知 kind 同罪:fail-closed。
func TestApply_EmptyKindIsUnsupported(t *testing.T) {
	err := exec.Apply(context.Background(), &agentv1.ApplyIntentRequest{Kind: ""})
	if !errors.Is(err, exec.ErrUnsupportedKind) {
		t.Fatalf("got %v, want %v", err, exec.ErrUnsupportedKind)
	}
}
