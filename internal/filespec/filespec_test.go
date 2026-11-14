package filespec

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 旧 spec(只有 content)必须继续被认出来:判不出"是不是旧形态"就会把
// 既有调用方全部当成目录 spec 处理。
func TestParse_LegacyContent(t *testing.T) {
	spec, err := Parse(`{"content":"hello"}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !spec.IsLegacy() {
		t.Fatalf("IsLegacy = false, want true")
	}
	if spec.Content != "hello" {
		t.Errorf("Content = %q, want %q", spec.Content, "hello")
	}
	if len(spec.Entries) != 0 {
		t.Errorf("Entries = %v, want 空", spec.Entries)
	}
}

// 目录形态:两条 entry,write 与 repoint 各一。
func TestParse_Entries(t *testing.T) {
	raw := `{"entries":[` +
		`{"path":"conf.d/a.conf","op":"write","content":"A"},` +
		`{"path":"current","op":"repoint","symlink_target":"../releases/2"}` +
		`]}`
	spec, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if spec.IsLegacy() {
		t.Fatalf("IsLegacy = true, want false")
	}
	if len(spec.Entries) != 2 {
		t.Fatalf("Entries 条数 = %d, want 2", len(spec.Entries))
	}
	if spec.Entries[0].Path != "conf.d/a.conf" || spec.Entries[0].Op != OpWrite || spec.Entries[0].Content != "A" {
		t.Errorf("entry[0] = %+v, want path=conf.d/a.conf op=write content=A", spec.Entries[0])
	}
	if spec.Entries[1].Path != "current" || spec.Entries[1].Op != OpRepoint || spec.Entries[1].SymlinkTarget != "../releases/2" {
		t.Errorf("entry[1] = %+v, want path=current op=repoint symlink_target=../releases/2", spec.Entries[1])
	}
}

// 键名拼错必须拒:content 写成 contents,若被忽略就会写出一个空文件,
// 而控制面以为内容已经落盘。
func TestParse_RejectsUnknownField(t *testing.T) {
	if _, err := Parse(`{"contents":"x"}`); err == nil {
		t.Fatal("未知顶层键应被拒绝")
	}
	if _, err := Parse(`{"entries":[{"path":"a","op":"write","content":"x","contnet":"typo"}]}`); err == nil {
		t.Fatal("未知 entry 键应被拒绝")
	}
}

// 不认识的 op 不能猜:猜错会把"改软链"做成"写文件"。
func TestParse_RejectsUnknownOp(t *testing.T) {
	if _, err := Parse(`{"entries":[{"path":"a","op":"delete","content":"x"}]}`); err == nil {
		t.Fatal("未知 op 应被拒绝")
	}
}

// write 必须带 content(可以是空串);repoint 必须带 symlink_target。
// 缺了就拒绝,而不是当成空内容写下去。
func TestParse_RejectsMissingPayload(t *testing.T) {
	if _, err := Parse(`{"entries":[{"path":"a","op":"write"}]}`); err == nil {
		t.Fatal("write 缺 content 应被拒绝")
	}
	if _, err := Parse(`{"entries":[{"path":"a","op":"repoint"}]}`); err == nil {
		t.Fatal("repoint 缺 symlink_target 应被拒绝")
	}
}

// entries 为空数组是"什么都没点名",不是旧形态:必须报错,否则一次 intent
// 会静默什么都不做。
func TestParse_RejectsEmptyEntries(t *testing.T) {
	if _, err := Parse(`{"entries":[]}`); err == nil {
		t.Fatal("空 entries 应被拒绝")
	}
}

func TestSpec_RejectsAbsolutePath(t *testing.T) {
	target := t.TempDir()
	spec := mustParse(t, `{"entries":[{"path":"/etc/passwd","op":"write","content":"x"}]}`)
	if err := spec.Validate(target); err == nil {
		t.Fatal("绝对路径应被拒绝")
	}
}

func TestSpec_RejectsDotDot(t *testing.T) {
	target := t.TempDir()
	spec := mustParse(t, `{"entries":[{"path":"../escape.conf","op":"write","content":"x"}]}`)
	if err := spec.Validate(target); err == nil {
		t.Fatal(".. 路径应被拒绝")
	}
}

// 父目录是软链且指向 target 之外时,整批拒绝:顺着它写会把文件落到
// target 之外,快照也存不到该写的地方。
func TestSpec_RejectsSymlinkedParentEscape(t *testing.T) {
	target := t.TempDir()
	if err := os.Symlink("/etc", filepath.Join(target, "a")); err != nil {
		t.Fatal(err)
	}
	spec := mustParse(t, `{"entries":[{"path":"a/passwd","op":"write","content":"x"}]}`)
	if err := spec.Validate(target); err == nil {
		t.Fatal("软链父目录逃逸应被拒绝")
	}
}

// 父目录是软链但仍指向 target 之内:允许(这是常见的 sites-enabled 布局)。
func TestSpec_AllowsSymlinkedParentInsideTarget(t *testing.T) {
	target := t.TempDir()
	if err := os.Mkdir(filepath.Join(target, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(target, "real"), filepath.Join(target, "link")); err != nil {
		t.Fatal(err)
	}
	spec := mustParse(t, `{"entries":[{"path":"link/a.conf","op":"write","content":"x"}]}`)
	if err := spec.Validate(target); err != nil {
		t.Fatalf("target 内的软链父目录应被接受: %v", err)
	}
}

// 同一条路径既是 write 又是 repoint:含义不唯一,整批拒绝。
func TestSpec_RejectsDuplicatePathOps(t *testing.T) {
	target := t.TempDir()
	spec := mustParse(t, `{"entries":[`+
		`{"path":"a.conf","op":"write","content":"x"},`+
		`{"path":"a.conf","op":"repoint","symlink_target":"../r1"}`+
		`]}`)
	if err := spec.Validate(target); err == nil {
		t.Fatal("同路径两种 op 应被拒绝")
	}
}

// write 的最终组件是软链时拒绝:顺着它写会改到链接指向的别处。
func TestSpec_RejectsWriteToSymlink(t *testing.T) {
	target := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.conf")
	if err := os.WriteFile(elsewhere, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(target, "link.conf")); err != nil {
		t.Fatal(err)
	}
	spec := mustParse(t, `{"entries":[{"path":"link.conf","op":"write","content":"x"}]}`)
	if err := spec.Validate(target); err == nil {
		t.Fatal("write 到软链应被拒绝")
	}
}

// repoint 的最终组件是普通文件时拒绝:把文件换成软链是另一种变更。
func TestSpec_RejectsRepointToRegularFile(t *testing.T) {
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "a.conf"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := mustParse(t, `{"entries":[{"path":"a.conf","op":"repoint","symlink_target":"../r1"}]}`)
	if err := spec.Validate(target); err == nil {
		t.Fatal("repoint 到普通文件应被拒绝")
	}
}

// 父目录不存在时拒绝:建目录是另一件事,隐式创建会让一次"写文件"顺手改掉目录树。
func TestSpec_RequiresExistingParent(t *testing.T) {
	target := t.TempDir()
	spec := mustParse(t, `{"entries":[{"path":"nope/a.conf","op":"write","content":"x"}]}`)
	if err := spec.Validate(target); err == nil {
		t.Fatal("父目录不存在应被拒绝")
	}
}

// target 必须是已存在目录:目录形态的 target 语义就是目录根。
func TestSpec_RejectsNonDirTarget(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "app.conf")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := mustParse(t, `{"entries":[{"path":"a.conf","op":"write","content":"x"}]}`)
	if err := spec.Validate(file); err == nil {
		t.Fatal("target 是普通文件时应被拒绝")
	}
}

func TestSpec_RejectsRelativeTarget(t *testing.T) {
	spec := mustParse(t, `{"entries":[{"path":"a.conf","op":"write","content":"x"}]}`)
	if err := spec.Validate("etc"); err == nil {
		t.Fatal("相对 target 应被拒绝")
	}
}

// 一批里任何一条不合法,整批拒绝:半截快照比没有快照更危险。
func TestSpec_RejectsWholeBatchOnAnyInvalidEntry(t *testing.T) {
	target := t.TempDir()
	spec := mustParse(t, `{"entries":[`+
		`{"path":"good.conf","op":"write","content":"x"},`+
		`{"path":"/etc/passwd","op":"write","content":"x"}`+
		`]}`)
	if err := spec.Validate(target); err == nil {
		t.Fatal("整批应被拒绝")
	}
}

// 旧 spec 不走目录校验:target 是文件,校验交给单文件路径。
func TestSpec_ValidateLegacyIsNoop(t *testing.T) {
	spec := mustParse(t, `{"content":"x"}`)
	if err := spec.Validate("/nonexistent/path.conf"); err != nil {
		t.Fatalf("旧 spec 的 Validate 应为空操作: %v", err)
	}
}

// SnapshotPaths 返回写前要留副本的绝对路径。
func TestSpec_SnapshotPaths(t *testing.T) {
	target := t.TempDir()
	spec := mustParse(t, `{"entries":[`+
		`{"path":"conf.d/a.conf","op":"write","content":"x"},`+
		`{"path":"current","op":"repoint","symlink_target":"../r1"}`+
		`]}`)
	got := spec.SnapshotPaths(target)
	want := []string{
		filepath.Join(target, "conf.d/a.conf"),
		filepath.Join(target, "current"),
	}
	if len(got) != len(want) {
		t.Fatalf("SnapshotPaths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SnapshotPaths[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// 旧 spec 的快照路径就是 target 本身:与 tx 的默认行为一致。
func TestSpec_SnapshotPathsLegacyIsTarget(t *testing.T) {
	spec := mustParse(t, `{"content":"x"}`)
	got := spec.SnapshotPaths("/etc/app.conf")
	if len(got) != 1 || got[0] != "/etc/app.conf" {
		t.Errorf("SnapshotPaths = %v, want [/etc/app.conf]", got)
	}
}

// 批内两条 entry 落在同一条祖先链上时(比如一边 repoint 目录软链、另一边写
// 该目录下的文件),文件最终落到旧目录还是新目标取决于 entry 顺序 —— 同一
// 请求两种结果。必须在快照前整批拒绝,且报错要能指认是哪两条。
//
// 判定只看归一化后的 entry 相对路径,不做文件系统查询。
func TestSpec_RejectsAncestorPathAmbiguity(t *testing.T) {
	linkToRelease := func(t *testing.T, target string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(target, "releases", "1"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(target, "releases", "1"), filepath.Join(target, "current")); err != nil {
			t.Fatal(err)
		}
	}
	mkdir := func(rel string) func(t *testing.T, target string) {
		return func(t *testing.T, target string) {
			t.Helper()
			if err := os.MkdirAll(filepath.Join(target, rel), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeFile := func(t *testing.T, target string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(target, "a.conf"), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name    string
		setup   func(t *testing.T, target string)
		entries string
		wantErr bool
		// 拒绝时必须被报错指认出来的两个下标与两条原始 path。
		wantRefs  [2]string
		wantPaths [2]string
	}{
		{
			name:  "repoint 目录在前、write 其子路径在后",
			setup: linkToRelease,
			entries: `{"path":"current","op":"repoint","symlink_target":"releases/2"},` +
				`{"path":"current/x.conf","op":"write","content":"x"}`,
			wantErr:   true,
			wantRefs:  [2]string{"entries[0]", "entries[1]"},
			wantPaths: [2]string{"current", "current/x.conf"},
		},
		{
			name:  "write 子路径在前、repoint 目录在后",
			setup: linkToRelease,
			entries: `{"path":"current/x.conf","op":"write","content":"x"},` +
				`{"path":"current","op":"repoint","symlink_target":"releases/2"}`,
			wantErr:   true,
			wantRefs:  [2]string{"entries[0]", "entries[1]"},
			wantPaths: [2]string{"current/x.conf", "current"},
		},
		{
			name:  "write 普通文件、又 write 它下面的路径",
			setup: writeFile,
			entries: `{"path":"a.conf","op":"write","content":"x"},` +
				`{"path":"a.conf/sub","op":"write","content":"y"}`,
			wantErr:   true,
			wantRefs:  [2]string{"entries[0]", "entries[1]"},
			wantPaths: [2]string{"a.conf", "a.conf/sub"},
		},
		{
			// 判定用的是归一化后的路径:current/./x.conf 与 current/x.conf 是同一条。
			name:  "归一化后才是祖先关系也要拒绝",
			setup: linkToRelease,
			entries: `{"path":"current","op":"repoint","symlink_target":"releases/2"},` +
				`{"path":"current/./x.conf","op":"write","content":"x"}`,
			wantErr:   true,
			wantRefs:  [2]string{"entries[0]", "entries[1]"},
			wantPaths: [2]string{"current", "current/./x.conf"},
		},
		{
			name:  "同目录下的兄弟路径不误伤",
			setup: mkdir("conf.d"),
			entries: `{"path":"conf.d/a.conf","op":"write","content":"x"},` +
				`{"path":"conf.d/b.conf","op":"write","content":"y"}`,
		},
		{
			name:  "同目录下不同子目录不误伤",
			setup: mkdir("conf.d/sub"),
			entries: `{"path":"conf.d/a.conf","op":"write","content":"x"},` +
				`{"path":"conf.d/sub/b.conf","op":"write","content":"y"}`,
		},
		{
			name:  "repoint 目录与另一目录下的文件无关",
			setup: func(t *testing.T, target string) { linkToRelease(t, target); mkdir("other")(t, target) },
			entries: `{"path":"current","op":"repoint","symlink_target":"releases/2"},` +
				`{"path":"other/x.conf","op":"write","content":"x"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := t.TempDir()
			tc.setup(t, target)
			spec := mustParse(t, `{"entries":[`+tc.entries+`]}`)
			err := spec.Validate(target)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("应接受,却被拒绝: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("祖先路径歧义应被拒绝")
			}
			msg := err.Error()
			for _, ref := range tc.wantRefs {
				if !strings.Contains(msg, ref) {
					t.Errorf("报错未指认 %s: %s", ref, msg)
				}
			}
			// 两条原始 path 都要出现在报错里,否则用户无从对照自己的输入。
			for _, p := range tc.wantPaths {
				if !strings.Contains(msg, fmt.Sprintf("%q", p)) {
					t.Errorf("报错未包含路径 %q: %s", p, msg)
				}
			}
		})
	}
}

func mustParse(t *testing.T, raw string) Spec {
	t.Helper()
	spec, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%s): %v", raw, err)
	}
	return spec
}
