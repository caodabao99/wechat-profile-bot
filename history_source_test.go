package main

// v6.3 §P9 历史消息统一访问层验收。
//
// 钉死四件事：
//  1. 窗口取数必须并上 messages_archive——这是正确性要求：归档会把老消息**移出** messages，
//     只查一张表的窗口统计会静默少算（不报错）。测试用「对照组」证明老写法确实会丢。
//  2. 时间口径唯一：msg_unix 缺失时回落 strftime，两类行都要落在同一个窗口里。
//  3. 并表结果必须带来源标记，且 limit 取的是「两表合起来最近的 N 条」。
//  4. 审计棘轮：散点对 messages 的直接 SQL 只允许减少、不允许增长（防回潮）。

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// insertRawMessage 直接按列写一条消息，好精确控制 msg_time / msg_unix 的组合
// （SaveMessages 会同时填两者，测不了「老数据只有 msg_time」这条回落路径）。
// msg_hash 用 content 充当——测试内同一联系人每条 content 都不同，满足 UNIQUE 约束。
func insertRawMessage(t *testing.T, db *sql.DB, table string, contactID int64, sender, content, msgTime string, msgUnix *int64) {
	t.Helper()
	if msgUnix == nil {
		if _, err := db.Exec(
			`INSERT INTO `+table+` (contact_id, sender, content, msg_hash, msg_time) VALUES (?, ?, ?, ?, ?)`,
			contactID, sender, content, content, msgTime); err != nil {
			t.Fatalf("写 %s: %v", table, err)
		}
		return
	}
	if _, err := db.Exec(
		`INSERT INTO `+table+` (contact_id, sender, content, msg_hash, msg_time, msg_unix) VALUES (?, ?, ?, ?, ?, ?)`,
		contactID, sender, content, content, msgTime, *msgUnix); err != nil {
		t.Fatalf("写 %s: %v", table, err)
	}
}

