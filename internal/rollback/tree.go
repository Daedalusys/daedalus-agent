package rollback

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 快照 manifest 的磁盘格式版本。字段增删时靠它分辨旧快照能不能读。
const treeManifestVersion = 1

// entry 类型。absent 也是可还原状态:"快照时这里本来没有文件"。
const (
	treeTypeFile    = "file"
	treeTypeSymlink = "symlink"
	treeTypeAbsent  = "absent"
)

// post 状态哈希的前缀。区分"文件内容哈希"与"软链目标哈希",否则一个软链
// 指向的字符串恰好等于某文件内容时会误判成同一状态。
const (
	postFilePrefix    = "file:"
	postSymlinkPrefix = "symlink:"
	postAbsent        = "absent"
)

// defaultBaselineRoots 是有镜像基线的路径前缀。这些路径的内容来自不可变
// 镜像,agent 侧再存一份全文既浪费又不可信;快照只留引用。
var defaultBaselineRoots = []string{"/usr"}

// 默认保留策略(设计 D8):最近 20 个 intent 的快照,总量不超过 512 MiB。
// 顺序不能反:先按个数删,再按字节删,否则大快照会把保留名额挤掉。
const (
	defaultKeepSnapshotSets = 20
	defaultSnapshotMaxBytes = 512 << 20
)

// TreeBackend 是目录级快照后端:内容寻址存对象,manifest 记录每个点名路径
// 的类型与元数据,还原按 manifest 逐条写回。
//
// 与 CopyBackend 的分工:单文件 intent 仍走 copy;只有当一次 intent 点名了
// 一组子路径时才走这里,因为只有集合语义才能判断"这一批是不是都还原成功"。
type TreeBackend struct {
	dataDir string

	// baselineRoots 可被测试替换;生产前缀见 defaultBaselineRoots。
	baselineRoots []string

	// keepSets / maxBytes 是建完快照后自动 GC 的策略。留成字段而不是
	// 直接写死常量:测试要能构造"保留 1 个"的场景。
	keepSets int
	maxBytes int64
}

var _ Backend = (*TreeBackend)(nil)

// NewTreeBackend 构造目录级快照后端。dataDir 是 agent 的数据目录,GC 靠它
// 找到快照集合;快照目录本身由调用方按 intent 命名后传入。
func NewTreeBackend(dataDir string) *TreeBackend {
	return &TreeBackend{
		dataDir:       dataDir,
		baselineRoots: append([]string(nil), defaultBaselineRoots...),
		keepSets:      defaultKeepSnapshotSets,
		maxBytes:      defaultSnapshotMaxBytes,
	}
}

// baselineRef 是一次"这条路径的内容由镜像基线提供"的记录。commit 为空表示
// 宿主没有可识别的部署号:引用仍然成立(我们不存全文),但本地还原无从取回。
type baselineRef struct {
	Commit string `json:"commit"`
	Path   string `json:"path"`
}

// treeManifestEntry 是一条被点名路径的快照记录。路径是绝对路径:快照必须
// 能在不知道 target 的情况下按路径还原,少一层"相对谁"的推理。
type treeManifestEntry struct {
	Path          string            `json:"path"`
	Type          string            `json:"type"`
	Mode          string            `json:"mode,omitempty"`
	UID           int               `json:"uid"`
	GID           int               `json:"gid"`
	SHA256        string            `json:"sha256,omitempty"`
	Size          int64             `json:"size"`
	Baseline      *baselineRef      `json:"baseline"`
	SymlinkTarget string            `json:"symlink_target,omitempty"`
	MTime         string            `json:"mtime,omitempty"`
	Xattrs        map[string]string `json:"xattrs,omitempty"`
	ACL           string            `json:"acl,omitempty"`
	// Uncaptured 记录没能抓到的元数据(xattr/ACL 工具缺失)。还原时按它
	// 告警,而不是假装已经还原。
	Uncaptured []string `json:"uncaptured,omitempty"`
}

// treeManifest 是 <snapshotDir>/manifest.json 的形状。post 是 apply 成功后
// 每条路径的新状态哈希,用于还原前的冲突检测。
type treeManifest struct {
	Version   int                 `json:"version"`
	IntentID  string              `json:"intent_id"`
	CreatedAt string              `json:"created_at"`
	Target    string              `json:"target"`
	Entries   []treeManifestEntry `json:"entries"`
	Post      map[string]string   `json:"post"`
}

