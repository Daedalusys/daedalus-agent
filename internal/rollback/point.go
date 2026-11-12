package rollback

import "github.com/Daedalusys/daedalus-sdk/objectmodel"

// Point 是一次 intent 的回滚点形态。
type Point int

const (
	// PointNone 表示这类变更没有可还原的快照点:改了宿主状态,文件级还原救不了。
	PointNone Point = iota
	// PointFile 表示回滚点是单个文件的副本,由内建的 copy 后端落地。
	PointFile
	// PointVolume 表示回滚点在卷层。本期**没有 producer**:卷级还原属于
	// daedalus-atomic 侧(bootc 镜像层 + 子卷),且 LVM 的 lvconvert --merge
	// 要卸载或重启才生效,agent 承诺不了即时还原。常量先占位,方便
	// SelectBackend 把"形态 + 宿主能力"两个输入的语义钉死;等 atomic 侧
	// 接入时再接 producer。
	PointVolume
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
