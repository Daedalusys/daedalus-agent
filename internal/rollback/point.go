package rollback

import (
	"github.com/Daedalusys/daedalus-sdk/objectmodel"
	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/filespec"
)

// Point 是一次 intent 的回滚点形态。
type Point int

const (
	// PointNone 表示这类变更没有可还原的快照点:改了宿主状态,文件级还原救不了。
	PointNone Point = iota
	// PointFile 表示回滚点是单个文件的副本,由内建的 copy 后端落地。
	PointFile
	// PointTree 表示回滚点是一组点名路径的内容寻址快照,由内建的 tree
	// 后端落地。与 PointFile 的区别只在"备份的形状",不在宿主能力。
	PointTree
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

// PointForIntent 在 PointFor 之上再看 spec 形状:同一个 file kind,旧形态
// (只有 content)是单文件回滚点,entries 形态是一组路径的目录回滚点。
//
// spec 解析不了时退回 kind 给出的单文件点:看不出版本形状,就按最保守的
// 「先快照再写」处理。真要是目录 spec,单文件快照会在目录上失败,宿主仍然
// 不被改动 —— 比"判成无回滚点、裸写一把"安全。
func PointForIntent(intent *agentv1.ApplyIntentRequest) Point {
	if intent == nil {
		return PointNone
	}
	point := PointFor(intent.Kind)
	if point != PointFile {
		return point
	}
	spec, err := filespec.Parse(intent.SpecJson)
	if err != nil {
		return point
	}
	if spec.IsLegacy() {
		return PointFile
	}
	return PointTree
}