// CreateSnapshotSet 在写任何文件之前,把 paths 的当前状态存成一份快照集。
//
// 先逐条抓取、全部成功才落 manifest:半截快照比没有快照更危险 —— 还原时
// 会以为没抓到的那几条"原本不存在"。
func (t *TreeBackend) CreateSnapshotSet(paths []string, snapshotDir string) error {
	if t.dataDir == "" {
		return errors.New("rollback: tree backend has no data dir; refusing to write snapshots")
	}
	if len(paths) == 0 {
		return errors.New("rollback: tree snapshot set needs at least one path")
	}
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		return fmt.Errorf("rollback: create snapshot dir %q: %w", snapshotDir, err)
	}
	objectDir := filepath.Join(snapshotDir, "objects")
	if err := os.MkdirAll(objectDir, 0o700); err != nil {
		return fmt.Errorf("rollback: create object dir %q: %w", objectDir, err)
	}

	m := treeManifest{
		Version:   treeManifestVersion,
		IntentID:  filepath.Base(snapshotDir),
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Target:    commonAncestor(paths),
		Entries:   make([]treeManifestEntry, 0, len(paths)),
		Post:      map[string]string{},
	}
	for _, path := range paths {
		entry, err := t.captureEntry(path, objectDir)
		if err != nil {
			return err
		}
		m.Entries = append(m.Entries, entry)
	}
	if err := saveManifest(snapshotDir, m); err != nil {
		return err
	}
	// 新快照落好之后顺手 GC。失败只记日志:这次 intent 的快照已经可用,
	// 清理不掉旧快照不该让本次 intent 失败。
	if err := t.GC(t.keepSets, t.maxBytes); err != nil {
		log.Printf("agent: rollback GC after snapshot %q failed: %v", snapshotDir, err)
	}
	return nil
}

// captureEntry 抓一条路径的写前状态。软链只存目标字符串(不跟随);普通
// 文件在基线路径下只存引用,否则存内容对象。
func (t *TreeBackend) captureEntry(path, objectDir string) (treeManifestEntry, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return treeManifestEntry{Path: path, Type: treeTypeAbsent}, nil
	}
	if err != nil {
		return treeManifestEntry{}, fmt.Errorf("rollback: inspect snapshot source %q: %w", path, err)
	}

	uid, gid, ok := ownerOf(fi)
	if !ok {
		return treeManifestEntry{}, fmt.Errorf("rollback: cannot determine owner of %q; refusing to drop it silently", path)
	}

	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return treeManifestEntry{}, fmt.Errorf("rollback: read symlink %q: %w", path, err)
		}
		return treeManifestEntry{
			Path:          path,
			Type:          treeTypeSymlink,
			UID:           uid,
			GID:           gid,
			SymlinkTarget: target,
		}, nil
	}

	if !fi.Mode().IsRegular() {
		return treeManifestEntry{}, fmt.Errorf("rollback: snapshot source %q is not a regular file or symlink; refusing", path)
	}

	entry := treeManifestEntry{
		Path:  path,
		Type:  treeTypeFile,
		Mode:  fmt.Sprintf("%04o", fi.Mode().Perm()),
		UID:   uid,
		GID:   gid,
		MTime: fi.ModTime().UTC().Format(time.RFC3339Nano),
	}
	if t.hasBaseline(path) {
		entry.Baseline = &baselineRef{Path: path}
		return entry, nil
	}

	sum, size, err := storeObject(path, objectDir)
	if err != nil {
		return treeManifestEntry{}, fmt.Errorf("rollback: store object for %q: %w", path, err)
	}
	entry.SHA256 = sum
	entry.Size = size
	entry.Xattrs, entry.ACL, entry.Uncaptured = captureExtras(path)
	return entry, nil
}

// hasBaseline 报告路径是否落在有镜像基线的区域内。
func (t *TreeBackend) hasBaseline(path string) bool {
	clean := filepath.Clean(path)
	for _, root := range t.baselineRoots {
		root = filepath.Clean(root)
		if clean == root || strings.HasPrefix(clean, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// storeObject 按 sha256 存一份内容,同内容只存一份。先写临时文件再 rename:
// 半截对象会让之后的还原读到一个"内容对不上哈希"的文件。
func storeObject(path, objectDir string) (string, int64, error) {
	in, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(objectDir, ".object-*")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hasher), in)
	if err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", 0, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", 0, err
	}
	sum := hex.EncodeToString(hasher.Sum(nil))
	final := filepath.Join(objectDir, sum)
	if _, err := os.Stat(final); err == nil {
		// 已有同内容对象:临时文件只是多读了一遍,丢掉即可。
		os.Remove(tmpName)
		return sum, size, nil
	}
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return "", 0, err
	}
	return sum, size, nil
}

