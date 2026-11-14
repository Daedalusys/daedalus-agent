// Package filespec 是 `file` intent spec_json 的单一事实源:解析、路径校验、
// 以及"写前该给哪些路径留副本"。
//
// exec 照着它落地写入,rpc 照着它决定快照源;两处各写一遍校验迟早会走偏,
// 所以形状与规则只住在这里。
package filespec

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Op 是一条 entry 的动作。第一期只做"写内容"与"改软链指向"。
type Op string

const (
	// OpWrite 写的是解析后的真实文件;软链本身不写穿。
	OpWrite Op = "write"
	// OpRepoint 改的是软链本身,目标是链接指向的字符串。
	OpRepoint Op = "repoint"
)

// Entry 是目录 spec 里的一条子路径变更。
type Entry struct {
	Path          string
	Op            Op
	Content       string
	SymlinkTarget string
}

// Spec 是解析后的 file spec。两种形态互斥:
//
//   - 旧形态:Content 有值、legacy 为真,按单文件处理;
//   - 目录形态:Entries 列出点名的子路径,target 是目录根。
//
// legacy 用未导出字段而不是"Entries 为空"来判断:显式的空 entries 是错误,
// 不能与旧形态混为一谈,否则一次 intent 会静默什么都不做。
type Spec struct {
	Content string
	Entries []Entry

	legacy bool
}

// entryJSON 与 wire 形状一一对应。Content / SymlinkTarget 用指针是为了
// 分清"没给"和"给了空串":write 写空文件是合法的,漏写 content 不是。
type entryJSON struct {
	Path          string  `json:"path"`
	Op            Op      `json:"op"`
	Content       *string `json:"content"`
	SymlinkTarget *string `json:"symlink_target"`
}

// specJSON 是顶层 wire 形状。两个分支按"哪个字段出现"来分,而不是按
// 值是否为空。
type specJSON struct {
	Content *string      `json:"content"`
	Entries *[]entryJSON `json:"entries"`
}

// Parse 解析 spec_json。兼容旧 {"content"} 形态;entries 字段出现时以它为
// 准(旧调用方不会带这个字段)。
func Parse(raw string) (Spec, error) {
	var wire specJSON
	dec := json.NewDecoder(strings.NewReader(raw))
	// 拼错键名若被静默忽略,一次"写文件"会写成空文件而控制面以为成功。
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return Spec{}, fmt.Errorf("filespec: invalid spec_json: %w", err)
	}

	if wire.Entries != nil {
		if len(*wire.Entries) == 0 {
			return Spec{}, errors.New("filespec: entries is empty; nothing to apply")
		}
		entries := make([]Entry, 0, len(*wire.Entries))
		for i, raw := range *wire.Entries {
			entry, err := raw.toEntry()
			if err != nil {
				return Spec{}, fmt.Errorf("filespec: entries[%d]: %w", i, err)
			}
			entries = append(entries, entry)
		}
		return Spec{Entries: entries}, nil
	}

	if wire.Content != nil {
		return Spec{Content: *wire.Content, legacy: true}, nil
	}
	return Spec{}, errors.New("filespec: spec_json has neither content nor entries")
}

// toEntry 把 wire 形状校验成内部 Entry。op 的载荷要求在这里钉死:漏了
// 就当 spec 错误,而不是替调用方补一个默认值。
func (e entryJSON) toEntry() (Entry, error) {
	if e.Path == "" {
		return Entry{}, errors.New("path is required")
	}
	switch e.Op {
	case OpWrite:
		if e.Content == nil {
			return Entry{}, errors.New("write requires content")
		}
		if e.SymlinkTarget != nil {
			return Entry{}, errors.New("write must not carry symlink_target")
		}
		return Entry{Path: e.Path, Op: OpWrite, Content: *e.Content}, nil
	case OpRepoint:
		if e.SymlinkTarget == nil || *e.SymlinkTarget == "" {
			return Entry{}, errors.New("repoint requires a non-empty symlink_target")
		}
		if e.Content != nil {
			return Entry{}, errors.New("repoint must not carry content")
		}
		return Entry{Path: e.Path, Op: OpRepoint, SymlinkTarget: *e.SymlinkTarget}, nil
	default:
		return Entry{}, fmt.Errorf("unknown op %q", e.Op)
	}
}

// IsLegacy 报告这是否是旧的单文件 spec(只有 content)。
func (s Spec) IsLegacy() bool { return s.legacy }

