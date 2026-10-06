package main

import "fmt"

// contactCleanupTables 是删除联系人时需要一并清理的非核心表（均含 contact_id 列）。
// 顺序考虑外键依赖：evidence 先于 facts，outcomes 先于 suggestions。
// 归档表 messages_archive 属核心表，也需一并清理（孤儿归档消息必须删除）。
// 注意：contact_connections（关系连线）不在本清单——它用 contact_a/contact_b 双列而非
// contact_id，由 contact.go 删除联系人时的专用 DELETE 处理；误入本清单会生成
// WHERE contact_id=? 触发「no such column」，而调用方只容忍「no such table」，会让删除回滚。
//
// 这些表由各自模块的 ensureXxxTables 在启动时创建，初始化失败时表可能根本不存在，
// 因此调用方必须容忍 "no such table" 错误——绝不能因为一个增值功能的表缺失，
// 就让「删除联系人」这个已验证的核心操作失败。
var contactCleanupTables = []string{
	"contact_tag_links",
	"contact_events",
	"followup_items",
	"messages_archive",
	"profile_fact_evidence",
	"profile_facts",
	"relationship_daily_metrics",
	"relationship_state_history",
	"relationship_state",
	"suggestion_outcomes",
	"relationship_action_suggestions",
	"assistant_emotions",
	"relationship_goals",
	"relationship_projects",
	"weekly_challenges",
	"contact_achievements",
	"contact_quality_history",
	"ai_response_cache",
}

// contactCleanupStmts 删除联系人时需要一并清理的增值功能关联表。
// 每条语句都只带一个 `?`（contact_id）。
// 通过 TableRegistry 校验表已注册，避免清理未管理的表。
func contactCleanupStmts() []string {
	stmts := make([]string, 0, len(contactCleanupTables))
	for _, name := range contactCleanupTables {
		if GetTableMeta(name) != nil {
			stmts = append(stmts, fmt.Sprintf(`DELETE FROM %s WHERE contact_id = ?`, name))
		}
	}
	return stmts
}