// RecordPostState 把 apply 之后每条路径的状态哈希写进 manifest.post。
//
// 这是冲突检测的唯一依据:还原时当前状态与 post 不一致,说明有别的东西
// 在 apply 之后动过这条路径,自动还原会把别人的新改动一起抹掉。
func (t *TreeBackend) RecordPostState(snapshotDir string, paths []string) error {
	m, err := loadManifest(snapshotDir)
	if err != nil {
		return err
	}
	known := make(map[string]struct{}, len(m.Entries))
	for _, e := range m.Entries {
		known[e.Path] = struct{}{}
	}
	if m.Post == nil {
		m.Post = map[string]string{}
	}
	for _, path := range paths {
		if _, ok := known[path]; !ok {
			return fmt.Errorf("rollback: post state for %q has no matching manifest entry", path)
		}
		hash, err := stateHash(path)
		if err != nil {
			return fmt.Errorf("rollback: record post state of %q: %w", path, err)
		}
		m.Post[path] = hash
	}
	return saveManifest(snapshotDir, m)
}

// RestoreSet 把一组路径还原成快照时的状态。先做完整校验(manifest 覆盖、
// post 冲突、基线不可还原),全部通过后才开始写 —— 不允许"还原了一半
// 才发现后一条还原不了"。
func (t *TreeBackend) RestoreSet(snapshotDir string, paths []string) error {
	m, err := loadManifest(snapshotDir)
	if err != nil {
		return err
	}
	byPath := make(map[string]treeManifestEntry, len(m.Entries))
	for _, e := range m.Entries {
		byPath[e.Path] = e
	}

	var plans []restorePlan
	var conflicts, baseline []string
	for _, path := range paths {
		entry, ok := byPath[path]
		if !ok {
			return fmt.Errorf("rollback: snapshot %q has no entry for path %q", snapshotDir, path)
		}
		if post, recorded := m.Post[path]; recorded {
			current, err := stateHash(path)
			if err != nil {
				return fmt.Errorf("rollback: inspect current state of %q: %w", path, err)
			}
			if current != post {
				conflicts = append(conflicts, path)
				continue
			}
		}
		if entry.Baseline != nil {
			baseline = append(baseline, path)
			continue
		}
		plans = append(plans, restorePlan{entry: entry, target: path})
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("rollback: refusing to restore %d path(s) changed after apply: %s",
			len(conflicts), strings.Join(conflicts, ", "))
	}
	if len(baseline) > 0 {
		return fmt.Errorf("rollback: %s is under a baseline root; the agent keeps no local copy and cannot restore it",
			strings.Join(baseline, ", "))
	}
	return t.executeRestore(snapshotDir, plans)
}

// CreateSnapshot 是 Backend 接口的单路径转发:manifest 里只留一条 entry。
func (t *TreeBackend) CreateSnapshot(sourcePath, snapshotPath string) error {
	return t.CreateSnapshotSet([]string{sourcePath}, snapshotPath)
}

// Restore 是 Backend 接口的单路径转发。单路径快照的 source 与 target 可能
// 不同名(接口允许),因此不按路径查 manifest,而是还原唯一那条 entry。
func (t *TreeBackend) Restore(snapshotPath, targetPath string) error {
	m, err := loadManifest(snapshotPath)
	if err != nil {
		return err
	}
	if len(m.Entries) != 1 {
		return fmt.Errorf("rollback: single-path restore needs exactly one manifest entry, got %d", len(m.Entries))
	}
	return t.executeRestore(snapshotPath, []restorePlan{{entry: m.Entries[0], target: targetPath}})
}

// restorePlan 是"把哪条记录写到哪个路径"。集合还原时两者相同;单路径转发
// 允许不同。
type restorePlan struct {
	entry  treeManifestEntry
	target string
}

