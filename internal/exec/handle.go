package exec

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Daedalusys/daedalus-sdk/pkgquery"
)

// 回滚句柄:一次变更留下的"可还原锚点"。
//
// 句柄是**不透明串**:控制面只负责存下来、以后原样送回,不解析它。格式由
// 本包定义,解析也只有本包一处实现(见 ParseHandle),免得格式在两个包之间
// 悄悄走偏:
//
//	dnf-history:<id>            dnf 事务 id(十进制)
//	apt-log:<start-unix>:<pkg>  apt 历史日志条目的 Start-Date + 目标包
//	service-state:<b64>         apply 之前的 systemd 单元状态(JSON + base64url)
//
// apt 为什么锚日志而不是历史 id:apt 3.2 之前没有任何 history 子命令
// (Debian 13 / apt 3.0.4 的 `apt history-list` 是 "Invalid operation"),id
// 无从取得;而日志序号会被 logrotate 平移,存下来的旧句柄可能指向别的事务。
// Start-Date 是日志里不随轮转变动的那一项。
//
// 句柄为空 = 这次变更没有可还原锚点(控制面对高危变更据此拒绝)。
const (
	handleDnfHistory = "dnf-history:"
	handleAptLog     = "apt-log:"
	handleService    = "service-state:"
)

// 句柄前缀(去掉冒号)。给解析方分派用:句柄长什么样只有本包说了算。
const (
	// HandleDnfHistory 是 dnf 事务句柄的 kind 段。
	HandleDnfHistory = "dnf-history"
	// HandleAptLog 是 apt 历史日志句柄的 kind 段。
	HandleAptLog = "apt-log"
	// HandleService 是 systemd 单元状态句柄的 kind 段。
	HandleService = "service-state"
)

// ServiceState 是回滚要用的 systemd 单元**前置**状态。
//
// 只有 ActiveState / UnitFileState / SubState:回滚就是拿它们决定反向动词
// (停 → 起 / 启用 → 禁用),别的属性与"这次变更怎么撤"无关。
type ServiceState struct {
	Unit          string `json:"unit"`
	ActiveState   string `json:"active_state"`
	UnitFileState string `json:"unit_file_state"`
	SubState      string `json:"sub_state"`
}

// encodeServiceState 把前置状态编码成完整句柄(前缀 + JSON + base64url)。
//
// base64url 而不是原文:句柄要能安全地穿过 JSON / URL / shell 参数。
func encodeServiceState(unit string, st ServiceState) string {
	st.Unit = unit
	b, err := json.Marshal(st)
	if err != nil {
		return ""
	}
	return handleService + base64.RawURLEncoding.EncodeToString(b)
}

