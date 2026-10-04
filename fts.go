package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

// FTS5 全文搜索支持（网页端搜索 + 归档）。
//
// 设计取舍：
//   - external content 虚表（content='messages'）：不复制正文，只建倒排索引，省空间。
//   - trigram 分词：支持中文子串检索。注意 trigram 只能匹配 >=3 个 Unicode 字符的查询，
//     更短的关键词（如"深圳""北京"两个字）MATCH 不到，必须由搜索层降级为 LIKE（见 search.go）。
//   - 同步触发器（AFTER INSERT/UPDATE/DELETE）：让普通写入、归档、恢复、合并撤销都自动维护索引，
//     无需在每条应用层写入路径手动同步，最不容易漏、最不易与数据脱节。
//   - 建立/回填失败不阻断启动：搜索自动退回 LIKE（见 ftsMessagesEnabled / ftsArchiveEnabled）。

// FTS 可用性开关：启动迁移时按实际建立结果设置，供搜索层决定是否走 FTS 路径。
var (
	ftsMessagesEnabled atomic.Bool
	ftsArchiveEnabled  atomic.Bool
)

// ftsTable 白名单：基表名只允许这两个值，用于安全地拼接建表/触发器 DDL（标识符不能用占位符绑定）。
func ftsTableOK(base string) bool { return base == "messages" || base == "messages_archive" }

// setupFTS 为给定内容表建立 external-content FTS5 索引 + 三个同步触发器，并 rebuild 回填历史数据。
// 全部 IF NOT EXISTS，幂等；基表不存在时返回 (false, nil)（例如尚未创建归档表的库，不算错误）。
// 表名来自白名单常量，非用户输入，拼接安全。
func setupFTS(db *sql.DB, base string) (bool, error) {
	if !ftsTableOK(base) {
		return false, fmt.Errorf("setupFTS: 未知的内容表 %q", base)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, base).Scan(&n); err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil // 基表不存在，跳过
	}
	fts := base + "_fts"

	if _, err := db.Exec(fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS %s USING fts5(
		content,
		content='%s',
		content_rowid='id',
		tokenize='trigram'
	)`, fts, base)); err != nil {
		return false, err
	}

	ddls := []string{
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s_ai AFTER INSERT ON %s BEGIN
			INSERT INTO %s(rowid, content) VALUES (new.id, new.content);
		END`, fts, base, fts),
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s_ad AFTER DELETE ON %s BEGIN
			INSERT INTO %s(%s, rowid, content) VALUES('delete', old.id, old.content);
		END`, fts, base, fts, fts),
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s_au AFTER UPDATE ON %s BEGIN
			INSERT INTO %s(%s, rowid, content) VALUES('delete', old.id, old.content);
			INSERT INTO %s(rowid, content) VALUES (new.id, new.content);
		END`, fts, base, fts, fts, fts),
	}
	for _, ddl := range ddls {
		if _, err := db.Exec(ddl); err != nil {
			return false, err
		}
	}

	// rebuild：从基表一次性回填整份索引。仅在建立时执行一次（迁移版本守卫 / 手动重建接口），
	// 不会在每次启动都全表扫描。
	if _, err := db.Exec(fmt.Sprintf(`INSERT INTO %s(%s) VALUES('rebuild')`, fts, fts)); err != nil {
		return false, err
	}
	return true, nil
}

// ensureFTSFor 建立某基表的 FTS 并把可用性开关置位；失败只告警、降级，不返回错误到启动流程。
func ensureFTSFor(db *sql.DB, base string, flag *atomic.Bool) {
	ok, err := setupFTS(db, base)
	if err != nil {
		slog.Warn("FTS5 建立失败，搜索将降级为 LIKE", "table", base, "err", err)
	}
	flag.Store(ok)
}

// rebuildFTS 重建（回填）活跃表与归档表的全文索引，供管理端手动触发。返回各自是否可用。
func rebuildFTS(db *sql.DB) (messagesOK, archiveOK bool) {
	ensureFTSFor(db, "messages", &ftsMessagesEnabled)
	ensureFTSFor(db, "messages_archive", &ftsArchiveEnabled)
	return ftsMessagesEnabled.Load(), ftsArchiveEnabled.Load()
}

// ftsMinTrigramLen trigram 分词可 MATCH 的最小 Unicode 字符数
const ftsMinTrigramLen = 3

// partitionFTSKeywords 按长度把关键词分成「可走 FTS(>=3字)」与「只能 LIKE(<3字)」两组。
func partitionFTSKeywords(keywords []string) (ftsKws, likeKws []string) {
	for _, k := range keywords {
		if utf8.RuneCountInString(k) >= ftsMinTrigramLen {
			ftsKws = append(ftsKws, k)
		} else {
			likeKws = append(likeKws, k)
		}
	}
	return ftsKws, likeKws
}

// ftsPhraseMatch 把多个关键词拼成安全的 FTS5 查询串。
// 每个关键词都作为「带引号短语」，并转义内部的雙引号，避免用户输入的 FTS 语法
// （AND / OR / NEAR / 列名 / 通配符 * / 引号）改变查询语义——等价于参数绑定的效果。
func ftsPhraseMatch(keywords []string) string {
	parts := make([]string, 0, len(keywords))
	for _, k := range keywords {
		parts = append(parts, `"`+strings.ReplaceAll(k, `"`, `""`)+`"`)
	}
	return strings.Join(parts, " AND ")
}
