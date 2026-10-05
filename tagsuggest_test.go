package main

// v5.0.0 智能分组建议回归（确定性规则 + 一键采纳）。
//
// 证明：
//   - SuggestTags 同输入两次调用逐字段相等（reflect.DeepEqual）；
//   - 命中规则的建议 reason 非空、confidence ∈ [1,100]、tagName 非空；
//   - 已挂同名标签被去重、每联系人限 perContactMax 条；
//   - assistant_emotions / followup_items 表缺失时静默跳过（不报错、不阻塞其它规则）；
//   - apply 幂等：重复采纳不产生重复 link、第二次 affected 为 0；
//   - apply 超限护栏生效。

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"
)

// sugDB 建一个「跑过 tag+timeline+followup+assistant 全表」的库，专门给 tagsuggest 用。
func sugDB(t *testing.T) *sql.DB {
	t.Helper()
	db := vaDB(t)
	if err := ensureAssistantTables(db); err != nil {
		t.Fatal(err)
	}
	return db
}

// sugSeedRichContact 造一个「画像有地域/职业/兴趣 + 近 90 天高频互动 + 情绪告警 + open 待跟进」的联系人。
// 消息铺到 30 个不同日历天、me/other 各 30 条：活跃度(封顶15天=40) + 均衡度(30) +
// 对方投入(封顶30条=15) + 对方均长(>=50字节封顶=15) → 亲密度≈100，稳定触发「核心关系」分层。
func sugSeedRichContact(t *testing.T, db *sql.DB, name string, profileJSON string) int64 {
	t.Helper()
	id := regressionContact(t, db, name)
	if err := SaveProfile(db, id, profileJSON, "s", "init"); err != nil {
		t.Fatal(err)
	}
	base := time.Now().AddDate(0, 0, -40)
	for i := 0; i < 30; i++ {
		d := base.AddDate(0, 0, i)
		vaMsg(t, db, id, "other", "这是一条测试消息用来刷活跃度，内容长度适中就够了嗯嗯。", d)
		vaMsg(t, db, id, "me", "收到，我这边也回复一下保持均衡。", d.Add(time.Hour))
	}
	return id
}

// sugHasTag 建议在列表中命中
func sugHasTag(list []TagSuggestion, cid int64, tag string) bool {
	for _, s := range list {
		if s.ContactID == cid && s.TagName == tag {
			return true
		}
	}
	return false
}

// sugInsertEmotionAlert 直接写一条 alert=1 的近窗口情绪记录
func sugInsertEmotionAlert(t *testing.T, db *sql.DB, cid int64) {
	t.Helper()
	dbMu.Lock()
	defer dbMu.Unlock()
	_, err := db.Exec(
		`INSERT INTO assistant_emotions (contact_id, emotion, score, summary, advice, alert, created_at)
		 VALUES (?, '低落', 25, '最近很疲惫', '多关心', 1, datetime('now','localtime'))`,
		cid)
	if err != nil {
		t.Fatal(err)
	}
}

// sugInsertOpenMoney 直接写一条 open 的 money 类待跟进
func sugInsertOpenMoney(t *testing.T, db *sql.DB, cid int64) {
	t.Helper()
	now := time.Now().Format(time.RFC3339)
	if _, err := insertFollowup(db, cid, "money", "对方借我的 500 元未还", "500", now, now); err != nil {
		t.Fatal(err)
	}
}

func TestSuggestTagsDeterministic(t *testing.T) {
	db := sugDB(t)
	pj := `{"basic_info":{"occupation":"后端开发工程师","location":"上海","important_dates":{"dates":[]}},
	        "interests":["攀岩","旅行"],
	        "communication_style":{"frequent_phrases":[]},
	        "personality":[],"emotional_patterns":{},"relationship":{},"important_facts":[]}`
	rich := sugSeedRichContact(t, db, "小张", pj)
	sugInsertEmotionAlert(t, db, rich)
	sugInsertOpenMoney(t, db, rich)

	// 全规则命中需容纳 7 条（分层/情绪/待跟进/地域/职业/兴趣×2），用硬上限 8。
	s1, err := SuggestTags(db, []int64{rich}, 8)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := SuggestTags(db, []int64{rich}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s1, s2) {
		t.Fatalf("两次调用应逐字段相等，got1=%+v got2=%+v", s1, s2)
	}
	if len(s1) == 0 {
		t.Fatal("应至少命中若干条建议")
	}
	// 关键规则齐备：分层 / 情绪 / 待跟进 / 地域 / 职业 / 兴趣
	for _, want := range []string{"核心关系", "近期需关心", "有往来待跟进", "上海", "技术", "攀岩"} {
		if !sugHasTag(s1, rich, want) {
			t.Fatalf("应命中建议 %q，实际: %+v", want, s1)
		}
	}
	// 每联系人限 8 条 → 长度 <=8；命中理由非空、confidence 合法
	if len(s1) > 8 {
		t.Fatalf("perContactMax=8 未生效: %d 条", len(s1))
	}
	for _, s := range s1 {
		if strings.TrimSpace(s.TagName) == "" {
			t.Fatalf("tagName 不应为空: %+v", s)
		}
		if strings.TrimSpace(s.Reason) == "" {
			t.Fatalf("reason 不应为空: %+v", s)
		}
		if s.Confidence <= 0 || s.Confidence > 100 {
			t.Fatalf("confidence 越界: %+v", s)
		}
	}
	// perContactMax 截断生效：只保留置信度最高的 2 条
	sTrim, err := SuggestTags(db, []int64{rich}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(sTrim) != 2 {
		t.Fatalf("perContactMax=2 应截断到 2 条, got %d: %+v", len(sTrim), sTrim)
	}
}

func TestSuggestTagsDedupExistingTag(t *testing.T) {
	db := sugDB(t)
	pj := `{"basic_info":{"occupation":"律师","location":"北京"},"interests":["阅读"]}`
	cid := sugSeedRichContact(t, db, "小李", pj)
	// 预先给该联系人挂上「北京」与「法律」两个标签
	bj, err := CreateTag(db, "北京")
	if err != nil {
		t.Fatal(err)
	}
	fl, err := CreateTag(db, "法律")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetContactTags(db, cid, []int64{bj.ID, fl.ID}); err != nil {
		t.Fatal(err)
	}
	list, err := SuggestTags(db, []int64{cid}, 8)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.TagName == "北京" || s.TagName == "法律" {
			t.Fatalf("已挂同名标签 %q 应被去重: %+v", s.TagName, list)
		}
	}
}

