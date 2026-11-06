package exec

import (
	"context"
	"errors"
	"strings"
	"testing"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// fakeSystemd 模拟宿主 systemctl:show 回报观测态,变更动词翻转状态并记录 argv。
type fakeSystemd struct {
	exists    map[string]bool
	active    map[string]string // ActiveState
	fileState map[string]string // UnitFileState
	calls     [][]string

	showOut string
	showErr error
	verbOut string
	verbErr error
}

func (h *fakeSystemd) run(_ context.Context, name string, args ...string) (string, error) {
	h.calls = append(h.calls, append([]string{name}, args...))
	if name != "systemctl" {
		return "", errors.New("fakeSystemd: unexpected command " + name)
	}
	if len(args) > 0 && args[0] == "show" {
		unit := args[len(args)-1]
		if h.showErr != nil {
			return h.showOut, h.showErr
		}
		if !h.exists[unit] {
			// 真机语义:单元不存在时 systemctl show **仍返回 rc=0**,
			// 只有 LoadState=not-found / ActiveState=inactive。
			// 观测端必须靠 LoadState 判不存在,不能靠退出码。
			return "LoadState=not-found\nActiveState=inactive\nUnitFileState=\n", nil
		}
		return "LoadState=loaded\nActiveState=" + h.active[unit] + "\nUnitFileState=" + h.fileState[unit] + "\n", nil
	}

	verb, unit := args[0], args[1]
	if h.verbErr != nil {
		return h.verbOut, h.verbErr
	}
	if !h.exists[unit] {
		return "Unit " + unit + " not found.", errors.New("exit status 5")
	}
	switch verb {
	case "start":
		h.active[unit] = "active"
	case "stop":
		h.active[unit] = "inactive"
	case "enable":
		h.fileState[unit] = "enabled"
	case "disable":
		h.fileState[unit] = "disabled"
	}
	return "", nil
}

func withFakeSystemd(t *testing.T, h *fakeSystemd) {
	t.Helper()
	prev := runCmd
	t.Cleanup(func() { runCmd = prev })
	runCmd = h.run
}

func newFakeSystemd(unit string) *fakeSystemd {
	return &fakeSystemd{
		exists:    map[string]bool{unit: true},
		active:    map[string]string{unit: "inactive"},
		fileState: map[string]string{unit: "disabled"},
	}
}

func svcIntent(state, unit string) *agentv1.ApplyIntentRequest {
	return &agentv1.ApplyIntentRequest{
		IntentId: "i-svc",
		Kind:     kindService,
		Target:   unit,
		SpecJson: `{"desired_state":"` + state + `"}`,
	}
}

// 四值各自的 systemctl argv 必须正确(且只在需要变更时才发变更动词)。
func TestService_StartStopEnabledDisabled(t *testing.T) {
	const unit = "nginx.service"
	cases := []struct {
		state string
		setup func(*fakeSystemd)
		want  []string
	}{
		{"started", func(h *fakeSystemd) { h.active[unit] = "inactive" }, []string{"systemctl", "start", unit}},
		{"stopped", func(h *fakeSystemd) { h.active[unit] = "active" }, []string{"systemctl", "stop", unit}},
		{"enabled", func(h *fakeSystemd) { h.fileState[unit] = "disabled" }, []string{"systemctl", "enable", unit}},
		{"disabled", func(h *fakeSystemd) { h.fileState[unit] = "enabled" }, []string{"systemctl", "disable", unit}},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			h := newFakeSystemd(unit)
			tc.setup(h)
			withFakeSystemd(t, h)

			if err := Apply(context.Background(), svcIntent(tc.state, unit)); err != nil {
				t.Fatalf("Apply: unexpected err: %v", err)
			}
			got := h.calls[len(h.calls)-1]
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("argv = %v, got %v, want %v", got, got, tc.want)
			}
		})
	}
}