// DecodeServiceState 是 encodeServiceState 的逆运算。
//
// 导出它是因为 internal/rollback 生成回滚计划要按前置状态算反向动词 ——
// 句柄的编码格式只在本包实现一处。
func DecodeServiceState(s string) (ServiceState, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return ServiceState{}, fmt.Errorf("exec: decode service state: %v", err)
	}
	var st ServiceState
	if err := json.Unmarshal(b, &st); err != nil {
		return ServiceState{}, fmt.Errorf("exec: decode service state: %v", err)
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
	case strings.HasPrefix(h, handleAptLog):
		return HandleAptLog, strings.TrimPrefix(h, handleAptLog), nil
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
//
// dnf4(Alma 9/10 的 4.14 / 4.20)不认 `--json`:那是 dnf5 的东西,在 dnf4 上
// 是 exit 2 的 usage。所以 JSON 走不通时退回文本界面,而不是把整条路径判死。
func captureDnfHandle(ctx context.Context, pkg string, since time.Time) (string, error) {
	out, err := runCmd(ctx, "dnf", "history", "--json")
	if err == nil {
		if entries, perr := parseDnfHistory(out); perr == nil {
			id, ok := matchDnfTransaction(entries, pkg, since)
			if !ok {
				return "", nil
			}
			return handleDnfHistory + strconv.FormatInt(id, 10), nil
		}
	}
	return captureDnfHandleText(ctx, pkg, since)
}

// dnfTextCandidates 是最多回查几条 `dnf history info`:本次 apply 的事务一定
// 在最近的几条里,往更早翻只会把"回滚错事务"的风险翻出来。
const dnfTextCandidates = 5

// captureDnfHandleText 走 dnf4 的文本界面取事务 id。
//
// `dnf history list` 只给 id 与截断的命令行,包清单要再问 `history info`;
// 任一候选解析不出时间就跳过它(不猜),全部不匹配才报错 —— 两条路径都拿不到
// 锚点时 APPLY 会把错误记下来并返回空句柄。
func captureDnfHandleText(ctx context.Context, pkg string, since time.Time) (string, error) {
	out, err := runCmd(ctx, "dnf", "history", "list")
	if err != nil {
		return "", fmt.Errorf("exec: dnf history list failed: %v", err)
	}
	ids := dnfHistoryIDs(out)
	if len(ids) == 0 {
		return "", fmt.Errorf("exec: dnf history list has no transaction id")
	}
	if len(ids) > dnfTextCandidates {
		ids = ids[:dnfTextCandidates]
	}
	for _, id := range ids {
		text, err := runCmd(ctx, "dnf", "history", "info", strconv.FormatInt(id, 10))
		if err != nil {
			continue
		}
		e, ok := parseDnfHistoryInfo(text)
		if !ok || !e.mentions(pkg) {
			continue
		}
		// 时间窗与 JSON 路径同语义:秒级时间戳留 5s 余量。
		if !e.Begin.IsZero() && !since.IsZero() && e.Begin.Unix() < since.Unix()-5 {
			continue
		}
		return handleDnfHistory + strconv.FormatInt(id, 10), nil
	}
	return "", fmt.Errorf("exec: dnf history has no transaction for package %q", pkg)
}

// dnfHistoryIDs 从 `dnf history list` 的表格里取事务 id,按大→小排好。
//
// 首列是 id,其余列是命令行/时间/动作;表头与分隔线首列不是数字,自然跳过。
func dnfHistoryIDs(out string) []int64 {
	seen := make(map[int64]struct{})
	var ids []int64
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		id, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || id < 0 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	return ids
}

// dnfTextEntry 是 `dnf history info`(文本)里回滚要用的字段。
type dnfTextEntry struct {
	Begin       time.Time
	CommandLine string
	Packages    []string
}

// dnf history info 的键与冒号之间是多列空格,值里也可能带空格:宽松匹配。
var (
	reDnfBeginTime   = regexp.MustCompile(`^\s*Begin time\s*:\s*(.+?)\s*$`)
	reDnfCommandLine = regexp.MustCompile(`^\s*Command Line\s*:\s*(.*)$`)
)

// parseDnfHistoryInfo 解析 `dnf history info <id>` 的文本输出。
//
// 第二个返回值报告 Begin time 是否解析成功:时间解析不了就不能把这条当锚点。
// Packages Altered 段里每行是 "<动作> <nevra> <仓库>",包名从 nevra 剥。
func parseDnfHistoryInfo(out string) (dnfTextEntry, bool) {
	var e dnfTextEntry
	haveBegin := false
	inPackages := false
	for _, line := range strings.Split(out, "\n") {
		if m := reDnfBeginTime.FindStringSubmatch(line); m != nil {
			// dnf 打印的是宿主本地时间;用 UTC 解析会让时间窗按秒错位。
			t, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", m[1], time.Local)
			if err != nil {
				return dnfTextEntry{}, false
			}
			e.Begin, haveBegin = t, true
			continue
		}
		if m := reDnfCommandLine.FindStringSubmatch(line); m != nil {
			e.CommandLine = strings.TrimSpace(m[1])
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "Packages Altered:" {
			inPackages = true
			continue
		}
		if !inPackages {
			continue
		}
		// 段结束:回到顶格的其他键(如 Scriptlet output)。
		if trimmed != "" && line[0] != ' ' && line[0] != '\t' {
			inPackages = false
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		if name := dnfNevraName(fields[1]); name != "" {
			e.Packages = append(e.Packages, name)
		}
	}
	return e, haveBegin
}

// mentions 报告这条事务是否动过 pkg:命令行或包清单提到都算。
func (e dnfTextEntry) mentions(pkg string) bool {
	if pkg == "" {
		return true
	}
	if e.CommandLine != "" && strings.Contains(e.CommandLine, pkg) {
		return true
	}
	for _, n := range e.Packages {
		if n == pkg {
			return true
		}
	}
	return false
}

// dnfNevraName 从 NEVRA("acl-2.4.0-1.el9_8.x86_64")里取出包名。
//
// 包名自己可以带 '-'("python3-dnf-plugins-core"),所以从右边剥:先去掉
// .arch,再去掉 release 和 version,剩下的才是名字。epoch 有两种位置
// ("1:foo-..." 与 "foo-1:..."),都要先剥掉,别让它混进包名。剥不出合法
// 名字返回空串(跳过这一行,不猜)。
func dnfNevraName(nevra string) string {
	s := strings.TrimSpace(nevra)
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[:i] // .arch
	}
	// "foo-1:1.0-1.el40":epoch 在名字与版本之间,名字已经在它之前。
	if i := strings.Index(s, ":"); i > 0 && !isDigits(s[:i]) {
		dash := strings.LastIndex(s[:i], "-")
		if dash > 0 && isDigits(s[dash+1:i]) {
			return validDnfName(s[:dash])
		}
	}
	if i := strings.Index(s, ":"); i > 0 && isDigits(s[:i]) {
		s = s[i+1:] // "1:foo-1.0-1.el9"
	}
	for range 2 {
		i := strings.LastIndex(s, "-")
		if i < 0 {
			return ""
		}
		s = s[:i]
	}
	return validDnfName(s)
}

// validDnfName 只放行包管理器认得的名字;剥错了就当这一行解析失败。
func validDnfName(s string) string {
	if s == "" || !pkgquery.IsValidPackageName(s) {
		return ""
	}
	return s
}

// isDigits 报告 s 是否全为十进制数字且非空。
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// AptLogEntry 是 /var/log/apt/history.log 里的一条事务。
//
// Packages 只收 Install/Upgrade/Remove/Reinstall 行上的包名(已剥 :arch):
// 回滚要动的是这些包,Start-Date 则是定位这条事务的不变量。
type AptLogEntry struct {
	StartDate   time.Time
	CommandLine string
	Packages    []string
}

// aptHistoryLogPath 是 apt 记录事务历史的文件。apt 3.2 之前没有任何 history
// 子命令,这个文件是唯一可用的锚点来源。
var aptHistoryLogPath = "/var/log/apt/history.log"

// readFile 是读宿主文件的注入缝(测试替换;生产即 os.ReadFile)。
var readFile = os.ReadFile

// ReadAptHistoryLog 读取并解析宿主的 apt 历史日志。
func ReadAptHistoryLog() ([]AptLogEntry, error) {
	b, err := readFile(aptHistoryLogPath)
	if err != nil {
		return nil, fmt.Errorf("exec: read %s: %w", aptHistoryLogPath, err)
	}
	return ParseAptHistoryLog(string(b)), nil
}

// aptLogActions 是日志里会动包的键;其余键(Commandline / Requested-By...)不是
// 包清单,靠它们猜包名等于删错东西。
var aptLogActions = []string{"Install:", "Upgrade:", "Remove:", "Reinstall:"}

// ParseAptHistoryLog 把 history.log 切成事务条目。
//
// 日志是纯文本块,块之间空行分隔;一个块里的动作行可能很长,apt 会换行,
// 续行以空格开头(先拼回逻辑行再切包)。以下几类一律跳过、不猜:
//   - Start-Date 解析不了的块(没有不变量就不是锚点);
//   - 认不出的动作行 / 切不出合法包名的 token。
func ParseAptHistoryLog(text string) []AptLogEntry {
	var entries []AptLogEntry
	var cur AptLogEntry
	started := false

	flush := func() {
		if started {
			entries = append(entries, cur)
		}
		cur, started = AptLogEntry{}, false
	}

	for _, line := range logicalAptLogLines(text) {
		switch {
		case strings.HasPrefix(line, "Start-Date:"):
			flush()
			t, err := ParseAptLogTime(strings.TrimPrefix(line, "Start-Date:"))
			if err != nil {
				continue // 这个块没有可用的不变量
			}
			cur.StartDate, started = t, true
		case !started:
			continue
		case strings.HasPrefix(line, "Commandline:"):
			cur.CommandLine = strings.TrimSpace(strings.TrimPrefix(line, "Commandline:"))
		case strings.HasPrefix(line, "End-Date:"):
			flush()
		default:
			for _, action := range aptLogActions {
				if strings.HasPrefix(line, action) {
					cur.Packages = append(cur.Packages, aptLogPackages(strings.TrimPrefix(line, action))...)
					break
				}
			}
		}
	}
	flush()
	return entries
}

// logicalAptLogLines 把物理行拼成逻辑行:续行以空白开头,属于上一行。
func logicalAptLogLines(text string) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && len(lines) > 0 {
			lines[len(lines)-1] += " " + strings.TrimSpace(line)
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// aptLogPackages 从动作行的值里取包名。
//
// 形状是 "pkg:arch (version), pkg2:arch (version, automatic)":逗号既分隔包,
// 也出现在括号里,所以只在括号外切。切不出合法名字的 token 跳过。
func aptLogPackages(value string) []string {
	var names []string
	depth, start := 0, 0
	appendToken := func(tok string) {
		name := aptLogPackageName(tok)
		if name != "" {
			names = append(names, name)
		}
	}
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				appendToken(value[start:i])
				start = i + 1
			}
		}
	}
	appendToken(value[start:])
	return names
}

