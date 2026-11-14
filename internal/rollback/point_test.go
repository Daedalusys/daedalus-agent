package rollback

import (
	"testing"

	"github.com/Daedalusys/daedalus-sdk/objectmodel"
	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

func TestPointFor(t *testing.T) {
	cases := []struct {
		kind string
		want Point
	}{
		{"file", PointFile},
		{"package", PointNone},
		{"service", PointNone},
		{"container", PointNone},
		{"policy", PointNone},
		{"", PointNone},
		{"totally-unknown", PointNone},
	}
	for _, c := range cases {
		if got := PointFor(c.kind); got != c.want {
			t.Errorf("PointFor(%q) = %v, want %v", c.kind, got, c.want)
		}
	}
}

// TestPointFor_DelegatesToObjectmodel 钉住"回滚点表只有一份":本包的 PointFor
// 必须与 SDK 的 objectmodel.HasRollbackPoint 逐 kind 一致,不得各说各话。
func TestPointFor_DelegatesToObjectmodel(t *testing.T) {
	for _, kind := range objectmodel.AllKinds() {
		want := PointNone
		if objectmodel.HasRollbackPoint(kind) {
			want = PointFile
		}
		if got := PointFor(string(kind)); got != want {
			t.Errorf("PointFor(%q) = %v, want %v(必须与 SDK 表一致)", kind, got, want)
		}
	}
}

// 旧 file spec(只有 content)仍是单文件回滚点:目录后端不能拿来处理它。
func TestPointForIntent_FileLegacyIsPointFile(t *testing.T) {
	got := PointForIntent(&agentv1.ApplyIntentRequest{
		Kind: "file", Target: "/etc/app.conf", SpecJson: `{"content":"x"}`,
	})
	if got != PointFile {
		t.Errorf("PointForIntent(legacy file) = %v, want PointFile", got)
	}
}

// 带 entries 的 file spec 是目录根:回滚点必须升成 PointTree。
func TestPointForIntent_FileEntriesIsPointTree(t *testing.T) {
	got := PointForIntent(&agentv1.ApplyIntentRequest{
		Kind: "file", Target: "/etc/nginx",
		SpecJson: `{"entries":[{"path":"a.conf","op":"write","content":"x"}]}`,
	})
	if got != PointTree {
		t.Errorf("PointForIntent(entries) = %v, want PointTree", got)
	}
}

// 非 file kind 的判定不因 spec 内容改变。
func TestPointForIntent_NonFileKindsUnchanged(t *testing.T) {
	cases := []struct {
		kind string
		want Point
	}{
		{"package", PointNone},
		{"service", PointNone},
		{"container", PointNone},
	}
	for _, c := range cases {
		if got := PointForIntent(&agentv1.ApplyIntentRequest{Kind: c.kind, SpecJson: `{"entries":[]}`}); got != c.want {
			t.Errorf("PointForIntent(%q) = %v, want %v", c.kind, got, c.want)
		}
	}
}

// spec 解析不了就判不出回滚点:按 None 处理,由执行器以 PERSISTENT 拒绝,
// 不能看图省事就当成"有回滚点"。
func TestPointForIntent_InvalidSpecHasNoPoint(t *testing.T) {
	if got := PointForIntent(&agentv1.ApplyIntentRequest{Kind: "file", SpecJson: `{"contents":"typo"}`}); got != PointNone {
		t.Errorf("PointForIntent(invalid spec) = %v, want PointNone", got)
	}
}