// 四值之外的 desired_state 拒绝,且不可重试。
func TestService_UnknownStateIsPersistent(t *testing.T) {
	const unit = "nginx.service"
	h := newFakeSystemd(unit)
	withFakeSystemd(t, h)

	err := Apply(context.Background(), svcIntent("restarted", unit))
	if !errors.Is(err, ErrPersistent) {
		t.Fatalf("got %v, want ErrPersistent", err)
	}
}

// 单元不存在 → 不可重试。
func TestService_MissingUnitIsPersistent(t *testing.T) {
	const unit = "nope.service"
	h := newFakeSystemd(unit)
	h.exists[unit] = false
	withFakeSystemd(t, h)

	err := Apply(context.Background(), svcIntent("started", unit))
	if !errors.Is(err, ErrPersistent) {
		t.Fatalf("got %v, want ErrPersistent", err)
	}
}

// dbus / systemd 不可达 → 可重试。
func TestService_DbusUnavailableIsTransient(t *testing.T) {
	const unit = "nginx.service"
	h := newFakeSystemd(unit)
	h.showOut = "Failed to connect to bus: No such file or directory"
	h.showErr = errors.New("exit status 1")
	withFakeSystemd(t, h)

	err := Apply(context.Background(), svcIntent("started", unit))
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("got %v, want ErrTransient", err)
	}
}

// 幂等:已在位则不发改变动词。
func TestService_IsIdempotent(t *testing.T) {
	const unit = "nginx.service"
	h := newFakeSystemd(unit)
	h.active[unit] = "active"
	h.fileState[unit] = "enabled"
	withFakeSystemd(t, h)

	for _, state := range []string{"started", "enabled"} {
		if err := Apply(context.Background(), svcIntent(state, unit)); err != nil {
			t.Fatalf("Apply(%s): unexpected err: %v", state, err)
		}
	}
	for _, c := range h.calls {
		if c[0] == "systemctl" && c[1] != "show" {
			t.Errorf("unexpected mutating call %v; already-satisfied state must be a no-op", c)
		}
	}
}

// 单元不存在时,**所有四值**都必须拒绝(真机上 systemctl show 对不存在的
// 单元仍 rc=0,只有 LoadState=not-found 能揭露它 —— 靠退出码观测会把
// "单元不存在"误读成"已停/已禁用"而回 SUCCESS)。
func TestService_MissingUnitRejectedForEveryState(t *testing.T) {
	const unit = "nope.service"
	for _, state := range []string{"started", "stopped", "enabled", "disabled"} {
		t.Run(state, func(t *testing.T) {
			h := newFakeSystemd(unit)
			h.exists[unit] = false
			withFakeSystemd(t, h)

			err := Apply(context.Background(), svcIntent(state, unit))
			if !errors.Is(err, ErrPersistent) {
				t.Fatalf("got %v, want ErrPersistent", err)
			}
			for _, c := range h.calls {
				if c[1] != "show" {
					t.Errorf("mutating call %v must not happen for a missing unit", c)
				}
			}
		})
	}
}

// 已取消的 ctx:必须在发变更命令前中止,且不改动宿主。
func TestService_CancelledContextAborts(t *testing.T) {
	const unit = "nginx.service"
	h := newFakeSystemd(unit)
	h.active[unit] = "inactive"
	withFakeSystemd(t, h)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Apply(ctx, svcIntent("started", unit))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	for _, c := range h.calls {
		if c[1] != "show" {
			t.Errorf("mutating call %v must not happen after cancellation", c)
		}
	}
}

// 幂等:变更动词跑完再连投同一 intent,不得再发一次变更命令。
func TestService_IdempotentAfterMutation(t *testing.T) {
	const unit = "nginx.service"
	h := newFakeSystemd(unit)
	withFakeSystemd(t, h)

	intent := svcIntent("started", unit)
	if err := Apply(context.Background(), intent); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if err := Apply(context.Background(), intent); err != nil {
		t.Fatalf("second Apply: unexpected err: %v", err)
	}

	var starts int
	for _, c := range h.calls {
		if len(c) > 1 && c[1] == "start" {
			starts++
		}
	}
	if starts != 1 {
		t.Errorf("systemctl start invocations = %d, got %d, want 1", starts, starts)
	}
}
