package rollback

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// treeFixture 建一个 dataDir + snapshots 根,并把 TreeBackend 指过去。
func treeFixture(t *testing.T) (dataDir string, b *TreeBackend) {
	t.Helper()
	dataDir = t.TempDir()
	return dataDir, NewTreeBackend(dataDir)
}

func treeSnapDir(dataDir, id string) string {
	return filepath.Join(dataDir, "snapshots", id)
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// 同一份内容出现两次只存一个 object:目录快照的成本靠内容寻址压下来。
func TestTree_SnapshotDedupsObjects(t *testing.T) {
	dataDir, b := treeFixture(t)
	root := t.TempDir()
	a := filepath.Join(root, "a.conf")
	c := filepath.Join(root, "b.conf")
	writeFile(t, a, "same", 0o644)
	writeFile(t, c, "same", 0o644)

	snap := treeSnapDir(dataDir, "intent-1")
	if err := b.CreateSnapshotSet([]string{a, c}, snap); err != nil {
		t.Fatalf("CreateSnapshotSet: %v", err)
	}

	objs, err := os.ReadDir(filepath.Join(snap, "objects"))
	if err != nil {
		t.Fatalf("read objects dir: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("objects 个数 = %d, want 1(同内容只存一份): %v", len(objs), objs)
	}

	m := readManifest(t, snap)
	if len(m.Entries) != 2 {
		t.Fatalf("manifest entries = %d, want 2", len(m.Entries))
	}
	if m.Entries[0].SHA256 == "" || m.Entries[0].SHA256 != m.Entries[1].SHA256 {
		t.Errorf("同内容的两条 entry 应共享 sha256, got %q / %q", m.Entries[0].SHA256, m.Entries[1].SHA256)
	}
	if !strings.HasPrefix(m.Entries[0].Path, "/") {
		t.Errorf("manifest entry path = %q, want 绝对路径", m.Entries[0].Path)
	}
}

// /usr 下有基线:只存引用,零字节 object。存全文既浪费又与镜像内容重复。
func TestTree_UsesBaselineForUsr(t *testing.T) {
	dataDir, b := treeFixture(t)
	root := t.TempDir()
	b.baselineRoots = []string{filepath.Join(root, "usr")}
	p := filepath.Join(root, "usr", "lib", "app.conf")
	writeFile(t, p, "from-image", 0o644)

	snap := treeSnapDir(dataDir, "intent-usr")
	if err := b.CreateSnapshotSet([]string{p}, snap); err != nil {
		t.Fatalf("CreateSnapshotSet: %v", err)
	}
	m := readManifest(t, snap)
	if len(m.Entries) != 1 {
		t.Fatalf("manifest entries = %d, want 1", len(m.Entries))
	}
	if m.Entries[0].Baseline == nil {
		t.Fatalf("entry.Baseline = nil, want 基线引用")
	}
	if m.Entries[0].SHA256 != "" || m.Entries[0].Size != 0 {
		t.Errorf("基线 entry 不该带内容, got sha=%q size=%d", m.Entries[0].SHA256, m.Entries[0].Size)
	}
	objs, err := os.ReadDir(filepath.Join(snap, "objects"))
	if err == nil && len(objs) != 0 {
		t.Errorf("基线路径不该写 object, got %v", objs)
	}
}

// manifest 是落盘契约:重读必须得到同一条目信息(write 内容与 symlink 目标)。
func TestTree_ManifestRoundTrip(t *testing.T) {
	dataDir, b := treeFixture(t)
	root := t.TempDir()
	f := filepath.Join(root, "app.conf")
	writeFile(t, f, "v1", 0o640)
	link := filepath.Join(root, "current")
	if err := os.Symlink("../releases/1", link); err != nil {
		t.Fatal(err)
	}

	snap := treeSnapDir(dataDir, "intent-rt")
	if err := b.CreateSnapshotSet([]string{f, link}, snap); err != nil {
		t.Fatalf("CreateSnapshotSet: %v", err)
	}

	// 原始字节也读一遍,确认是 JSON 且字段名按 spec 落盘。
	raw, err := os.ReadFile(filepath.Join(snap, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"version"`, `"intent_id"`, `"created_at"`, `"target"`, `"entries"`, `"post"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("manifest.json 缺字段 %s: %s", key, raw)
		}
	}

	m := readManifest(t, snap)
	if m.IntentID != "intent-rt" {
		t.Errorf("intent_id = %q, want intent-rt", m.IntentID)
	}
	byPath := map[string]treeManifestEntry{}
	for _, e := range m.Entries {
		byPath[e.Path] = e
	}
	fe, ok := byPath[f]
	if !ok {
		t.Fatalf("manifest 缺 %q", f)
	}
	if fe.Type != treeTypeFile || fe.Mode != "0640" || fe.Size != 2 {
		t.Errorf("file entry = %+v, want type=file mode=0640 size=2", fe)
	}
	le, ok := byPath[link]
	if !ok {
		t.Fatalf("manifest 缺 symlink %q", link)
	}
	if le.Type != treeTypeSymlink || le.SymlinkTarget != "../releases/1" {
		t.Errorf("symlink entry = %+v, want type=symlink target=../releases/1", le)
	}
}

// 还原要逐字:内容、mode、uid/gid、软链目标字符串都回原样。
func TestTree_RestoreExact(t *testing.T) {
	dataDir, b := treeFixture(t)
	root := t.TempDir()
	f := filepath.Join(root, "app.conf")
	writeFile(t, f, "original", 0o640)
	if err := os.Chmod(f, 0o640); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(f)
	if err != nil {
		t.Fatal(err)
	}
	wantUID, wantGID, ok := ownerOf(fi)
	if !ok {
		t.Fatal("无法读取属主")
	}

	link := filepath.Join(root, "current")
	if err := os.Symlink("../releases/1", link); err != nil {
		t.Fatal(err)
	}

	snap := treeSnapDir(dataDir, "intent-exact")
	if err := b.CreateSnapshotSet([]string{f, link}, snap); err != nil {
		t.Fatalf("CreateSnapshotSet: %v", err)
	}

	// 模拟 apply 把两处都改坏。
	writeFile(t, f, "broken", 0o600)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatal(err)
	}
	if err := b.RecordPostState(snap, []string{f, link}); err != nil {
		t.Fatalf("RecordPostState: %v", err)
	}

	if err := b.RestoreSet(snap, []string{f, link}); err != nil {
		t.Fatalf("RestoreSet: %v", err)
	}

	got, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Errorf("内容 = %q, want original", got)
	}
	gfi, err := os.Stat(f)
	if err != nil {
		t.Fatal(err)
	}
	if gfi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", gfi.Mode().Perm())
	}
	gotUID, gotGID, ok := ownerOf(gfi)
	if !ok {
		t.Fatal("无法读取属主")
	}
	if gotUID != wantUID || gotGID != wantGID {
		t.Errorf("owner = %d:%d, want %d:%d", gotUID, gotGID, wantUID, wantGID)
	}

	tgt, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if tgt != "../releases/1" {
		t.Errorf("symlink target = %q, want ../releases/1", tgt)
	}
}

