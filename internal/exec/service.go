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
// 缺失即无法判定,拒绝。
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
	switch spec.DesiredState {
	case stateStarted, stateStopped, stateEnabled, stateDisabled:
		return spec.DesiredState, nil
	}
	return "", fmt.Errorf("%w: unknown desired_state %q (v1 supports started|stopped|enabled|disabled)", ErrPersistent, spec.DesiredState)
}

// serviceExecutor 实现 service.set。无状态,可复用。
type serviceExecutor struct{}

// Apply 把 service.set 落到宿主 systemd 上。
//
// 观测一律走 `systemctl show --property=<属性> <unit>`:它对 inactive /
// disabled 的单元仍返回 rc=0。**不要**用 `systemctl is-enabled` 的退出码
// 判观测 —— 它对 disabled 单元返回 rc=1,会把"观测到了 disabled"误读成
// "观测失败"(core#62 已记录过这个坑)。
func (serviceExecutor) Apply(ctx context.Context, intent *agentv1.ApplyIntentRequest) error {
	unit := intent.Target
	if !unitNameRe.MatchString(unit) {
		return fmt.Errorf("%w: invalid systemd unit name %q", ErrPersistent, unit)
	}
	state, err := parseServiceSpec(intent.SpecJson)
	if err != nil {
		return err
	}

	switch state {
	case stateStarted:
		active, err := showProperty(ctx, unit, "ActiveState")
		if err != nil {
			return err
		}
		if active == "active" {
			return nil
		}
		return runSystemctl(ctx, "start", unit)
	case stateStopped:
		active, err := showProperty(ctx, unit, "ActiveState")
		if err != nil {
			return err
		}
		if active != "active" {
			// inactive / failed / activating 之下再 stop 都是空操作。
			return nil
		}
		return runSystemctl(ctx, "stop", unit)
	case stateEnabled, stateDisabled:
		fileState, err := showProperty(ctx, unit, "UnitFileState")
		if err != nil {
			return err
		}
		if state == stateEnabled && fileState == "enabled" {
			return nil
		}
		if state == stateDisabled && fileState == "disabled" {
			return nil
		}
		return runSystemctl(ctx, strings.TrimSuffix(state, "d"), unit)
	}
	// 防御性兜底:parseServiceSpec 已拒过表外值,这里不可达。
	return fmt.Errorf("%w: unknown desired_state %q", ErrPersistent, state)
}

// showProperty 读 systemctl 的一个属性值。属性行形如 "ActiveState=active";
// 拿不到该属性即视为无法观测(fail-closed),不猜。
func showProperty(ctx context.Context, unit, property string) (string, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", fmt.Errorf("exec: service.set %q cancelled: %w", unit, ctxErr)
	}
	out, err := runCmd(ctx, "systemctl", "show", "--property="+property, unit)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("exec: service.set %q cancelled: %w", unit, ctxErr)
		}
		return "", classifySystemctlError(out, err)
	}
	prefix := property + "="
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix)), nil
		}
	}
	return "", fmt.Errorf("%w: systemctl show returned no %s for %q: %s", ErrPersistent, property, unit, firstLine(out))
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
// PERSISTENT 处理(不可重试),与 package.set 同款 fail-closed 姿态。
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