func TestSuggestTagsMissingTablesSilent(t *testing.T) {
	// 用 vaDB（不含 assistant_emotions），且刻意不写 followup_items 数据：
	// 应能正常给出分层 / 地域 / 职业 / 兴趣 类建议，不因表缺失 500
	db := regressionDB(t)
	if err := ensureTagTables(db); err != nil {
		t.Fatal(err)
	}
	// 不建 followup_items 也不建 assistant_emotions
	pj := `{"basic_info":{"occupation":"医生","location":"广州"},"interests":["书法"]}`
	cid := regressionContact(t, db, "小王")
	if err := SaveProfile(db, cid, pj, "s", "init"); err != nil {
		t.Fatal(err)
	}
	base := time.Now().AddDate(0, 0, -10)
	for i := 0; i < 6; i++ {
		sender := "other"
		if i%2 == 1 {
			sender = "me"
		}
		vaMsg(t, db, cid, sender, "日常聊天内容一", base.Add(time.Duration(i)*time.Hour))
	}
	list, err := SuggestTags(db, []int64{cid}, 8)
	if err != nil {
		t.Fatalf("缺表不应报错: %v", err)
	}
	if !sugHasTag(list, cid, "广州") || !sugHasTag(list, cid, "医疗") || !sugHasTag(list, cid, "书法") {
		t.Fatalf("画像/地域/职业规则应命中，got %+v", list)
	}
	// 情绪 / 待跟进规则应缺席
	for _, s := range list {
		if s.TagName == "近期需关心" || s.TagName == "有往来待跟进" {
			t.Fatalf("情绪/待跟进表缺失时不应出现 %q: %+v", s.TagName, list)
		}
	}
}

func TestApplyTagSuggestionIdempotent(t *testing.T) {
	db := sugDB(t)
	cid := regressionContact(t, db, "小赵")
	n1, err := ApplyTagSuggestion(db, cid, "老同学")
	if err != nil {
		t.Fatal(err)
	}
	if n1 != 1 {
		t.Fatalf("首次采纳 affected 应为 1, got %d", n1)
	}
	n2, err := ApplyTagSuggestion(db, cid, "老同学")
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Fatalf("重复采纳 affected 应为 0, got %d", n2)
	}
	// 数据库层链路数应为 1
	dbMu.Lock()
	var cnt int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM contact_tag_links l JOIN contact_tags t ON t.id = l.tag_id
		 WHERE l.contact_id = ? AND t.name = '老同学'`, cid).Scan(&cnt); err != nil {
		dbMu.Unlock()
		t.Fatal(err)
	}
	dbMu.Unlock()
	if cnt != 1 {
		t.Fatalf("link 应只有一条, got %d", cnt)
	}

	// 批量 apply 混合新旧 items → 只新增未挂的部分
	type item = struct {
		ContactID int64  `json:"contactId"`
		TagName   string `json:"tagName"`
	}
	items := []item{
		{cid, "老同学"}, // 重复
		{cid, "客户"},  // 新
	}
	affected, err := ApplyTagSuggestions(db, items)
	if err != nil {
		t.Fatal(err)
	}
	if affected != 1 {
		t.Fatalf("批量 apply 期望 affected=1（去重后只新增一个 link）, got %d", affected)
	}

	// 联系人不存在 → 拒绝
	if _, err := ApplyTagSuggestion(db, 999999, "任意"); err == nil {
		t.Fatal("联系人不存在应报错")
	}
}

func TestApplyRejectsOversizedBatch(t *testing.T) {
	db := sugDB(t)
	cid := regressionContact(t, db, "小钱")
	type item = struct {
		ContactID int64  `json:"contactId"`
		TagName   string `json:"tagName"`
	}
	items := make([]item, suggestApplyMaxItems+1)
	for i := range items {
		items[i].ContactID = cid
		items[i].TagName = "标签" + strings.Repeat("x", 1) + time.Now().Format("150405.000000000") + string(rune('A'+i%26)) + string(rune('a'+i%26))
	}
	if _, err := ApplyTagSuggestions(db, items); err == nil {
		t.Fatal("超过 suggestApplyMaxItems 应拒绝")
	}
}

// 服务层：ids 为空应走全量选样路径（有护栏），返回非 nil 切片。
func TestSuggestTagsAllContacts(t *testing.T) {
	db := sugDB(t)
	_ = regressionContact(t, db, "路人甲") // 没画像没消息 → 建议可能为空，但不应报错
	list, err := SuggestTags(db, nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	if list == nil {
		t.Fatal("空返回也应是 [] 而非 nil")
	}
}