// 别人在 apply 之后又改过:拒绝自动还原,不覆盖新改动。
func TestTree_RestoreRefusesOnConflict(t *testing.T) {
	dataDir, b := treeFixture(t)
	root := t.TempDir()
	f := filepath.Join(root, "app.conf")
	writeFile(t, f, "v1", 0o644)

	snap := treeSnapDir(dataDir, "intent-conflict")
	if err := b.CreateSnapshotSet([]string{f}, snap); err != nil {
		t.Fatalf("CreateSnapshotSet: %v", err)
	}
	writeFile(t, f, "v2-applied", 0o644)
	if err := b.RecordPostState(snap, []string{f}); err != nil {
		t.Fatalf("RecordPostState: %v", err)
	}
	// apply 之后别人又写了一次。
	writeFile(t, f, "v3-someone-else", 0o644)

	err := b.RestoreSet(snap, []string{f})
	if err == nil {
		t.Fatal("post 哈希不符时应拒绝自动还原")
	}
	if !strings.Contains(err.Error(), f) {
		t.Errorf("冲突错误应点名路径 %q, got %v", f, err)
	}
	got, _ := os.ReadFile(f)
	if string(got) != "v3-someone-else" {
		t.Errorf("冲突时不该覆盖别人的改动, got %q", got)
	}
}

// 快照时不存在、apply 时新建的文件,还原要删掉它。
func TestTree_RestoreRemovesFileCreatedAfterSnapshot(t *testing.T) {
	dataDir, b := treeFixture(t)
	root := t.TempDir()
	f := filepath.Join(root, "new.conf")

	snap := treeSnapDir(dataDir, "intent-absent")
	if err := b.CreateSnapshotSet([]string{f}, snap); err != nil {
		t.Fatalf("CreateSnapshotSet: %v", err)
	}
	writeFile(t, f, "created-by-apply", 0o644)
	if err := b.RecordPostState(snap, []string{f}); err != nil {
		t.Fatalf("RecordPostState: %v", err)
	}
	if err := b.RestoreSet(snap, []string{f}); err != nil {
		t.Fatalf("RestoreSet: %v", err)
	}
	if _, err := os.Lstat(f); !os.IsNotExist(err) {
		t.Errorf("还原后文件应不存在, stat err = %v", err)
	}
}

