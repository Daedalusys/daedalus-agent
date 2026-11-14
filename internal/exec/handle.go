package exec

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 回滚句柄:一次变更留下的"可还原锚点"。
//
// 句柄是**不透明串**:控制面只负责存下来、以后原样送回,不解析它。格式由
// 本包定义,解析也只有本包一处实现(见 ParseHandle),免得格式在两个包之间
// 悄悄走偏:
//
//	dnf-history:<id>       dnf 事务 id(十进制)
//	apt-history:<id>       apt 历史 id(十进制)
//	service-state:<b64>    apply 之前的 systemd 单元状态(JSON + base64url)
//
// 句柄为空 = 这次变更没有可还原锚点(控制面对高危变更据此拒绝)。
const (
	handleDnfHistory = "dnf-history:"
	handleAptHistory = "apt-history:"
	handleService    = "service-state:"
)

// 句柄前缀(去掉冒号)。给解析方分派用:句柄长什么样只有本包说了算。
const (
	// HandleDnfHistory 是 dnf 事务句柄的 kind 段。
	HandleDnfHistory = "dnf-history"
	// HandleAptHistory 是 apt 历史句柄的 kind 段。
	HandleAptHistory = "apt-history"
	// HandleService 是 systemd 单元状态句柄的 kind 段。
	HandleService = "service-state"
)

// serviceState 是回滚要用的 systemd 单元**前置**状态。
//
// 只有 ActiveState / UnitFileState / SubState:回滚就是拿它们决定反向动词
// (停 → 起 / 启用 → 禁用),别的属性与"这次变更怎么撤"无关。
type serviceState struct {
	Unit          string `json:"unit"`
	ActiveState   string `json:"active_state"`
	UnitFileState string `json:"unit_file_state"`
	SubState      string `json:"sub_state"`
}

// encodeServiceState 把前置状态编码成完整句柄(前缀 + JSON + base64url)。
//
// base64url 而不是原文:句柄要能安全地穿过 JSON / URL / shell 参数。
func encodeServiceState(unit string, st serviceState) string {
	st.Unit = unit
	b, err := json.Marshal(st)
	if err != nil {
		return ""
	}
	return handleService + base64.RawURLEncoding.EncodeToString(b)
}

// decodeServiceState 是 encodeServiceState 的逆运算。
func decodeServiceState(s string) (serviceState, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return serviceState{}, fmt.Errorf("exec: decode service state: %v", err)
	}
	var st serviceState
	if err := json.Unmarshal(b, &st); err != nil {
		return serviceState{}, fmt.Errorf("exec: decode service state: %v", err)
	}
	return st, nil
}

// decodeHandle 拆句柄:(kind, id)。id 是前缀之后的那一段。
//
// 未知前缀一律报错(fail-closed):猜错 kind 会拿另一种回滚手段去动宿主。
func decodeHandle(h string) (kind, id string, err error) {
	switch {
	case strings.HasPrefix(h, handleDnfHistory):
		return HandleDnfHistory, strings.TrimPrefix(h, handleDnfHistory), nil
	case strings.HasPrefix(h, handleAptHistory):
		return HandleAptHistory, strings.TrimPrefix(h, handleAptHistory), nil
	case strings.HasPrefix(h, handleService):
		return HandleService, strings.TrimPrefix(h, handleService), nil
	}
	return "", "", fmt.Errorf("exec: unknown rollback handle %q", h)
}

// ParseHandle 是 decodeHandle 的对外形态:internal/rollback 生成回滚计划时
// 要按同一张前缀表分派,句柄格式只在本包实现一处。
func ParseHandle(h string) (kind, id string, err error) {
	return decodeHandle(h)
}

// handleCollector 收集一次 apply 期间抓到的句柄素材。
//
// 为什么走 ctx 而不是让执行器直接把句柄返回:HandleProvider 的签名钉死在
// "apply 之后再问一次",而 service 的回滚依据是 apply **之前**的状态 ——
// 它只能在 Apply 里抓到。ctx 是同一次调用内唯一能把这块素材递给
// RollbackHandle 的通道。
type handleCollector struct {
	mu        sync.Mutex
	startedAt time.Time
	changed   bool
	service   string
}

func newHandleCollector() *handleCollector {
	return &handleCollector{startedAt: time.Now()}
}

// markChanged 记下"本次 apply 真的动了宿主"。
func (c *handleCollector) markChanged() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.changed = true
}

// wasChanged 报告本次 apply 是否真的动过宿主。
func (c *handleCollector) wasChanged() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.changed
}

// setService 记下 service 的前置状态句柄。
func (c *handleCollector) setService(h string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.service = h
}

// serviceHandle 取出 service 的前置状态句柄。
func (c *handleCollector) serviceHandle() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.service
}

// since 是本次 apply 的开始时刻,用来给包管理器的事务划时间窗。
func (c *handleCollector) since() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.startedAt
}

// handleCtxKey 是包内私有的 ctx key。
type handleCtxKey struct{}

// withCollector 把收集器挂到 ctx 上。
func withCollector(ctx context.Context, c *handleCollector) context.Context {
	return context.WithValue(ctx, handleCtxKey{}, c)
}

