package exec

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// dnf 装完包必须留下可持久引用的回滚句柄:控制面只存这个串,事后靠它发起
// 回滚。句柄里的 id 必须是**本次**事务,不是同包的历史事务。
func TestPackage_HandleAfterDnfInstall(t *testing.T) {
	h := &fakeHost{manager: "dnf", installed: map[string]bool{}}
	h.historyOut = `[{"id":41,"command_line":"upgrade curl"},{"id":42,"command_line":"install nginx"}]`
	withFakeHost(t, h)

	handle, err := ApplyWithHandle(context.Background(), pkgIntent(`{"desired_state":"present"}`, "nginx"))
	if err != nil {
		t.Fatalf("ApplyWithHandle: unexpected err: %v", err)
	}
	if want := handleDnfHistory + "42"; handle != want {
		t.Fatalf("handle = %q, want %q", handle, want)
	}
}

// apt 同理:句柄指向历史里本次变更那一条,且必须跳过表头与其他包的事务。
func TestPackage_HandleAfterAptInstall(t *testing.T) {
	h := &fakeHost{manager: "apt", installed: map[string]bool{}}
	h.historyOut = "ID                 Command line\n" +
		"     6             install curl\n" +
		"     7             install nginx\n"
	withFakeHost(t, h)

	handle, err := ApplyWithHandle(context.Background(), pkgIntent(`{"desired_state":"present"}`, "nginx"))
	if err != nil {
		t.Fatalf("ApplyWithHandle: unexpected err: %v", err)
	}
	if want := handleAptHistory + "7"; handle != want {
		t.Fatalf("handle = %q, want %q", handle, want)
	}
}

// apply 是 no-op 时不能给句柄:句柄会让控制面事后"回滚"一次从未发生的变更
// (比如把本来就在跑的 nginx 停掉)。
func TestPackage_HandleEmptyWhenNoChange(t *testing.T) {
	h := &fakeHost{manager: "dnf", installed: map[string]bool{"nginx": true}}
	h.historyOut = `[{"id":42,"command_line":"install nginx"}]`
	withFakeHost(t, h)

	handle, err := ApplyWithHandle(context.Background(), pkgIntent(`{"desired_state":"present"}`, "nginx"))
	if err != nil {
		t.Fatalf("ApplyWithHandle: unexpected err: %v", err)
	}
	if handle != "" {
		t.Fatalf("handle = %q, want empty: 本次 apply 没有改动宿主", handle)
	}
}

// 句柄捕获失败只能让句柄为空,绝不能再把已经落地的 apply 报成失败
// (否则控制面会重试一个已经生效的变更)。
func TestPackage_HandleCaptureFailureStillSucceeds(t *testing.T) {
	h := &fakeHost{manager: "dnf", installed: map[string]bool{}}
	h.historyOut = "error: cannot read history"
	h.historyErr = errors.New("exit status 1")
	withFakeHost(t, h)

	handle, err := ApplyWithHandle(context.Background(), pkgIntent(`{"desired_state":"present"}`, "nginx"))
	if err != nil {
		t.Fatalf("apply 结局被句柄捕获失败改写了: %v", err)
	}
	if handle != "" {
		t.Fatalf("handle = %q, want empty", handle)
	}
}

// service 的句柄必须带 apply **之前**的状态:apply 之后读到的状态是变更
// 结果,拿它算反向动词只会原地打转。
func TestService_HandleCapturesPreState(t *testing.T) {
	const unit = "nginx.service"
	h := newFakeSystemd(unit) // ActiveState=inactive / UnitFileState=disabled
	withFakeSystemd(t, h)

	handle, err := ApplyWithHandle(context.Background(), svcIntent("started", unit))
	if err != nil {
		t.Fatalf("ApplyWithHandle: unexpected err: %v", err)
	}
	if !strings.HasPrefix(handle, handleService) {
		t.Fatalf("handle = %q, want prefix %q", handle, handleService)
	}
	st, err := decodeServiceState(strings.TrimPrefix(handle, handleService))
	if err != nil {
		t.Fatalf("decodeServiceState: %v", err)
	}
	if st.Unit != unit {
		t.Errorf("Unit = %q, want %q", st.Unit, unit)
	}
	if st.ActiveState != "inactive" {
		t.Errorf("ActiveState = %q, want inactive (apply 前的状态)", st.ActiveState)
	}
	if st.UnitFileState != "disabled" {
		t.Errorf("UnitFileState = %q, want disabled", st.UnitFileState)
	}
}

// service 是 no-op 时不给句柄(理由同 package:回滚会停掉一个我们没启动的服务)。
func TestService_HandleEmptyWhenNoChange(t *testing.T) {
	const unit = "nginx.service"
	h := newFakeSystemd(unit)
	h.active[unit] = "active"
	withFakeSystemd(t, h)

	handle, err := ApplyWithHandle(context.Background(), svcIntent("started", unit))
	if err != nil {
		t.Fatalf("ApplyWithHandle: unexpected err: %v", err)
	}
	if handle != "" {
		t.Fatalf("handle = %q, want empty", handle)
	}
}

// file 的回滚点仍是内建 copy,不产句柄。
func TestHandle_FileKindHasNoHandle(t *testing.T) {
	target := filepath.Join(t.TempDir(), "app.conf")
	intent := &agentv1.ApplyIntentRequest{
		IntentId: "i-file",
		Kind:     kindFile,
		Target:   target,
		SpecJson: `{"content":"hello"}`,
	}
	handle, err := ApplyWithHandle(context.Background(), intent)
	if err != nil {
		t.Fatalf("ApplyWithHandle: unexpected err: %v", err)
	}
	if handle != "" {
		t.Fatalf("handle = %q, want empty for kind=file", handle)
	}
}

// 句柄前缀是控制面与 agent 之间的契约:认识的前缀要能拆出 id,不认识的一律
// 报错(fail-closed,不猜)。
func TestHandle_DecodeKnownAndUnknown(t *testing.T) {
	kind, id, err := decodeHandle(handleDnfHistory + "42")
	if err != nil {
		t.Fatalf("decodeHandle: %v", err)
	}
	if kind != "dnf-history" || id != "42" {
		t.Fatalf("decodeHandle = (%q,%q), want (dnf-history,42)", kind, id)
	}
	if _, _, err := decodeHandle("bogus:1"); err == nil {
		t.Fatal("decodeHandle(bogus) = nil error, want error")
	}
}
