package main

// contactCleanupStmts 删除联系人时需要一并清理的增值功能关联表。
// 每条语句都只带一个 `?`（contact_id）。
//
// 这些表由各自模块的 ensureXxxTables 在启动时创建，初始化失败时表可能根本不存在，
// 因此调用方必须容忍 "no such table" 错误——绝不能因为一个增值功能的表缺失，
// 就让「删除联系人」这个已验证的核心操作失败。
func contactCleanupStmts() []string {
	return []string{
		`DELETE FROM contact_tag_links WHERE contact_id = ?`,
		`DELETE FROM contact_events WHERE contact_id = ?`,
		`DELETE FROM followup_items WHERE contact_id = ?`,
		// 归档表也要清：老消息搬进 messages_archive 后主表已经没有它了，
		// 不清就会留下一批永远挂在不存在的联系人名下的孤儿归档（恢复归档也回不来）
		`DELETE FROM messages_archive WHERE contact_id = ?`,
	}
}
