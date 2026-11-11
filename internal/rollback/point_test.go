package rollback

import "testing"

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