// executeRestore 先软链后文件:还原顺序与写入顺序相反。软链先回来,后续
// 文件写入经过它时才是快照当时的目录视角。
func (t *TreeBackend) executeRestore(snapshotDir string, plans []restorePlan) error {
	for _, pl := range plans {
		if pl.entry.Type == treeTypeSymlink {
			if err := restoreSymlink(pl.entry, pl.target); err != nil {
				return err
			}
		}
	}
	for _, pl := range plans {
		switch pl.entry.Type {
		case treeTypeFile:
			if err := restoreFile(snapshotDir, pl.entry, pl.target); err != nil {
				return err
			}
		case treeTypeAbsent:
			if err := restoreAbsent(pl.target); err != nil {
				return err
			}
		}
	}
	return nil
}

// restoreSymlink 用临时链接 + rename 原子替换:先删后建中间会有一段
// "链接不存在"的窗口,别的进程正好读它就是另一个状态。
func restoreSymlink(entry treeManifestEntry, target string) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".symlink-*")
	if err != nil {
		return fmt.Errorf("rollback: create temp symlink in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	tmp.Close()
	// CreateTemp 建的是普通文件,rename 之前必须让位给链接。
	if err := os.Remove(tmpName); err != nil {
		return fmt.Errorf("rollback: clear temp file %q: %w", tmpName, err)
	}
	if err := os.Symlink(entry.SymlinkTarget, tmpName); err != nil {
		return fmt.Errorf("rollback: create symlink %q -> %q: %w", tmpName, entry.SymlinkTarget, err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rollback: install symlink %q: %w", target, err)
	}
	if err := os.Lchown(target, entry.UID, entry.GID); err != nil {
		return fmt.Errorf("rollback: preserve symlink owner %d:%d on %q: %w", entry.UID, entry.GID, target, err)
	}
	return nil
}

// restoreFile 写临时文件 + rename 原子替换,再补 mode/属主/xattr/ACL/mtime。
//
// 顺序不能换:chown 会清掉 setuid/setgid,先 chown 再 chmod 才能把原 mode
// 原样还回去;xattr/ACL 在 rename 之前设好,新文件一露面就是完整状态。
func restoreFile(snapshotDir string, entry treeManifestEntry, target string) error {
	object := filepath.Join(snapshotDir, "objects", entry.SHA256)
	in, err := os.Open(object)
	if err != nil {
		return fmt.Errorf("rollback: snapshot object for %q unusable: %w", target, err)
	}
	defer in.Close()

	mode, err := parseMode(entry.Mode)
	if err != nil {
		return fmt.Errorf("rollback: mode %q of %q: %w", entry.Mode, target, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(target), ".restore-*")
	if err != nil {
		return fmt.Errorf("rollback: create temp file for %q: %w", target, err)
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("rollback: write temp file for %q: %w", target, err)
	}
	if err := tmp.Chown(entry.UID, entry.GID); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("rollback: preserve owner %d:%d on %q: %w", entry.UID, entry.GID, target, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("rollback: preserve mode %v on %q: %w", mode, target, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rollback: close temp file for %q: %w", target, err)
	}
	if err := applyExtras(tmpName, entry); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rollback: install %q: %w", target, err)
	}
	if entry.MTime != "" {
		at, err := time.Parse(time.RFC3339Nano, entry.MTime)
		if err == nil {
			if err := os.Chtimes(target, at, at); err != nil {
				return fmt.Errorf("rollback: restore mtime of %q: %w", target, err)
			}
		}
	}
	return nil
}

// restoreAbsent 把"快照时不存在"落回去:apply 新建的文件要删掉。
func restoreAbsent(target string) error {
	fi, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("rollback: inspect %q for restore-to-absent: %w", target, err)
	}
	if fi.IsDir() {
		return fmt.Errorf("rollback: %q was absent at snapshot time but is now a directory; refusing to remove it", target)
	}
	if err := os.Remove(target); err != nil {
		return fmt.Errorf("rollback: restore-to-absent could not remove %q: %w", target, err)
	}
	return nil
}

// GC 按保留策略清理快照集合:
//
//  1. 先按 created_at 删到只剩 maxSets 个;
//  2. 再按总字节上限删最旧的;
//  3. 最后清掉保留集合里没被 manifest 引用的孤儿 object。
//
// 第 3 步放在最后:前两步删掉的集合已经带走了它们的对象,剩下的集合里
// 仍被引用的 sha256 一个都不能动,否则那份快照就还原不了。
func (t *TreeBackend) GC(maxSets int, maxBytes int64) error {
	if t.dataDir == "" {
		return errors.New("rollback: tree backend has no data dir; cannot run GC")
	}
	if maxSets <= 0 {
		return fmt.Errorf("rollback: GC maxSets must be positive, got %d", maxSets)
	}
	root := filepath.Join(t.dataDir, "snapshots")
	dirEntries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("rollback: scan snapshot root %q: %w", root, err)
	}

	var sets []treeSnapshotSet
	for _, de := range dirEntries {
		// 只认有 manifest 的目录:旧单文件快照(.snap)不归这里管。
		if !de.IsDir() {
			continue
		}
		dir := filepath.Join(root, de.Name())
		m, err := loadManifest(dir)
		if err != nil {
			log.Printf("agent: rollback GC skipping %q: %v", dir, err)
			continue
		}
		created, err := time.Parse(time.RFC3339Nano, m.CreatedAt)
		if err != nil {
			// 时间解析不了就按最旧处理:宁可多删,也别让一个坏 manifest
			// 永久占住保留名额。
			created = time.Time{}
		}
		sets = append(sets, treeSnapshotSet{dir: dir, created: created, size: dirSize(dir)})
	}
	sort.Slice(sets, func(i, j int) bool {
		if sets[i].created.Equal(sets[j].created) {
			return sets[i].dir < sets[j].dir
		}
		return sets[i].created.Before(sets[j].created)
	})

	var total int64
	for _, s := range sets {
		total += s.size
	}
	remaining := sets
	for len(remaining) > maxSets {
		if err := removeSet(remaining[0]); err != nil {
			return err
		}
		total -= remaining[0].size
		remaining = remaining[1:]
	}
	// 字节上限是软上限:至少留最新那份。它可能就是正在执行的这条 intent
	// 的还原点,删掉等于让它没有退路。
	for total > maxBytes && len(remaining) > 1 {
		if err := removeSet(remaining[0]); err != nil {
			return err
		}
		total -= remaining[0].size
		remaining = remaining[1:]
	}

	for _, s := range remaining {
		if err := sweepObjects(s.dir); err != nil {
			return err
		}
	}
	return nil
}

// treeSnapshotSet 是 GC 的候选单位。
type treeSnapshotSet struct {
	dir     string
	created time.Time
	size    int64
}

func removeSet(s treeSnapshotSet) error {
	if err := os.RemoveAll(s.dir); err != nil {
		return fmt.Errorf("rollback: GC remove %q: %w", s.dir, err)
	}
	return nil
}

// sweepObjects 删掉集合里没被 manifest 引用的 object。
func sweepObjects(snapshotDir string) error {
	m, err := loadManifest(snapshotDir)
	if err != nil {
		return err
	}
	referenced := make(map[string]struct{}, len(m.Entries))
	for _, e := range m.Entries {
		if e.SHA256 != "" {
			referenced[e.SHA256] = struct{}{}
		}
	}
	objects := filepath.Join(snapshotDir, "objects")
	dirEntries, err := os.ReadDir(objects)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("rollback: GC scan objects %q: %w", objects, err)
	}
	for _, de := range dirEntries {
		if de.IsDir() {
			continue
		}
		if _, ok := referenced[de.Name()]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(objects, de.Name())); err != nil {
			return fmt.Errorf("rollback: GC remove orphan object %q: %w", de.Name(), err)
		}
	}
	return nil
}