// 快照源是目录:拒绝。目录会把无关文件卷进还原范围。
func TestTree_SnapshotRefusesDirectory(t *testing.T) {
	dataDir, b := treeFixture(t)
	root := t.TempDir()
	dir := filepath.Join(root, "conf.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	err := b.CreateSnapshotSet([]string{dir}, treeSnapDir(dataDir, "intent-dir"))
	if err == nil {
		t.Fatal("目录快照源应被拒绝")
	}
}

// 还原时请求了一个 manifest 里没有的路径:拒绝,不猜。
func TestTree_RestoreRefusesUnknownPath(t *testing.T) {
	dataDir, b := treeFixture(t)
	root := t.TempDir()
	f := filepath.Join(root, "app.conf")
	writeFile(t, f, "v1", 0o644)
	snap := treeSnapDir(dataDir, "intent-unknown")
	if err := b.CreateSnapshotSet([]string{f}, snap); err != nil {
		t.Fatalf("CreateSnapshotSet: %v", err)
	}
	if err := b.RestoreSet(snap, []string{filepath.Join(root, "other.conf")}); err == nil {
		t.Fatal("manifest 里没有的路径应被拒绝")
	}
}

// post 没记录(进程崩溃 / 旧快照)时仍可还原,但不做冲突检测。
func TestTree_RestoreWithoutPostSkipsConflictCheck(t *testing.T) {
	dataDir, b := treeFixture(t)
	root := t.TempDir()
	f := filepath.Join(root, "app.conf")
	writeFile(t, f, "v1", 0o644)
	snap := treeSnapDir(dataDir, "intent-nopost")
	if err := b.CreateSnapshotSet([]string{f}, snap); err != nil {
		t.Fatalf("CreateSnapshotSet: %v", err)
	}
	writeFile(t, f, "v2", 0o644)
	if err := b.RestoreSet(snap, []string{f}); err != nil {
		t.Fatalf("post 缺失时应允许还原: %v", err)
	}
	got, _ := os.ReadFile(f)
	if string(got) != "v1" {
		t.Errorf("内容 = %q, want v1", got)
	}
}

// GC 不能删掉仍被保留 manifest 引用的 object;没人引用的孤儿 object 才删。
func TestTree_GCKeepsReferencedObjects(t *testing.T) {
	dataDir, b := treeFixture(t)
	root := t.TempDir()
	old := filepath.Join(root, "old.conf")
	cur := filepath.Join(root, "cur.conf")
	writeFile(t, old, "old-content", 0o644)
	writeFile(t, cur, "current-content", 0o644)

	oldSnap := treeSnapDir(dataDir, "intent-old")
	if err := b.CreateSnapshotSet([]string{old}, oldSnap); err != nil {
		t.Fatalf("CreateSnapshotSet(old): %v", err)
	}
	curSnap := treeSnapDir(dataDir, "intent-current")
	if err := b.CreateSnapshotSet([]string{cur}, curSnap); err != nil {
		t.Fatalf("CreateSnapshotSet(cur): %v", err)
	}
	// 孤儿 object:不在任何 manifest 里。
	orphanDir := filepath.Join(curSnap, "objects")
	orphan := filepath.Join(orphanDir, strings.Repeat("0", 64))
	if err := os.WriteFile(orphan, []byte("stray"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := readManifest(t, curSnap)
	refObj := filepath.Join(curSnap, "objects", m.Entries[0].SHA256)

	if err := b.GC(1, 1<<30); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if _, err := os.Stat(refObj); err != nil {
		t.Errorf("被引用的 object 被删了: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("孤儿 object 应被删除, stat err = %v", err)
	}
	if _, err := os.Stat(oldSnap); !os.IsNotExist(err) {
		t.Errorf("超出保留数的旧快照应被删除, stat err = %v", err)
	}
}

// --- helpers ---------------------------------------------------------------

func readManifest(t *testing.T, snap string) treeManifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(snap, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m treeManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	return m
}

func writeManifest(t *testing.T, snap string, m treeManifest) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snap, "manifest.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// 让测试可显式指定 created_at:GC 按时间排序,真实时钟在同毫秒内不可靠。
func setCreatedAt(t *testing.T, snap string, at time.Time) {
	t.Helper()
	m := readManifest(t, snap)
	m.CreatedAt = at.UTC().Format(time.RFC3339Nano)
	writeManifest(t, snap, m)
}
