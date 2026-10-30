package op

import "gorm.io/gorm"

// 好几张表的 Enabled 带 gorm `default:true`。GORM 新建时把 bool 的 false 当成"没填"：
// INSERT 省掉这一列，库里落成默认值 true，再经 RETURNING 把 true 写回结构体 ——
// 调用方明确要"新建即停用"也会悄悄变成启用。
//
// 用法：Create 之前记下哪些行要 false，Create 之后（同一事务里）用这个函数显式写回。
// 不改字段 tag：去掉 default 会让老代码里依赖"没传就是启用"的路径一起变。
func persistDisabledAfterCreate(tx *gorm.DB, table any, ids []int) error {
	if len(ids) == 0 {
		return nil
	}
	return tx.Model(table).Where("id IN ?", ids).Update("enabled", false).Error
}