// dirSize 统计一个快照集合占用的字节数(含 manifest,口径简单可预期)。
func dirSize(dir string) int64 {
	var total int64
	filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}

// stateHash 把一条路径的当前状态压成一个可比较的字符串。冲突检测只关心
// "和 apply 之后一样吗",不关心具体内容。
func stateHash(path string) (string, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return postAbsent, nil
	}
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256([]byte(target))
		return postSymlinkPrefix + hex.EncodeToString(sum[:]), nil
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a regular file or symlink", path)
	}
	in, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer in.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, in); err != nil {
		return "", err
	}
	return postFilePrefix + hex.EncodeToString(hasher.Sum(nil)), nil
}

// commonAncestor 给 manifest.target 一个可读的"这批路径的根"。它只供人
// 审阅,还原一律按每条 entry 的绝对路径走。
func commonAncestor(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	split := func(p string) []string {
		return strings.Split(filepath.Dir(filepath.Clean(p)), string(os.PathSeparator))
	}
	common := split(paths[0])
	for _, path := range paths[1:] {
		parts := split(path)
		n := 0
		for n < len(common) && n < len(parts) && common[n] == parts[n] {
			n++
		}
		common = common[:n]
	}
	joined := strings.Join(common, string(os.PathSeparator))
	if joined == "" {
		return string(os.PathSeparator)
	}
	return joined
}