// collectorFrom 取出 ctx 上的收集器;没有就返回 nil(直接调执行器时)。
func collectorFrom(ctx context.Context) *handleCollector {
	c, _ := ctx.Value(handleCtxKey{}).(*handleCollector)
	return c
}

// dnfHistoryEntry 是 dnf history --json 里的一条事务。
//
// Packages 用 RawMessage:dnf 的 packages 字段在不同版本里是字符串数组也
// 可能是对象数组,固定成一种形状会让整个历史解析直接失败。
type dnfHistoryEntry struct {
	ID             int64           `json:"id"`
	CommandLine    string          `json:"command_line"`
	Packages       json.RawMessage `json:"packages"`
	TransBeginTime int64           `json:"trans_begin_time"`
}

// captureDnfHandle 找本次 apply 对应的 dnf 事务 id。
//
// 匹配不到就返回空句柄 + nil:拿不到锚点不是错误,但绝不是"随便挑一个"
// ——回滚错的事务比没有回滚更糟。
func captureDnfHandle(ctx context.Context, pkg string, since time.Time) (string, error) {
	out, err := runCmd(ctx, "dnf", "history", "--json")
	if err != nil {
		return "", fmt.Errorf("exec: dnf history failed: %v", err)
	}
	entries, err := parseDnfHistory(out)
	if err != nil {
		return "", err
	}
	id, ok := matchDnfTransaction(entries, pkg, since)
	if !ok {
		return "", nil
	}
	return handleDnfHistory + strconv.FormatInt(id, 10), nil
}

// parseDnfHistory 解析 dnf history --json。
//
// 外层形状有两种:顶层数组,或带 history 字段的对象。两种都认。
func parseDnfHistory(out string) ([]dnfHistoryEntry, error) {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil, fmt.Errorf("exec: dnf history returned empty output")
	}
	var arr []dnfHistoryEntry
	if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
		return arr, nil
	}
	var doc struct {
		History []dnfHistoryEntry `json:"history"`
	}
	if err := json.Unmarshal([]byte(trimmed), &doc); err == nil {
		return doc.History, nil
	}
	return nil, fmt.Errorf("exec: cannot parse dnf history output")
}

// matchDnfTransaction 选出本次 apply 对应的事务:包名对得上,且不早于
// apply 开始的时间窗。取 id 最大的那条 —— 刚发生的变更在历史里排在最后。
func matchDnfTransaction(entries []dnfHistoryEntry, pkg string, since time.Time) (int64, bool) {
	var best int64
	found := false
	for _, e := range entries {
		if e.ID <= 0 {
			continue
		}
		if !entryMentionsPkg(e, pkg) {
			continue
		}
		// 时间窗:dnf 的事务时间戳是秒级,留 5s 余量给取整误差。
		if e.TransBeginTime > 0 && !since.IsZero() && e.TransBeginTime < since.Unix()-5 {
			continue
		}
		if e.ID > best {
			best, found = e.ID, true
		}
	}
	return best, found
}

// entryMentionsPkg 报告这条事务是否动过目标包。
//
// 事务里完全没有包信息时**不排除**它:信息缺失不等于对不上,再由时间窗
// 与"取最新一条"兜底;反过来把信息不全的事务一律排除,会漏掉真实的锚点。
func entryMentionsPkg(e dnfHistoryEntry, pkg string) bool {
	if pkg == "" {
		return true
	}
	if e.CommandLine != "" {
		return strings.Contains(e.CommandLine, pkg)
	}
	names := dnfEntryPackages(e.Packages)
	if len(names) == 0 {
		return true
	}
	for _, n := range names {
		if n == pkg {
			return true
		}
	}
	return false
}

// dnfEntryPackages 取出事务里的包名(字符串数组或对象数组都认)。
func dnfEntryPackages(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err == nil {
		return names
	}
	var objs []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &objs); err == nil {
		for _, o := range objs {
			names = append(names, o.Name)
		}
	}
	return names
}

// captureAptHandle 找本次 apply 对应的 apt 历史 id。
func captureAptHandle(ctx context.Context, pkg string) (string, error) {
	out, err := runCmd(ctx, "apt", "history-list")
	if err != nil {
		return "", fmt.Errorf("exec: apt history-list failed: %v", err)
	}
	id, ok := matchAptTransaction(out, pkg)
	if !ok {
		return "", nil
	}
	return handleAptHistory + strconv.FormatInt(id, 10), nil
}

// matchAptTransaction 从 apt history-list 的输出里选事务 id。
//
// 输出形状:首行是表头("ID  Command line"),其后每行首列是 id、其余是命令
// 行。取 id 最大的一条 —— 刚发生的变更排在最后。
func matchAptTransaction(out, pkg string) (int64, bool) {
	var best int64
	found := false
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		id, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue // 表头之类的非数字行
		}
		cmd := strings.Join(fields[1:], " ")
		if pkg != "" && cmd != "" && !strings.Contains(cmd, pkg) {
			continue
		}
		if id > best {
			best, found = id, true
		}
	}
	return best, found
}
