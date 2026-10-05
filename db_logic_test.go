package main

// 核心 DB 逻辑测试（此前 0% 覆盖）。重点锁住本轮发现的一处真实缺陷：
// 公共 RecomputeOtherMsgCount 之前只算 messages、漏算 messages_archive，
// 对已归档的联系人重算会把多年消息数清零（现已改为委派归档感知的 tx 版本）。
// 另验证分页/筛选的「总数与分页一致、limit 夹取、offset 归一、合并排除」，
// 以及 saveProfileHistory「只写历史、空画像回落 {}、不动当前画像」。

import (
	"database/sql"
	"testing"
)

func readOtherMsgCount(t *testing.T, db *sql.DB, id int64) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT other_msg_count FROM contacts WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("读取 other_msg_count 失败: %v", err)
	}
	return n
}

func TestRecomputeOtherMsgCountIsArchiveAware(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "归档计数")
	regressionMessages(t, db, id, "一", "二", "三") // 3 条 other

	if err := RecomputeOtherMsgCount(db, id); err != nil {
		t.Fatal(err)
	}
	if got := readOtherMsgCount(t, db, id); got != 3 {
		t.Fatalf("归档前重算应为 3，得 %d", got)
	}

	// 归档：把 2025-06-10 的老消息移入 messages_archive（活跃表清空）
	if err := ensureArchiveTables(db); err != nil {
		t.Fatal(err)
	}
	moved, err := RunArchive(db, 30)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 3 {
		t.Fatalf("应归档 3 条，实得 %d", moved)
	}
	// 关键断言：归档后重算仍须等于 3（旧的不归档感知版本会得到 0 → 计数被清零）
	if err := RecomputeOtherMsgCount(db, id); err != nil {
		t.Fatal(err)
	}
	if got := readOtherMsgCount(t, db, id); got != 3 {
		t.Fatalf("归档后重算应仍为 3（归档感知），得 %d —— 若为 0 说明计数被清零", got)
	}
}

func TestGetContactsPagePaginationAndFilterConsistency(t *testing.T) {
	db := regressionDB(t)
	ids := []int64{}
	for _, nm := range []string{"张三", "李四", "王五", "赵六", "孙七"} {
		ids = append(ids, regressionContact(t, db, nm))
	}
	// 让 张三 成为已合并联系人（merged_into 指向他人），验证默认排除
	if _, err := db.Exec(`UPDATE contacts SET merged_into = ? WHERE id = ?`, ids[1], ids[0]); err != nil {
		t.Fatal(err)
	}

	// 默认不含合并：total 应为 4（排除张三）
	list, total, err := GetContactsPage(db, false, "", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 || len(list) != 4 {
		t.Fatalf("不含合并应 4 条，得 total=%d len=%d", total, len(list))
	}
	for _, c := range list {
		if c.Name == "张三" {
			t.Fatal("已合并联系人不应出现在 includeMerged=false 结果中")
		}
	}

	// 含合并：total 应为 5
	if _, totalAll, err := GetContactsPage(db, true, "", 0, 10); err != nil || totalAll != 5 {
		t.Fatalf("含合并应 5 条，得 total=%d err=%v", totalAll, err)
	}

	// 分页：limit=2 第 1 页 2 条、总数 4；跨页不重不漏，累加=total
	seen := map[int64]bool{}
	pageTotal := 0
	for off := 0; ; off += 2 {
		page, tot, err := GetContactsPage(db, false, "", off, 2)
		if err != nil {
			t.Fatal(err)
		}
		if tot != 4 {
			t.Fatalf("total 应恒为 4，得 %d", tot)
		}
		for _, c := range page {
			if seen[c.ID] {
				t.Fatalf("分页出现重复联系人 id=%d", c.ID)
			}
			seen[c.ID] = true
		}
		pageTotal += len(page)
		if len(page) < 2 {
			break
		}
	}
	if pageTotal != 4 {
		t.Fatalf("各页累加应=total 4，得 %d", pageTotal)
	}

	// 关键词过滤：命中数=total，且都含关键字
	qList, qTotal, err := GetContactsPage(db, false, "李", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if qTotal != 1 || len(qList) != 1 || qList[0].Name != "李四" {
		t.Fatalf("按“李”应命中 1 条李四，得 total=%d %#v", qTotal, qList)
	}

	// 参数归一：limit<=0 回落默认(30)、offset<0 归零，均不报错
	if _, _, err := GetContactsPage(db, false, "", -5, 0); err != nil {
		t.Fatalf("limit<=0/offset<0 应被归一而非报错: %v", err)
	}
	// 超上限夹取到 200：不报错、返回全部（此处 5 条 < 200）
	if l, tot, err := GetContactsPage(db, true, "", 0, 99999); err != nil || tot != 5 || len(l) != 5 {
		t.Fatalf("limit 超上限应夹取而非报错，得 len=%d total=%d err=%v", len(l), tot, err)
	}
}

func TestSaveProfileHistoryWritesOnlyHistory(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "历史画像")

	// 记录当前画像，验证 saveProfileHistory 不会改动它
	var before string
	if err := db.QueryRow(`SELECT COALESCE(profile_json,'') FROM contacts WHERE id=?`, id).Scan(&before); err != nil {
		t.Fatal(err)
	}

	if err := saveProfileHistory(db, id, "", "画像生成失败：LLM 超时"); err != nil {
		t.Fatal(err)
	}

	// 空 profileJSON 应回落为 "{}"
	hist, err := GetProfileHistory(db, id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) == 0 {
		t.Fatal("应写入一条历史")
	}
	if hist[0].ProfileJSON != "{}" {
		t.Fatalf("空画像应回落为 {}，得 %q", hist[0].ProfileJSON)
	}
	if hist[0].ChangeSummary == "" {
		t.Fatal("变更说明应原样写入")
	}

	// 不应改动 contacts 当前画像
	var after string
	if err := db.QueryRow(`SELECT COALESCE(profile_json,'') FROM contacts WHERE id=?`, id).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("saveProfileHistory 不应改当前画像: before=%q after=%q", before, after)
	}
}
