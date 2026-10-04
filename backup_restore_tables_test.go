package main

// Bug1 回归：备份恢复必须携带 v2.4~v3.1 全部增值表，且不产生「串数据」（旧标签/跟进挂到被替换的新联系人上）。
// 表清单已从手写数组改为按 main 结构动态枚举，这三条覆盖：全量往返、防悬空、老备份缺表。

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// ensureAllValueAddedTables 建齐增值表（InitDB 迁移不含这些，测试需手动建，模拟真实启动后的库）。
func ensureAllValueAddedTables(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, fn := range []func(*sql.DB) error{ensureTagTables, ensureTimelineTables, ensureFollowupTables} {
		if err := fn(db); err != nil {
			t.Fatal(err)
		}
	}
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...interface{}) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("执行失败 %q: %v", q, err)
	}
}

func restoreCount(t *testing.T, db *sql.DB, q string, args ...interface{}) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("统计失败 %q: %v", q, err)
	}
	return n
}

// 全量往返：源库的标签/事件/跟进必须在恢复后原样出现在目标库，覆盖目标库原有增值数据。
func TestRestoreCarriesValueAddedTables(t *testing.T) {
	src := regressionDB(t)
	ensureAllValueAddedTables(t, src)
	a := regressionContact(t, src, "甲")
	mustExec(t, src, `INSERT INTO contact_tags(id,name) VALUES(1,'VIP')`)
	mustExec(t, src, `INSERT INTO contact_tag_links(contact_id,tag_id) VALUES(?,1)`, a)
	mustExec(t, src, `INSERT INTO contact_events(id,contact_id,kind,title,event_time) VALUES(1,?,'first_meet','第一次见面','2024-01-01T10:00:00Z')`, a)
	mustExec(t, src, `INSERT INTO followup_items(id,contact_id,content) VALUES(1,?,'催他还钱')`, a)

	dst := regressionDB(t)
	ensureAllValueAddedTables(t, dst)
	d := regressionContact(t, dst, "旧目标")
	mustExec(t, dst, `INSERT INTO contact_tags(id,name) VALUES(99,'OLD')`)
	mustExec(t, dst, `INSERT INTO contact_tag_links(contact_id,tag_id) VALUES(?,99)`, d)

	zipPath, cleanup, err := BuildBackupZip(src, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := RestoreBackupZip(dst, zipPath, filepath.Dir(zipPath), false); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}

	if got := restoreCount(t, dst, `SELECT COUNT(*) FROM contact_tags WHERE name='VIP'`); got != 1 {
		t.Fatalf("恢复后应带入源标签 VIP, got %d", got)
	}
	if got := restoreCount(t, dst, `SELECT COUNT(*) FROM contact_tags WHERE name='OLD'`); got != 0 {
		t.Fatalf("目标库原有标签 OLD 应被清除, got %d", got)
	}
	if got := restoreCount(t, dst, `SELECT COUNT(*) FROM contact_tag_links`); got != 1 {
		t.Fatalf("标签关联应为 1, got %d", got)
	}
	if got := restoreCount(t, dst, `SELECT COUNT(*) FROM contact_events`); got != 1 {
		t.Fatalf("事件应为 1, got %d", got)
	}
	if got := restoreCount(t, dst, `SELECT COUNT(*) FROM followup_items WHERE content='催他还钱'`); got != 1 {
		t.Fatalf("跟进应带入源数据, got %d", got)
	}
}

// 串数据专项：恢复后所有标签关联/事件/跟进的 contact_id 必须在 contacts 里真实存在，无悬空。
func TestRestoreNoCrossLinking(t *testing.T) {
	src := regressionDB(t)
	ensureAllValueAddedTables(t, src)
	a := regressionContact(t, src, "只此一人")
	mustExec(t, src, `INSERT INTO contact_tags(id,name) VALUES(1,'同事')`)
	mustExec(t, src, `INSERT INTO contact_tag_links(contact_id,tag_id) VALUES(?,1)`, a)

	dst := regressionDB(t)
	ensureAllValueAddedTables(t, dst)
	// 目标库预置一堆将被替换掉的联系人 + 挂在其上的标签关联
	d1 := regressionContact(t, dst, "张三")
	d2 := regressionContact(t, dst, "李四")
	mustExec(t, dst, `INSERT INTO contact_tags(id,name) VALUES(50,'老客户')`)
	mustExec(t, dst, `INSERT INTO contact_tag_links(contact_id,tag_id) VALUES(?,50)`, d1)
	mustExec(t, dst, `INSERT INTO contact_tag_links(contact_id,tag_id) VALUES(?,50)`, d2)

	zipPath, cleanup, err := BuildBackupZip(src, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := RestoreBackupZip(dst, zipPath, filepath.Dir(zipPath), false); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}

	// 恢复后 contacts 只剩源库那 1 人
	if got := restoreCount(t, dst, `SELECT COUNT(*) FROM contacts`); got != 1 {
		t.Fatalf("恢复后联系人应为 1, got %d", got)
	}
	// 绝不能有指向不存在联系人的标签关联/事件/跟进（这正是修复前会发生的串数据）
	if got := restoreCount(t, dst, `SELECT COUNT(*) FROM contact_tag_links l WHERE NOT EXISTS (SELECT 1 FROM contacts c WHERE c.id=l.contact_id)`); got != 0 {
		t.Fatalf("存在悬空标签关联（串数据）: %d", got)
	}
	if got := restoreCount(t, dst, `SELECT COUNT(*) FROM contact_events e WHERE NOT EXISTS (SELECT 1 FROM contacts c WHERE c.id=e.contact_id)`); got != 0 {
		t.Fatalf("存在悬空事件（串数据）: %d", got)
	}
	if got := restoreCount(t, dst, `SELECT COUNT(*) FROM followup_items f WHERE NOT EXISTS (SELECT 1 FROM contacts c WHERE c.id=f.contact_id)`); got != 0 {
		t.Fatalf("存在悬空跟进（串数据）: %d", got)
	}
}

