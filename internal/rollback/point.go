package rollback

import "github.com/Daedalusys/daedalus-sdk/objectmodel"

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
//
// 唯一的表是 objectmodel.HasRollbackPoint,本包不复制它:多一份硬编码就会
// 在 SDK 增删 kind 时悄悄走偏。
func PointFor(kind string) Point {
	if objectmodel.HasRollbackPoint(objectmodel.Kind(kind)) {
		return PointFile
	}
	return PointNone
}
