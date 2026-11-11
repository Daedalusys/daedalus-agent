package rollback

// Point 是一次 intent 的回滚点形态。
type Point int

const (
	// PointNone 表示这类变更没有可还原的快照点:改了宿主状态,文件级还原救不了。
	PointNone Point = iota
	// PointFile 表示回滚点是单个文件的副本。
	PointFile
)

// PointFor 按资源 kind 返回回滚点形态。表外 kind 一律 PointNone ——
// 「不认识的 kind」不能被当成「有回滚点」。
func PointFor(kind string) Point {
	if kind == "file" {
		return PointFile
	}
	return PointNone
}