func loadManifest(snapshotDir string) (treeManifest, error) {
	raw, err := os.ReadFile(filepath.Join(snapshotDir, "manifest.json"))
	if err != nil {
		return treeManifest{}, fmt.Errorf("rollback: read manifest in %q: %w", snapshotDir, err)
	}
	var m treeManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return treeManifest{}, fmt.Errorf("rollback: parse manifest in %q: %w", snapshotDir, err)
	}
	if m.Version != treeManifestVersion {
		return treeManifest{}, fmt.Errorf("rollback: manifest in %q has version %d, want %d", snapshotDir, m.Version, treeManifestVersion)
	}
	return m, nil
}

// saveManifest 原子落盘:manifest 是这份快照唯一的索引,写坏了整份快照都
// 读不出来。
func saveManifest(snapshotDir string, m treeManifest) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("rollback: marshal manifest: %w", err)
	}
	tmp, err := os.CreateTemp(snapshotDir, ".manifest-*")
	if err != nil {
		return fmt.Errorf("rollback: create temp manifest: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("rollback: write temp manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rollback: close temp manifest: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(snapshotDir, "manifest.json")); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rollback: install manifest in %q: %w", snapshotDir, err)
	}
	return nil
}

func parseMode(s string) (os.FileMode, error) {
	if s == "" {
		return 0o644, nil
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, err
	}
	return os.FileMode(v), nil
}

// captureExtras 抓 xattr 与 ACL。工具缺失或执行失败都记成"未捕获",并由
// 调用方在还原时告警 —— 抓不到不等于没有,更不等于已还原。
func captureExtras(path string) (map[string]string, string, []string) {
	var uncaptured []string
	xattrs, ok := captureXattrs(path)
	if !ok {
		uncaptured = append(uncaptured, "xattrs")
	}
	acl, ok := captureACL(path)
	if !ok {
		uncaptured = append(uncaptured, "acl")
	}
	return xattrs, acl, uncaptured
}

// captureXattrs 用 -e base64 让二进制值也能原样往返(setfattr 认 0s 前缀)。
func captureXattrs(path string) (map[string]string, bool) {
	if _, err := exec.LookPath("getfattr"); err != nil {
		return nil, false
	}
	out, err := runCommandOutput("getfattr", "-d", "-m", "-", "-e", "base64", "--absolute-names", path)
	if err != nil {
		return nil, false
	}
	xattrs := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		xattrs[name] = value
	}
	if len(xattrs) == 0 {
		return nil, true
	}
	return xattrs, true
}

func captureACL(path string) (string, bool) {
	if _, err := exec.LookPath("getfacl"); err != nil {
		return "", false
	}
	out, err := runCommandOutput("getfacl", "-p", "-c", path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

func applyExtras(path string, entry treeManifestEntry) error {
	if len(entry.Uncaptured) > 0 {
		log.Printf("agent: rollback restore %q without %s: not captured at snapshot time",
			entry.Path, strings.Join(entry.Uncaptured, "/"))
	}
	for name, value := range entry.Xattrs {
		if err := runCommand("setfattr", "-n", name, "-v", value, path); err != nil {
			return fmt.Errorf("rollback: restore xattr %s on %q: %w", name, entry.Path, err)
		}
	}
	if entry.ACL == "" {
		return nil
	}
	aclFile, err := os.CreateTemp("", "daedalus-acl-*")
	if err != nil {
		return fmt.Errorf("rollback: stash ACL for %q: %w", entry.Path, err)
	}
	name := aclFile.Name()
	defer os.Remove(name)
	if _, err := aclFile.WriteString(entry.ACL); err != nil {
		aclFile.Close()
		return fmt.Errorf("rollback: write ACL temp for %q: %w", entry.Path, err)
	}
	if err := aclFile.Close(); err != nil {
		return fmt.Errorf("rollback: close ACL temp for %q: %w", entry.Path, err)
	}
	if err := runCommand("setfacl", "--set-file="+name, path); err != nil {
		return fmt.Errorf("rollback: restore ACL on %q: %w", entry.Path, err)
	}
	return nil
}