// Validate 在 target 上做真实路径校验,不写任何东西。旧 spec 是单文件,
// 目标类型与父目录由单文件写入路径自己判,这里不做目录假设。
//
// 整批先校验完:任何一条不合法都让整个 intent 拒绝,不留半截快照。
func (s Spec) Validate(target string) error {
	if s.IsLegacy() {
		return nil
	}
	if len(s.Entries) == 0 {
		return errors.New("filespec: spec has no entries")
	}
	if !filepath.IsAbs(target) {
		return fmt.Errorf("target %q must be an absolute path", target)
	}
	fi, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("target %q is not usable: %w", target, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("target %q is not a directory; directory spec needs a directory root", target)
	}
	targetClean := filepath.Clean(target)
	// 父目录的真实路径是边界判定的基准:target 自身可能是软链,拿未解析的
	// 字符串做前缀比较会把 target 内的合法路径误判成越界。
	targetReal, err := filepath.EvalSymlinks(targetClean)
	if err != nil {
		return fmt.Errorf("resolve target %q: %w", target, err)
	}

	seen := make(map[string]Op, len(s.Entries))
	for _, entry := range s.Entries {
		clean, err := validatePathShape(targetClean, entry.Path)
		if err != nil {
			return err
		}
		if prev, dup := seen[clean]; dup {
			return fmt.Errorf("path %q appears more than once (ops %s and %s); refusing ambiguous spec", entry.Path, prev, entry.Op)
		}
		seen[clean] = entry.Op

		full := filepath.Join(targetClean, clean)
		if err := validateParent(targetReal, full); err != nil {
			return err
		}
		if err := validateFinal(full, entry); err != nil {
			return err
		}
	}
	return nil
}

// validatePathShape 挡住形状层面的越界:绝对路径、空路径、清理后仍是
// . 或 ..、以及拼出 target 之外的路径。
func validatePathShape(targetClean, path string) (string, error) {
	if path == "" {
		return "", errors.New("filespec: entry path is empty")
	}
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("entry path %q must be relative to target", path)
	}
	clean := filepath.Clean(path)
	if clean == "." || clean == ".." {
		return "", fmt.Errorf("entry path %q does not name a subpath", path)
	}
	full := filepath.Join(targetClean, clean)
	// 前缀必须带分隔符:否则 /etc/nginx-evil 会被当成 /etc/nginx 之下。
	if !strings.HasPrefix(full, targetClean+string(os.PathSeparator)) {
		return "", fmt.Errorf("entry path %q escapes target %q", path, targetClean)
	}
	return clean, nil
}

// validateParent 要求父目录已存在,且解析软链后仍在 target 真实路径之内。
//
// 不建父目录:建目录是另一件事,隐式创建会让一次"写文件"顺手改掉目录树。
// 父目录是软链且指向 target 之外时整批拒绝 —— 顺着它写会把文件落到别处,
// 快照也存不到该写的地方。
func validateParent(targetReal, full string) error {
	parent := filepath.Dir(full)
	pfi, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("parent dir of %q is not usable: %w", full, err)
	}
	if !pfi.IsDir() {
		return fmt.Errorf("parent of %q is not a directory", full)
	}
	parentReal, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return fmt.Errorf("resolve parent of %q: %w", full, err)
	}
	if parentReal != targetReal && !strings.HasPrefix(parentReal, targetReal+string(os.PathSeparator)) {
		return fmt.Errorf("parent of %q resolves to %q, outside target %q", full, parentReal, targetReal)
	}
	return nil
}

// validateFinal 拦住会写穿/改错对象的目标类型:
//
//   - write 的最终组件必须是普通文件;软链会写到链接指向的别处,目录不是文件。
//   - repoint 的最终组件必须是软链;把普通文件换成链接是另一种变更。
func validateFinal(full string, entry Entry) error {
	fi, err := os.Lstat(full)
	if errors.Is(err, os.ErrNotExist) {
		// 不存在是合法的:write 新建文件,repoint 新建链接。
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %q: %w", full, err)
	}
	switch entry.Op {
	case OpWrite:
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("write target %q is a symlink; refusing to write through it", full)
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("write target %q is not a regular file", full)
		}
	case OpRepoint:
		if fi.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("repoint target %q is not a symlink", full)
		}
	}
	return nil
}

// SnapshotPaths 返回写前需要留副本的绝对路径。旧 spec 的快照源就是 target
// 本身,与 tx 的默认行为一致。
func (s Spec) SnapshotPaths(target string) []string {
	if s.IsLegacy() {
		return []string{target}
	}
	paths := make([]string, 0, len(s.Entries))
	for _, entry := range s.Entries {
		paths = append(paths, filepath.Join(target, entry.Path))
	}
	return paths
}
