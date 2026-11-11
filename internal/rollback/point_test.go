package rollback

import (
	"testing"

	"github.com/Daedalusys/daedalus-sdk/objectmodel"
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