// aptLogPackageName 从 "hello:amd64 (2.10-5)" 里取出 hello。
func aptLogPackageName(token string) string {
	token = strings.TrimSpace(token)
	if i := strings.IndexAny(token, ": \t("); i >= 0 {
		token = token[:i]
	}
	if token == "" || !pkgquery.IsValidPackageName(token) {
		return ""
	}
	return token
}

// ParseAptLogTime 解析 apt 的历史时间("2026-11-06  10:28:29")。
//
// 日期与时间之间是两个空格,history.log 与 `apt history-info` 的 "Start time"
// 都用这个格式;日志写的是宿主本地时间,故按 Local 解析(否则时间窗会整体错位)。
func ParseAptLogTime(v string) (time.Time, error) {
	fields := strings.Fields(v)
	if len(fields) != 2 {
		return time.Time{}, fmt.Errorf("exec: cannot parse apt time %q", strings.TrimSpace(v))
	}
	return time.ParseInLocation("2006-01-02 15:04:05", fields[0]+" "+fields[1], time.Local)
}

// captureAptHandle 在 apt 历史日志里找本次 apply 留下的锚点。
//
// 取日志里**最后一条**同时满足"Start-Date 在本次 apply 之后(留 5s 取整
// 余量)"与"变更集合点到目标包"的条目。日志不存在 / 找不到都返回空句柄 +
// nil:拿不到锚点不是错误,但绝不乱挑。
func captureAptHandle(ctx context.Context, pkg string, since time.Time) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	entries, err := ReadAptHistoryLog()
	if err != nil {
		return "", nil
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if !aptEntryHasPackage(e, pkg) {
			continue
		}
		if !since.IsZero() && e.StartDate.Before(since.Add(-5*time.Second)) {
			continue
		}
		return handleAptLog + strconv.FormatInt(e.StartDate.Unix(), 10) + ":" + pkg, nil
	}
	return "", nil
}

// aptEntryHasPackage 报告这条日志条目是否动过 pkg。
func aptEntryHasPackage(e AptLogEntry, pkg string) bool {
	for _, p := range e.Packages {
		if p == pkg {
			return true
		}
	}
	return false
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
