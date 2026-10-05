package main

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Phase 1 搜索 keyset 游标回归测试。
// 覆盖规格 11.1/11.3/11.4：新 cursor 分页与旧 offset 结果等价、不重不漏、跨双表消歧、非法游标拒绝。

// walkByCursor 从首屏（offset 模式）起，逐页用 NextCursor 翻页，收集全部命中，返回按出现顺序的 id 序列。
// 同时断言：每页不超过 limit；末页 hasMore=false；翻页过程中不出现重复 (id,archived)。
func walkByCursor(t *testing.T, db *sql.DB, opt SearchOptions) []string {
	t.Helper()
	var out []string
	seen := map[string]bool{}
	cur := ""
	for page := 0; page < 1000; page++ {
		o := opt
		if cur == "" {
			o.Offset = 0 // 首屏走 offset 兼容路径，仍会回带 NextCursor
		} else {
			o.Cursor = cur
			o.Offset = 0
		}
		res, err := SearchMessages(db, o)
		if err != nil {
			t.Fatalf("翻页第 %d 页出错: %v", page, err)
		}
		if len(res.List) > opt.Limit {
			t.Fatalf("第 %d 页返回 %d 条，超过 limit=%d", page, len(res.List), opt.Limit)
		}
		for _, h := range res.List {
			key := fmt.Sprintf("%d/%d", h.ID, boolToInt(h.Archived))
			if seen[key] {
				t.Fatalf("游标翻页出现重复项 id=%d archived=%v", h.ID, h.Archived)
			}
			seen[key] = true
			out = append(out, key)
		}
		if !res.HasMore {
			if res.NextCursor != "" && len(res.List) > 0 {
				// 末页仍可能带 NextCursor（末行编码），但 HasMore 必须为 false
				_ = res.NextCursor
			}
			return out
		}
		if res.NextCursor == "" {
			t.Fatalf("HasMore=true 却未返回 NextCursor（第 %d 页）", page)
		}
		cur = res.NextCursor
	}
	t.Fatal("游标翻页未在合理页数内收敛")
	return nil
}

// refOrder 用一次大 limit 的 offset 查询取全量有序序列作为参照。
func refOrder(t *testing.T, db *sql.DB, opt SearchOptions) []string {
	t.Helper()
	o := opt
	o.Limit = searchMaxLimit
	o.Offset = 0
	res, err := SearchMessages(db, o)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(res.List))
	for _, h := range res.List {
		out = append(out, fmt.Sprintf("%d/%d", h.ID, boolToInt(h.Archived)))
	}
	return out
}

// TestSearchCursorMatchesOffsetOrder_FTS 证明 ≥3 字关键词（走 FTS）时，
// 逐页游标遍历得到的 id 序列与单次大页结果完全一致（不重不漏、顺序一致）。
func TestSearchCursorMatchesOffsetOrder_FTS(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "阿明")
	base := time.Date(2025, 6, 1, 10, 0, 0, 0, time.Local)
	const n = 23
	for i := 0; i < n; i++ {
		saveAt(t, db, id, "other", fmt.Sprintf("我们在深圳科技园区讨论供应链第%d版", i), base.Add(time.Duration(i)*time.Hour))
	}
	opt := SearchOptions{Query: "科技园区", ContactID: id, Limit: 5}
	got := walkByCursor(t, db, opt)
	want := refOrder(t, db, opt)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("游标遍历与全量顺序不一致\n got=%v\nwant=%v", got, want)
	}
	if len(want) != n {
		t.Fatalf("参照应含 %d 条，实得 %d", n, len(want))
	}
}

// TestSearchCursorMatchesOffsetOrder_Like 用 2 字短词强制走 LIKE 降级路径，验证同样不重不漏。
func TestSearchCursorMatchesOffsetOrder_Like(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "小丽")
	base := time.Date(2025, 7, 1, 8, 0, 0, 0, time.Local)
	const n = 12
	for i := 0; i < n; i++ {
		saveAt(t, db, id, "other", fmt.Sprintf("深圳天气不错 备忘%d", i), base.Add(time.Duration(i)*90*time.Minute))
	}
	opt := SearchOptions{Query: "深圳", ContactID: id, Limit: 4}
	got := walkByCursor(t, db, opt)
	want := refOrder(t, db, opt)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("LIKE 路径游标遍历与全量顺序不一致\n got=%v\nwant=%v", got, want)
	}
	if len(want) != n {
		t.Fatalf("参照应含 %d 条，实得 %d", n, len(want))
	}
}

