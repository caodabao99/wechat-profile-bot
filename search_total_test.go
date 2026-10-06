package main

// 蓝图 §11.1 回归：搜索默认不执行全表 COUNT(*)（深分页/大结果集主开销），hasMore/nextCursor
// 改由「多取一条」推断；仅显式 includeTotal=true 才回 total。FTS 与 LIKE 降级两路径同契约。

import (
	"database/sql"
	"fmt"
	"testing"
	"time"
)

func seedSearchRows(t *testing.T, db *sql.DB, cid int64, kw string, n int, base time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		saveAt(t, db, cid, "other", fmt.Sprintf("%s 内容第%d条", kw, i), base.Add(time.Duration(i)*time.Hour))
	}
}

// TestSearchDefaultSkipsTotalOptInReturnsIt：默认 Total=0 但 HasMore 正确；opt-in 才给真实总数。
func TestSearchDefaultSkipsTotalOptInReturnsIt(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "阿明")
	base := time.Date(2025, 6, 1, 10, 0, 0, 0, time.Local)
	const n = 23 // ≥3 字关键词 → 走 FTS 路径
	seedSearchRows(t, db, cid, "科技园区", n, base)

	// 默认（IncludeTotal=false）：不算总数，Total=0，但分页信号齐全
	res, err := SearchMessages(db, SearchOptions{Query: "科技园区", ContactID: cid, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 0 {
		t.Fatalf("默认不应算总数, got Total=%d", res.Total)
	}
	if len(res.List) != 5 || !res.HasMore || res.NextCursor == "" {
		t.Fatalf("默认应有 5 条+hasMore+游标: len=%d hasMore=%v cur=%q", len(res.List), res.HasMore, res.NextCursor)
	}

	// 显式 opt-in：回真实总数，其余不变
	res2, err := SearchMessages(db, SearchOptions{Query: "科技园区", ContactID: cid, Limit: 5, IncludeTotal: true})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Total != n {
		t.Fatalf("includeTotal=true 应回总数 %d, got %d", n, res2.Total)
	}
	if len(res2.List) != 5 || res2.List[0].ID != res.List[0].ID {
		t.Fatalf("opt-in 不应改变当页内容: %+v vs %+v", res2.List[0], res.List[0])
	}
}

// TestSearchHasMoreConvergesWithoutTotal：默认无总数下，逐页游标仍恰好遍历全部、末批 HasMore=false。
func TestSearchHasMoreConvergesWithoutTotal(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "小丽")
	base := time.Date(2025, 7, 1, 8, 0, 0, 0, time.Local)
	const n = 12
	seedSearchRows(t, db, cid, "深圳", n, base) // 2 字短词 → 走 LIKE 降级路径

	seen := map[string]bool{}
	cur := ""
	pages := 0
	for {
		res, err := SearchMessages(db, SearchOptions{Query: "深圳", ContactID: cid, Limit: 4, Cursor: cur})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, h := range res.List {
			key := fmt.Sprintf("%d/%d", h.ID, boolToInt(h.Archived))
			if seen[key] {
				t.Fatalf("重复命中 id=%s（游标翻页不唯一）", key)
			}
			seen[key] = true
		}
		if !res.HasMore {
			break
		}
		if res.NextCursor == "" {
			t.Fatal("HasMore=true 却无 NextCursor")
		}
		cur = res.NextCursor
		if pages > 50 {
			t.Fatal("未在合理页数内收敛")
		}
	}
	if len(seen) != n {
		t.Fatalf("无总数默认下应恰好遍历 %d 条, got %d", n, len(seen))
	}
}

// 末批（不足一页）：无总数下 HasMore 必须为 false（旧实现靠 total 推断，现已改多取一条）。
func TestSearchLastPageHasMoreFalseWithoutTotal(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "小刚")
	base := time.Date(2025, 8, 1, 8, 0, 0, 0, time.Local)
	seedSearchRows(t, db, cid, "科技园区", 3, base)

	res, err := SearchMessages(db, SearchOptions{Query: "科技园区", ContactID: cid, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.List) != 3 || res.HasMore {
		t.Fatalf("不足一页应 HasMore=false 且 Total 未算: len=%d hasMore=%v total=%d", len(res.List), res.HasMore, res.Total)
	}
}