// 老备份兼容：备份库里根本没有增值表时，恢复不得报 no such table，且目标库增值表被清空不回填（防悬空）。
func TestRestoreOldBackupWithoutValueAddedTables(t *testing.T) {
	// 源库故意只建核心表（模拟 v2.4 前），contact_tags 等不存在
	src := regressionDB(t)
	a := regressionContact(t, src, "老库里的人")
	regressionMessages(t, src, a, "你好", "在吗", "改天聊")

	dst := regressionDB(t)
	ensureAllValueAddedTables(t, dst)
	d := regressionContact(t, dst, "现在的人")
	mustExec(t, dst, `INSERT INTO contact_tags(id,name) VALUES(1,'临时')`)
	mustExec(t, dst, `INSERT INTO contact_tag_links(contact_id,tag_id) VALUES(?,1)`, d)

	zipPath, cleanup, err := BuildBackupZip(src, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	// 关键：老备份缺增值表，恢复不应因此报错
	if _, err := RestoreBackupZip(dst, zipPath, filepath.Dir(zipPath), false); err != nil {
		t.Fatalf("恢复老备份失败: %v", err)
	}
	// 目标库联系人被整表替换为源库那 1 人
	if got := restoreCount(t, dst, `SELECT COUNT(*) FROM contacts`); got != 1 {
		t.Fatalf("恢复后联系人应为 1, got %d", got)
	}
	// 增值表（目标库有、备份无）必须被清空——否则残留关联会挂到已被替换的联系人上
	if got := restoreCount(t, dst, `SELECT COUNT(*) FROM contact_tag_links`); got != 0 {
		t.Fatalf("老备份无标签数据时目标标签关联应清空, got %d", got)
	}
}

// 全局配置保护：assistant_settings / mode_presets 不按 contact_id 关联，旧备份缺表时
// 必须保留主库现状（而非先清空再回填 0 行）——否则恢复一次旧备份就把用户的
// 自动化设置/自定义预设静默重置。与上面“按联系人维度清空”互补、不矛盾。
func TestRestoreOldBackupPreservesGlobalSettings(t *testing.T) {
	// 旧备份（源库）只建核心表，根本没有 assistant_settings / mode_presets
	src := regressionDB(t)
	a := regressionContact(t, src, "老库里的人")
	regressionMessages(t, src, a, "你好")

	// 目标库已有这些全局配置表并写入可识别内容（与启动后真实一致：三张设置表都在）
	dst := regressionDB(t)
	if err := ensureAssistantTables(dst); err != nil {
		t.Fatal(err)
	}
	if err := ensureArchiveTables(dst); err != nil {
		t.Fatal(err)
	}
	if err := ensureModePresetTables(dst); err != nil {
		t.Fatal(err)
	}
	asst := defaultAssistantSettings()
	asst.SilenceDays = 88
	if err := saveAssistantSettings(dst, asst); err != nil {
		t.Fatal(err)
	}
	arch := normalizeArchiveSettings(ArchiveSettings{})
	arch.RetentionDays = 456
	if err := saveArchiveSettings(dst, arch); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveModePreset(dst, "我的模式"); err != nil {
		t.Fatal(err)
	}

	zipPath, cleanup, err := BuildBackupZip(src, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := RestoreBackupZip(dst, zipPath, filepath.Dir(zipPath), false); err != nil {
		t.Fatalf("恢复老备份失败: %v", err)
	}

	// 会话数据照旧整表替换为源库的 1 人
	if got := restoreCount(t, dst, `SELECT COUNT(*) FROM contacts`); got != 1 {
		t.Fatalf("恢复后联系人应为 1, got %d", got)
	}
	// 全局自动化设置必须保留（旧备份缺该表，不能清空）
	if got := restoreCount(t, dst, `SELECT COUNT(*) FROM assistant_settings`); got != 1 {
		t.Fatalf("旧备份无 assistant_settings 时主库设置应保留, got %d", got)
	}
	if after, err := loadAssistantSettings(dst); err != nil || after.SilenceDays != 88 {
		t.Fatalf("助手设置被误重置: %+v err=%v", after, err)
	}
	// 自定义预设也必须保留
	if got := restoreCount(t, dst, `SELECT COUNT(*) FROM mode_presets WHERE name='我的模式'`); got != 1 {
		t.Fatalf("旧备份无 mode_presets 时自定义预设应保留, got %d", got)
	}
	// 归档设置表同样保留（旧备份缺该表时不清空），内容不变
	if got := restoreCount(t, dst, `SELECT COUNT(*) FROM archive_settings`); got != 1 {
		t.Fatalf("旧备份无 archive_settings 时主库归档设置应保留, got %d", got)
	}
	if after, err := loadArchiveSettings(dst); err != nil || after.RetentionDays != 456 {
		t.Fatalf("归档设置被误重置: %+v err=%v", after, err)
	}
}
