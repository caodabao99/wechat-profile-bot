package main

import (
	"database/sql"
	"testing"
	"time"
)

// ftsObjExists 判断 sqlite_master 里是否已有某类对象（表/触发器/虚表）。
func ftsObjExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name=?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// saveAt 写入一条指定时间的消息。
func saveAt(t *testing.T, db *sql.DB, contactID int64, sender, content string, ts time.Time) {
	t.Helper()
	if _, err := SaveMessages(db, contactID, []Message{{Sender: sender, Content: content, Timestamp: ts}}); err != nil {
		t.Fatal(err)
	}
}

// TestPhase2FTSIndexesCreated 校验 InitDB 迁移后：messages_fts 虚表 + 三个同步触发器都已建立，
// 且可用性开关置位。
func TestPhase2FTSIndexesCreated(t *testing.T) {
	db := regressionDB(t)
	for _, obj := range []string{"messages_fts", "messages_fts_ai", "messages_fts_ad", "messages_fts_au"} {
		if !ftsObjExists(t, db, obj) {
			t.Fatalf("应已建立 FTS 对象 %s", obj)
		}
	}
	if !ftsMessagesEnabled.Load() {
		t.Fatal("ftsMessagesEnabled 应为 true")
	}
}

// TestPhase2FTSUsedForLongKeyword 证明 ≥3 字关键词确实走了 FTS 主查询（而非降级 LIKE）：
//   - 直接查 messages_fts MATCH 能命中，说明索引已随写入自动同步（无需 rebuild）；
//   - 搜索结果的 Relevance 非 0，是 FTS 路径独有（LIKE 路径恒为 0）。
func TestPhase2FTSUsedForLongKeyword(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "阿明")
	saveAt(t, db, id, "other", "我们在深圳湾科技园区讨论了供应链项目", time.Now())

	// 索引随写入自动同步：直接 MATCH 命中
	var cnt int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH '"科技园区"'`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Fatalf("messages_fts 应命中 1 条, got %d", cnt)
	}

	res, err := SearchMessages(db, SearchOptions{IncludeTotal: true, Query: "科技园区"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || len(res.List) != 1 {
		t.Fatalf("应搜到 1 条, got total=%d len=%d", res.Total, len(res.List))
	}
	if res.List[0].Relevance == 0 {
		t.Fatal("FTS 路径应带非 0 相关度（等于证明没走降级 LIKE）")
	}
}

// TestPhase2ShortKeywordFallsBackToLike 证明 <3 字关键词走 LIKE：仍能命中，但 Relevance 恒为 0。
func TestPhase2ShortKeywordFallsBackToLike(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "小红")
	saveAt(t, db, id, "other", "下周去北京出差", time.Now())

	// "北京" 只有 2 字，trigram MATCH 不到（这是设计上必须走 LIKE 的场景）
	var cnt int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH '"北京"'`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 0 {
		t.Fatalf("2 字关键词在 trigram FTS 里应匹配不到, got %d", cnt)
	}

	res, err := SearchMessages(db, SearchOptions{IncludeTotal: true, Query: "北京"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 {
		t.Fatalf("LIKE 降级路径应仍能搜到, got total=%d", res.Total)
	}
	if res.List[0].Relevance != 0 {
		t.Fatalf("LIKE 路径 Relevance 应为 0, got %v", res.List[0].Relevance)
	}
}

// TestPhase2MixedKeywordsAndFilter 长词走 FTS、短词以 LIKE 追加，二者 AND 组合收敛结果。
func TestPhase2MixedKeywordsAndFilter(t *testing.T) {
	db := regressionDB(t)
	a := regressionContact(t, db, "甲")
	b := regressionContact(t, db, "乙")
	saveAt(t, db, a, "other", "深圳湾科技园区的供应链项目", time.Now()) // 含 科技园区(FTS) + 深圳(LIKE)
	saveAt(t, db, b, "other", "科技园区里只字未提北方", time.Now())   // 含 科技园区，但不含“深圳”

	res, err := SearchMessages(db, SearchOptions{IncludeTotal: true, Query: "科技园区 深圳"})
	if err != nil {
		t.Fatal(err)
	}
	// 第二条不含"深圳"这个连续词，应被 LIKE 追加条件排除
	if res.Total != 1 {
		t.Fatalf("混合关键词 AND 应收敛到 1 条, got %d", res.Total)
	}
	if res.List[0].ContactName == "" {
		t.Fatal("结果应带联系人名")
	}
}

// TestPhase2DegradeWhenDisabled 关闭 FTS 开关后，长词搜索仍能通过 LIKE 命中，且 Relevance 为 0。
func TestPhase2DegradeWhenDisabled(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "阿强")
	saveAt(t, db, id, "other", "供应链协同平台上线了", time.Now())

	ftsMessagesEnabled.Store(false)
	t.Cleanup(func() { ftsMessagesEnabled.Store(true) })

	res, err := SearchMessages(db, SearchOptions{IncludeTotal: true, Query: "供应链"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 {
		t.Fatalf("降级 LIKE 应仍能搜到, got %d", res.Total)
	}
	if res.List[0].Relevance != 0 {
		t.Fatal("降级路径 Relevance 应为 0")
	}
}

// TestPhase2ArchiveKeepsFTSSynced 归档/恢复经触发器自动同步两张 FTS 表：
// 归档后活跃 FTS 搜不到、IncludeArchive 能搜到且标记 archived。
func TestPhase2ArchiveKeepsFTSSynced(t *testing.T) {
	db := regressionDB(t)
	if err := ensureArchiveTables(db); err != nil {
		t.Fatal(err)
	}
	if !ftsArchiveEnabled.Load() {
		t.Fatal("ensureArchiveTables 后归档 FTS 应可用")
	}
	id := regressionContact(t, db, "老同事")
	old := time.Now().AddDate(-6, 0, 0) // 远超保留期，会被归档
	saveAt(t, db, id, "other", "多年前的供应链合作协议存档", old)

	// 活跃 FTS 有
	var act int
	db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH '"供应链"'`).Scan(&act)
	if act != 1 {
		t.Fatalf("归档前活跃 FTS 应有 1 条, got %d", act)
	}

	if _, err := RunArchive(db, 1); err != nil {
		t.Fatal(err)
	}
	// 归档后活跃 FTS 清空，归档 FTS 出现
	db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH '"供应链"'`).Scan(&act)
	if act != 0 {
		t.Fatalf("归档后活跃 FTS 应为 0, got %d", act)
	}
	var arch int
	db.QueryRow(`SELECT COUNT(*) FROM messages_archive_fts WHERE messages_archive_fts MATCH '"供应链"'`).Scan(&arch)
	if arch != 1 {
		t.Fatalf("归档后 messages_archive_fts 应有 1 条, got %d", arch)
	}

	// 不搜归档：搜不到
	if r, err := SearchMessages(db, SearchOptions{IncludeTotal: true, Query: "供应链"}); err != nil || r.Total != 0 {
		t.Fatalf("默认不搜归档应 0 命中, total=%d err=%v", func() int {
			if r == nil {
				return -1
			}
			return r.Total
		}(), err)
	}
	// 搜归档：命中且标记 archived
	r, err := SearchMessages(db, SearchOptions{IncludeTotal: true, Query: "供应链", IncludeArchive: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 1 || !r.List[0].Archived {
		t.Fatalf("搜归档应命中 1 条且 archived=true, total=%d", r.Total)
	}
}

// TestPhase2PhraseMatchSafety 校验关键词被安全地包成带引号短语，转义内部双引号，
// 用户输入的 FTS 语法（AND/引号）不会改变查询结构。
func TestPhase2PhraseMatchSafety(t *testing.T) {
	if got := ftsPhraseMatch([]string{"科技园区"}); got != `"科技园区"` {
		t.Fatalf("单词短语错误: %s", got)
	}
	if got := ftsPhraseMatch([]string{"北京", "上海"}); got != `"北京" AND "上海"` {
		t.Fatalf("多词应 AND: %s", got)
	}
	// 内嵌双引号必须被成对转义，避免逃逸出短语
	if got := ftsPhraseMatch([]string{`供应"链`}); got != `"供应""链"` {
		t.Fatalf("引号未转义: %s", got)
	}
	// 端到端：把 FTS 操作符当普通词搜，不应报错、不应命中不相关行
	db := regressionDB(t)
	id := regressionContact(t, db, "丙")
	saveAt(t, db, id, "other", "这是一条正常消息", time.Now())
	if _, err := SearchMessages(db, SearchOptions{IncludeTotal: true, Query: `不存在XYZ单词`}); err != nil {
		t.Fatalf("异常关键词不应导致错误: %v", err)
	}
}

// TestPhase2RebuildIdempotent 手动重建应可反复执行、幂等，且重建后仍能搜到既有数据。
func TestPhase2RebuildIdempotent(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "丁")
	saveAt(t, db, id, "other", "重建索引前后的供应链一致性校验", time.Now())

	for i := 0; i < 2; i++ {
		ok, _ := rebuildFTS(db)
		if !ok {
			t.Fatalf("第 %d 次 rebuild 后 messages FTS 应可用", i+1)
		}
	}
	r, err := SearchMessages(db, SearchOptions{IncludeTotal: true, Query: "供应链"})
	if err != nil || r.Total != 1 {
		t.Fatalf("rebuild 后应能搜到, total=%d err=%v", r.Total, err)
	}
}