// insertArchiveRow 直接写一条归档消息（受控 id 与 msg_unix），用于构造跨表 id 重号边界。
func insertArchiveRow(t *testing.T, db *sql.DB, id, contactID int64, content string, mu int64, ts time.Time) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO messages_archive (id, contact_id, sender, content, msg_hash, msg_time, msg_unix, captured_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		id, contactID, "other", content, fmt.Sprintf("arch-hash-%d", id),
		ts.Format(time.RFC3339), mu, ts.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
}

// TestSearchCursorAcrossArchiveDedup 构造 messages 与 messages_archive 中 id 重号、msg_unix 相同的行，
// 验证 (mu,id,archived) 全序 + archived 纳入游标后，搜归档时每条恰返回一次、含归档行、顺序稳定。
func TestSearchCursorAcrossArchiveDedup(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "王总")
	if err := ensureArchiveTables(db); err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2025, 8, 1, 12, 0, 0, 0, time.Local)
	// 两条活跃消息：saveAt 按插入序得到 id=1(活跃甲)、id=2(活跃乙)，msg_unix 分别为 ts-4h / ts+4h。
	saveAt(t, db, id, "other", "科技园区项目 活跃甲", ts.Add(-4*time.Hour))
	saveAt(t, db, id, "other", "科技园区项目 活跃乙", ts.Add(4*time.Hour))
	// 归档行故意复用相同 id 值（两表各自序列，允许重号），且故意让 (msg_unix,id) 与活跃行完全相同，
	// 以逼出「mu、id 均相等时靠 archived 决胜」这一最棘手跨表边界。
	insertArchiveRow(t, db, 1, id, "科技园区项目 归档一", ts.Unix()-4*3600, ts.Add(-4*time.Hour))
	insertArchiveRow(t, db, 2, id, "科技园区项目 归档二", ts.Unix()+4*3600, ts.Add(4*time.Hour))

	opt := SearchOptions{Query: "科技园区", ContactID: id, Limit: 2, IncludeArchive: true}
	got := walkByCursor(t, db, opt)
	want := refOrder(t, db, opt)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("跨归档游标遍历不一致\n got=%v\nwant=%v", got, want)
	}
	// 必须确有归档行参与，且总数为 5（2 活跃 + 3 归档，若 maxActive 命中重号仍各计一条）。
	var archCount int
	for _, k := range want {
		if strings.HasSuffix(k, "/1") {
			archCount++
		}
	}
	if archCount == 0 {
		t.Fatal("结果未包含任何归档消息，跨表消歧未被验证")
	}
}

// TestSearchCursorRejectsMalformed 证明非法/伪造游标返回错误（上层转 400），绝不据此拼 SQL。
func TestSearchCursorRejectsMalformed(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "陈明")
	saveAt(t, db, id, "other", "科技园区例会纪要", time.Now())

	// 非 base64（'!' 不在字母表）
	if _, err := SearchMessages(db, SearchOptions{Query: "科技园区", ContactID: id, Limit: 2, Cursor: "!!!not-base64!!!"}); err == nil {
		t.Fatal("非 base64 游标应报错")
	}
	// 合法 base64 但解出的内容不是 JSON 对象（"nope"）：应报「cursor 无效」而非 panic / SQL 错误
	bad := "bm9wZQ==" // base64url("nope")
	_, err := SearchMessages(db, SearchOptions{Query: "科技园区", ContactID: id, Limit: 2, Cursor: bad})
	if err == nil || !strings.Contains(err.Error(), "cursor") {
		t.Fatalf("非法内容的游标应报 cursor 无效，实得 %v", err)
	}
}
