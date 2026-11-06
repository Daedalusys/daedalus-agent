package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// service.set 的 kind 字符串。上线的是**资源 kind**(objectmodel.Kind
// 的 "service"),不是 core 风险判定表内部用的适配器名 "service.set"。
const kindService = "service"

// service.set 的 desired_state 冻结四值。
const (
	stateStarted  = "started"
	stateStopped  = "stopped"
	stateEnabled  = "enabled"
	stateDisabled = "disabled"
)

// serviceVerbs 是 desired_state → systemctl 子命令的冻结映射。
var serviceVerbs = map[string]string{
	stateStarted:  "start",
	stateStopped:  "stop",
	stateEnabled:  "enable",
	stateDisabled: "disable",
}

// unitNameRe 是 systemd 单元名的白名单。形状:名字段 + 已知单元后缀。
// 名字段首字符限定为字母数字,后缀收窄到 systemd 认识的类型 —— 这样
// "--foo" / "/etc/passwd" / 带空白的注入串都进不来。
var unitNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.@-]*\.(service|socket|target|timer|mount|automount|path|slice|scope|swap|device)$`)

// serviceSpec 是 service.set 的 SpecJson schema。
type serviceSpec struct {
	DesiredState string `json:"desired_state"`
}

// parseServiceSpec 解析并校验 spec。desired_state 必须显式给出四值之一 ——
// service 的"默认动作"没有像 package 那样的唯一自然解读(启动还是启用?),
// 缺失即无法判定,拒绝。未知键同样拒(手改注入的多余键即拒)。
func parseServiceSpec(specJSON string) (string, error) {
	raw := strings.TrimSpace(specJSON)
	if raw == "" || raw == "null" {
		return "", fmt.Errorf("%w: service.set requires a spec_json object with \"desired_state\"", ErrPersistent)
	}
	var spec serviceSpec
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return "", fmt.Errorf("%w: invalid service.set spec_json: %v", ErrPersistent, err)
	}
	if _, ok := serviceVerbs[spec.DesiredState]; !ok {
		return "", fmt.Errorf("%w: unknown desired_state %q (v1 supports started|stopped|enabled|disabled)", ErrPersistent, spec.DesiredState)
	}
	return spec.DesiredState, nil
}

// unitState 是一次观测读到的单元状态。
type unitState struct {
	LoadState     string
	ActiveState   string
	UnitFileState string
}

// serviceExecutor 实现 service.set。无状态,可复用。
type serviceExecutor struct{}

// Apply 把 service.set 落到宿主 systemd 上。
//
// 观测一次读全 LoadState / ActiveState / UnitFileState:
//
//   - **不能靠退出码判存在性**:`systemctl show <不存在的单元>` 仍返回
//     rc=0(只有 LoadState=not-found),`systemctl is-enabled` 对 disabled
//     单元则返回 rc=1 —— 两个方向都会把观测判反(core#62 记过后者)。
//   - 单元不存在一律拒绝:否则 stop/disable 会因"当前不是 active/enabled"
//     直接 no-op 回 SUCCESS,把"改错了单元名"伪装成"已到位"。
func (serviceExecutor) Apply(ctx context.Context, intent *agentv1.ApplyIntentRequest) error {
	unit := intent.Target
	if !unitNameRe.MatchString(unit) {
		return fmt.Errorf("%w: invalid systemd unit name %q", ErrPersistent, unit)
	}
	state, err := parseServiceSpec(intent.SpecJson)
	if err != nil {
		return err
	}
	st, err := showUnitState(ctx, unit)
	if err != nil {
		return err
	}

	switch state {
	case stateStarted:
		if st.ActiveState == "active" {
			return nil
		}
	case stateStopped:
		if st.ActiveState != "active" {
			// inactive / failed / activating 之下再 stop 都是空操作。
			return nil
		}
	case stateEnabled:
		if st.UnitFileState == "enabled" {
			return nil
		}
	case stateDisabled:
		if st.UnitFileState == "disabled" {
			return nil
		}
	}
	return runSystemctl(ctx, serviceVerbs[state], unit)
}

// showUnitState 一次读全单元的三个属性。拿不到 LoadState 视为无法观测
// (fail-closed),不猜。
func showUnitState(ctx context.Context, unit string) (unitState, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return unitState{}, fmt.Errorf("exec: service.set %q cancelled: %w", unit, ctxErr)
	}
	out, err := runCmd(ctx, "systemctl", "show", "--property=LoadState,ActiveState,UnitFileState", unit)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return unitState{}, fmt.Errorf("exec: service.set %q cancelled: %w", unit, ctxErr)
		}
		return unitState{}, classifySystemctlError(out, err)
	}

	var st unitState
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "LoadState":
			st.LoadState = val
		case "ActiveState":
			st.ActiveState = val
		case "UnitFileState":
			st.UnitFileState = val
		}
	}
	if st.LoadState == "" {
		return unitState{}, fmt.Errorf("%w: systemctl show returned no LoadState for %q: %s", ErrPersistent, unit, firstLine(out))
	}
	if st.LoadState == "not-found" {
		return unitState{}, fmt.Errorf("%w: unit %q does not exist (LoadState=not-found)", ErrPersistent, unit)
	}
	return st, nil
}

// runSystemctl 发一条变更动词。
func runSystemctl(ctx context.Context, verb, unit string) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("exec: service.set %s %q cancelled: %w", verb, unit, ctxErr)
	}
	out, err := runCmd(ctx, "systemctl", verb, unit)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("exec: service.set %s %q cancelled: %w", verb, unit, ctxErr)
		}
		return classifySystemctlError(out, err)
	}
	return nil
}

// classifySystemctlError 把 systemctl 输出归类成可重试性。判据不明时按
// PERSISTENT 处理(不可重试),与 package 同款 fail-closed 姿态。
func classifySystemctlError(output string, err error) error {
	low := strings.ToLower(output)

	// dbus / systemd 侧不可达或未就绪 → 等一下再试可能就好。
	for _, p := range []string{
		"failed to connect to bus",
		"failed to get d-bus connection",
		"failed to connect to socket",
		"system has not been booted with systemd",
		"connection refused",
		"transport endpoint is not connected",
	} {
		if strings.Contains(low, p) {
			return fmt.Errorf("%w: systemctl: %s", ErrTransient, firstLine(output))
		}
	}

	// 单元不存在 → 重试无益。
	for _, p := range []string{
		"could not be found",
		"not found",
		"no such unit",
		"unit file is masked",
	} {
		if strings.Contains(low, p) {
			return fmt.Errorf("%w: systemctl: unit not found: %s", ErrPersistent, firstLine(output))
		}
	}

	return fmt.Errorf("%w: systemctl: unclassified failure: %s (%v)", ErrPersistent, firstLine(output), err)
}