func TestHistoryMessagesIncludesArchivedRows(t *testing.T) {
	db := regressionDB(t)
	if err := ensureArchiveTables(db); err != nil {
		t.Fatalf("建归档表: %v", err)
	}
	id := regressionContact(t, db, "归档可见")

	old := time.Now().AddDate(0, 0, -200).Format("2006-01-02 15:04:05")
	recent := time.Now().Add(-time.Hour).Format("2006-01-02 15:04:05")
	insertRawMessage(t, db, "messages", id, "other", "两年前的老话", old, nil)
	insertRawMessage(t, db, "messages", id, "other", "最近的话", recent, nil)

	// 手动把老消息搬进归档表——与 RunArchive 的效果一致（从 messages 移出、进 messages_archive）
	if _, err := db.Exec(
		`INSERT INTO messages_archive (contact_id, sender, content, msg_hash, msg_time)
		 SELECT contact_id, sender, content, msg_hash, msg_time FROM messages WHERE content = '两年前的老话'`); err != nil {
		t.Fatalf("搬归档: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM messages WHERE content = '两年前的老话'`); err != nil {
		t.Fatalf("删原行: %v", err)
	}

	// 对照组：老写法只查 messages，260 天窗口里确实只剩 1 条——这就是会被静默丢掉的那段
	var naive int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE contact_id = ? AND COALESCE(msg_time,'') != ''`, id).Scan(&naive); err != nil {
		t.Fatal(err)
	}
	if naive != 1 {
		t.Fatalf("对照组应先证明归档把 messages 掏空了，实得 %d（否则本测试是空断言）", naive)
	}

	got, err := HistoryMessages(db, HistoryFilter{
		ContactID: id,
		SinceUnix: time.Now().AddDate(0, 0, -260).Unix(),
	}, 0)
	if err != nil {
		t.Fatalf("HistoryMessages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("并表后应看到 2 条（1 条已归档），实得 %d", len(got))
	}
	// 时间正序：老话在前
	if got[0].Content != "两年前的老话" || got[1].Content != "最近的话" {
		t.Fatalf("应按时间正序返回，实得 %q, %q", got[0].Content, got[1].Content)
	}
	if !got[0].Archived || got[1].Archived {
		t.Fatalf("来源标记应区分归档行，实得 archived[0]=%v archived[1]=%v", got[0].Archived, got[1].Archived)
	}

	// 计数口径同样并表
	n, err := HistoryCount(db, HistoryFilter{ContactID: id})
	if err != nil {
		t.Fatalf("HistoryCount: %v", err)
	}
	if n != 2 {
		t.Fatalf("HistoryCount 应并表数到 2，实得 %d", n)
	}
}

func TestHistoryTimeFallsBackToStrftime(t *testing.T) {
	db := regressionDB(t)
	if err := ensureArchiveTables(db); err != nil {
		t.Fatal(err)
	}
	id := regressionContact(t, db, "时间回落")

	since := time.Now().AddDate(0, 0, -30)
	// 老库风格：只有 msg_time、没有 msg_unix —— 若窗口条件只认 msg_unix，这行会被静默过滤
	// 放在窗口内（29 天前）以证明 strftime 回落确实生效
	insertRawMessage(t, db, "messages", id, "other", "只有时间文本", since.Add(24*time.Hour).Format("2006-01-02 15:04:05"), nil)
	// 新库风格：两者都有（窗口内，28 天前）
	unix := since.Add(48 * time.Hour).Unix()
	insertRawMessage(t, db, "messages", id, "other", "有 unix 也有文本", time.Unix(unix, 0).Format("2006-01-02 15:04:05"), &unix)
	// 窗口之外（40 天前，早于 since）
	out := since.Add(-10 * 24 * time.Hour)
	unixOut := out.Unix()
	insertRawMessage(t, db, "messages", id, "other", "窗口外", out.Format("2006-01-02 15:04:05"), &unixOut)
	// 无时间戳：只在做窗口聚合时必须被排除（否则落进哪个桶都不对）
	insertRawMessage(t, db, "messages", id, "other", "没有时间", "", nil)

	got, err := HistoryMessages(db, HistoryFilter{
		ContactID: id, SinceUnix: since.Unix(), WithTimeOnly: true,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var contents []string
	for _, m := range got {
		contents = append(contents, m.Content)
	}
	joined := strings.Join(contents, "|")
	if !strings.Contains(joined, "只有时间文本") || !strings.Contains(joined, "有 unix 也有文本") {
		t.Fatalf("窗口内两类时间口径都应命中，实得 %q", joined)
	}
	if strings.Contains(joined, "窗口外") || strings.Contains(joined, "没有时间") {
		t.Fatalf("窗口外与无时间戳行都不得混进来，实得 %q", joined)
	}
}

func TestHistoryLimitTakesMostRecentAcrossTables(t *testing.T) {
	db := regressionDB(t)
	if err := ensureArchiveTables(db); err != nil {
		t.Fatal(err)
	}
	id := regressionContact(t, db, "跨表裁剪")

	// 归档表放 3 条更早的，messages 放 2 条较新的
	for i := 0; i < 3; i++ {
		ts := time.Now().AddDate(0, 0, -100+i).Format("2006-01-02 15:04:05")
		insertRawMessage(t, db, "messages_archive", id, "other", "旧"+string(rune('0'+i)), ts, nil)
	}
	for i := 0; i < 2; i++ {
		ts := time.Now().AddDate(0, 0, -1+i).Format("2006-01-02 15:04:05")
		insertRawMessage(t, db, "messages", id, "other", "新"+string(rune('0'+i)), ts, nil)
	}

	got, err := HistoryMessages(db, HistoryFilter{ContactID: id}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("limit=2 应只留 2 条，实得 %d", len(got))
	}
	// 留下的一定是全局最近的 2 条（都在 messages），且按时间正序
	if got[0].Content != "新0" || got[1].Content != "新1" {
		t.Fatalf("应保留最近的 2 条并正序，实得 %q, %q", got[0].Content, got[1].Content)
	}
}

// P9 审计棘轮：非测试 Go 文件里对 messages 的直接 SQL 只能减少、不能增长。
//
// 为什么要这条：统一层写出来不等于问题消失——真正的风险是今后有人图省事又手写一遍
// 「FROM messages 加时间条件」，漏掉归档并表，再产出一个静默失真的统计。
// 基线里的数字是**当前实测残留**，迁移一处就调小一处；新文件出现直接 SQL 必须为 0。
func TestNoGrowthOfRawMessagesQueries(t *testing.T) {
	// 统一层自己必须有这几处；这些不是待清理的散点。
	exempt := map[string]bool{"history_source.go": true, "storage.go": true}
	baseline := map[string]int{
		"achievements.go":   1,
		"archive.go":        4,
		"ask.go":            1,
		"assistant.go":      4,
		"contact.go":        1,
		"followup.go":       1,
		"life_narrative.go": 3,
		"life_state.go":     1,
		"merge.go":          5,
		"metrics.go":        3,
		"mirror.go":         2,
		"relationship.go":   5,
		"search.go":         1,
		"social.go":         2,
		"timeline.go":       2,
	}

	rawRE := regexp.MustCompile(`FROM messages\b`)
	counts := map[string]int{}
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		base := filepath.Base(path)
		if exempt[base] {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if n := len(rawRE.FindAllIndex(b, -1)); n > 0 {
			counts[base] = n
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}

	// 1) 新出现的文件（基线里没有）一律不允许
	var fresh []string
	for f := range counts {
		if _, ok := baseline[f]; !ok {
			fresh = append(fresh, f)
		}
	}
	sort.Strings(fresh)
	if len(fresh) > 0 {
		t.Fatalf("这些文件不该再有对 messages 的直接 SQL（请改走 history_source.go 的统一入口）：%v", fresh)
	}
	// 2) 已有文件不得高于基线
	var grew []string
	for f, want := range baseline {
		if got := counts[f]; got > want {
			grew = append(grew, f)
		}
	}
	sort.Strings(grew)
	if len(grew) > 0 {
		t.Fatalf("散点 SQL 相对基线增长了：%v", grew)
	}
	// 3) 基线本身不能虚高：写进基线的残留必须真实存在（防止把已迁移的文件留在名单里冒充进度）
	for f, want := range baseline {
		if want > 0 && counts[f] == 0 {
			t.Fatalf("%s 已在基线记为 %d 处残留，实测已为 0——请把基线调小", f, want)
		}
	}
}
