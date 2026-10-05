package main

// 增值功能（v3.1）回归测试：联系人标签、全文搜索、时间线、待跟进、
// 日历订阅、社交大盘、疑似重复推荐、年度关系报告。
//
// 这些功能全部是纯增量模块，测试重点除了各自的行为，还包括
// "表不存在时不能拖垮既有路径"这一零侵入约束。

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// vaDB 建一个跑过全部增值建表的测试库
func vaDB(t *testing.T) *sql.DB {
	t.Helper()
	db := regressionDB(t)
	for _, ensure := range []func(*sql.DB) error{ensureTagTables, ensureTimelineTables, ensureFollowupTables} {
		if err := ensure(db); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// vaMsg 按指定时间写一条消息
func vaMsg(t *testing.T, db *sql.DB, cid int64, sender, content string, ts time.Time) {
	t.Helper()
	if _, err := SaveMessages(db, cid, []Message{{Sender: sender, Content: content, Timestamp: ts}}); err != nil {
		t.Fatal(err)
	}
}

// ---------- 联系人标签 ----------

func TestTagNameNormalizeAndParse(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"  家人  ", "家人", false},
		{"老\t同学\n", "老 同学", false},
		{"   ", "", true},
		{"", "", true},
		{strings.Repeat("长", maxTagNameRunes+1), "", true},
		{strings.Repeat("长", maxTagNameRunes), strings.Repeat("长", maxTagNameRunes), false},
	}
	for _, c := range cases {
		got, err := normalizeTagName(c.in)
		if c.wantErr {
			if err == nil {
				t.Fatalf("normalizeTagName(%q) 期望报错，实际得到 %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("normalizeTagName(%q) 意外报错: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("normalizeTagName(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}

	// parseTagIDs 只负责解析，去重交给 SetContactTags（保持原始顺序，便于按位置对应）
	if ids := parseTagIDs("1,2,,3,x,2"); len(ids) != 4 || ids[0] != 1 || ids[2] != 3 || ids[3] != 2 {
		t.Fatalf("parseTagIDs 解析错误: %v", ids)
	}
	if ids := parseTagIDs("  "); len(ids) != 0 {
		t.Fatalf("parseTagIDs 空串应返回空切片，得到 %v", ids)
	}
}

func TestTagsLifecycleAndBatch(t *testing.T) {
	db := vaDB(t)
	a := regressionContact(t, db, "张三")
	b := regressionContact(t, db, "李四")

	family, err := CreateTag(db, " 家人 ")
	if err != nil {
		t.Fatal(err)
	}
	if family.Name != "家人" || family.ID <= 0 {
		t.Fatalf("CreateTag 返回异常: %+v", family)
	}
	// 同名再建应幂等复用，不产生第二条
	again, err := CreateTag(db, "家人")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != family.ID {
		t.Fatalf("同名标签应复用 id %d，实际 %d", family.ID, again.ID)
	}
	work, err := CreateTag(db, "客户")
	if err != nil {
		t.Fatal(err)
	}

	if err := SetContactTags(db, a, []int64{family.ID, work.ID, work.ID, 0}); err != nil {
		t.Fatal(err)
	}
	got, err := GetContactTags(db, a)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("联系人 a 应有 2 个标签，实际 %d 个: %+v", len(got), got)
	}
	// 覆盖式设置：只留一个
	if err := SetContactTags(db, a, []int64{work.ID}); err != nil {
		t.Fatal(err)
	}
	if got, _ = GetContactTags(db, a); len(got) != 1 || got[0].ID != work.ID {
		t.Fatalf("SetContactTags 应是覆盖式，实际 %+v", got)
	}
	// 不存在的标签要报错，且整批回滚
	if err := SetContactTags(db, a, []int64{work.ID, 9999}); err == nil {
		t.Fatal("给不存在的标签打标应该报错")
	}
	if got, _ = GetContactTags(db, a); len(got) != 1 {
		t.Fatalf("打标失败应回滚，实际 %+v", got)
	}
	// 不存在的联系人
	if err := SetContactTags(db, 9999, []int64{work.ID}); err == nil {
		t.Fatal("给不存在的联系人打标应该报错")
	}

	// 批量打标
	n, err := BatchTag(db, []int64{a, b, 9999}, []int64{family.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("批量打标应影响 2 行（不存在的联系人跳过），实际 %d", n)
	}
	tags, err := ListTags(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 {
		t.Fatalf("应有 2 个标签，实际 %d", len(tags))
	}
	if tags[0].Name != "家人" || tags[0].Count != 2 {
		t.Fatalf("ListTags 应按人数倒序，实际 %+v", tags)
	}
	// 重复打同一个标签不应该再增加
	if n, _ = BatchTag(db, []int64{a}, []int64{family.ID}, false); n != 0 {
		t.Fatalf("重复打标影响行数应为 0，实际 %d", n)
	}
	// 批量移除
	if n, err = BatchTag(db, []int64{a, b}, []int64{family.ID}, true); err != nil || n != 2 {
		t.Fatalf("批量移除应影响 2 行，实际 %d (err=%v)", n, err)
	}
	if _, err = BatchTag(db, nil, []int64{family.ID}, false); err == nil {
		t.Fatal("没有选中联系人时批量操作应报错")
	}

	// 改名撞车
	if err := RenameTag(db, work.ID, "家人"); err == nil {
		t.Fatal("改成已存在的标签名应报错")
	}
	if err := RenameTag(db, work.ID, "  老同学  "); err != nil {
		t.Fatal(err)
	}
	if err := RenameTag(db, 9999, "无所谓"); err == nil {
		t.Fatal("改不存在的标签应报错")
	}

	// 删除标签应连带清掉关联，但不动联系人
	if err := DeleteTag(db, work.ID); err != nil {
		t.Fatal(err)
	}
	if err := DeleteTag(db, work.ID); err == nil {
		t.Fatal("重复删除应报错")
	}
	var links int
	if err := db.QueryRow(`SELECT COUNT(*) FROM contact_tag_links WHERE tag_id = ?`, work.ID).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if links != 0 {
		t.Fatalf("删除标签后关联应清空，实际剩 %d 条", links)
	}
	var contacts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM contacts`).Scan(&contacts); err != nil {
		t.Fatal(err)
	}
	if contacts != 2 {
		t.Fatalf("删除标签不应影响联系人，实际 %d 个", contacts)
	}
}

// ---------- 聊天记录全文搜索 ----------

func TestSearchKeywordsAndSnippet(t *testing.T) {
	if _, err := searchKeywords("   "); err == nil {
		t.Fatal("空关键词应报错")
	}
	kws, err := searchKeywords("  生日  礼物   餐厅 ")
	if err != nil {
		t.Fatal(err)
	}
	if len(kws) != 3 || kws[0] != "生日" {
		t.Fatalf("关键词拆分错误: %v", kws)
	}
	// 超过上限的关键词被丢弃
	kws, err = searchKeywords(strings.Repeat("a ", searchMaxKeywords+3))
	if err != nil {
		t.Fatal(err)
	}
	if len(kws) != searchMaxKeywords {
		t.Fatalf("关键词个数应被限制为 %d，实际 %d", searchMaxKeywords, len(kws))
	}

	if got := escapeLike(`100%_a\b`); got != `100\%\_a\\b` {
		t.Fatalf("escapeLike 转义错误: %s", got)
	}

	// 短内容原样返回（空白折叠成单空格）
	if got := buildSnippet("今天  很开心", []string{"开心"}); got != "今天 很开心" {
		t.Fatalf("短片段应折叠空白后原样返回，实际 %q", got)
	}
	// 长内容截断出省略号，且命中词在片段里
	long := strings.Repeat("前", 40) + "生日礼物" + strings.Repeat("后", 40)
	snip := buildSnippet(long, []string{"生日礼物"})
	if !strings.Contains(snip, "生日礼物") || !strings.HasPrefix(snip, "…") || !strings.HasSuffix(snip, "…") {
		t.Fatalf("片段截断不符合预期: %q", snip)
	}
	// 没命中时也要给出开头一段
	if got := buildSnippet(strings.Repeat("啊", 100), []string{"生日"}); !strings.HasPrefix(got, "啊") || !strings.HasSuffix(got, "…") {
		t.Fatalf("未命中时应截取开头，实际 %q", got)
	}
}

func TestSearchMessagesFiltersAndArchiveDegrade(t *testing.T) {
	db := vaDB(t)
	a := regressionContact(t, db, "张三")
	b := regressionContact(t, db, "李四")
	base := time.Date(2026, 3, 10, 9, 0, 0, 0, time.Local)
	vaMsg(t, db, a, "other", "下周我生日，记得来", base)
	vaMsg(t, db, a, "me", "好啊，礼物我准备好了", base.Add(time.Hour))
	vaMsg(t, db, b, "other", "今天天气不错", base.AddDate(0, 0, -20))

	res, err := SearchMessages(db, SearchOptions{Query: "生日", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || len(res.List) != 1 {
		t.Fatalf("应只命中 1 条，实际 total=%d list=%d", res.Total, len(res.List))
	}
	hit := res.List[0]
	if hit.ContactID != a || hit.Sender != "other" || hit.Archived {
		t.Fatalf("命中内容不符: %+v", hit)
	}
	if !strings.Contains(hit.Snippet, "生日") {
		t.Fatalf("片段应包含关键词: %q", hit.Snippet)
	}
	if hit.ContactName == "" {
		t.Fatal("命中结果应带联系人名")
	}

	// 联系人过滤
	res, err = SearchMessages(db, SearchOptions{Query: "我", ContactID: b})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 0 {
		t.Fatalf("按联系人过滤后应为 0 条，实际 %d", res.Total)
	}

	// 时间范围（只给日期时上界含当天）
	res, err = SearchMessages(db, SearchOptions{Query: "天气", From: "2026-02-18", To: "2026-02-18"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 {
		t.Fatalf("时间范围应命中 1 条，实际 %d", res.Total)
	}

	// 分页
	res, err = SearchMessages(db, SearchOptions{Query: "我", Offset: 0, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.List) != 1 || res.Total < 1 || res.Limit != 1 {
		t.Fatalf("分页参数异常: total=%d list=%d limit=%d", res.Total, len(res.List), res.Limit)
	}

	// 归档表不存在时必须降级而不是报错
	res, err = SearchMessages(db, SearchOptions{Query: "生日", IncludeArchive: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.ArchiveSkipped {
		t.Fatal("归档表不存在时 archiveSkipped 应为 true")
	}
	if res.Total != 1 {
		t.Fatalf("降级后仍应搜到活跃消息，实际 %d 条", res.Total)
	}

	// 空结果返回空切片而非 nil
	res, err = SearchMessages(db, SearchOptions{Query: "不存在的词"})
	if err != nil {
		t.Fatal(err)
	}
	if res.List == nil {
		t.Fatal("空结果应返回空切片")
	}
	if _, err = SearchMessages(db, SearchOptions{Query: ""}); err == nil {
		t.Fatal("空查询应报错")
	}
}

func TestSearchAfterArchiveTablesReady(t *testing.T) {
	db := vaDB(t)
	if err := ensureArchiveTables(db); err != nil {
		t.Fatal(err)
	}
	a := regressionContact(t, db, "张三")
	vaMsg(t, db, a, "other", "老照片里的生日会", time.Date(2026, 3, 10, 9, 0, 0, 0, time.Local))
	res, err := SearchMessages(db, SearchOptions{Query: "生日", IncludeArchive: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.ArchiveSkipped {
		t.Fatal("归档表就绪时不应标记为跳过")
	}
	if res.Total != 1 || res.List[0].Archived {
		t.Fatalf("应只命中活跃消息 1 条，实际 %+v", res.List)
	}
}

// ---------- 联系人时间线 ----------

func TestTimelineDerivedAndCustomEvents(t *testing.T) {
	db := vaDB(t)
	a := regressionContact(t, db, "张三")
	ts := time.Date(2026, 3, 10, 9, 0, 0, 0, time.Local)
	vaMsg(t, db, a, "other", "第一条消息", ts)

	items, err := GetContactTimeline(db, a, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		t.Fatal("有消息的联系人应该有派生时间线节点")
	}
	derived := false
	for _, it := range items {
		if it.Source == "derived" {
			derived = true
		}
		if it.Source == "record" {
			t.Fatalf("还没手动记录，不应出现 record 节点: %+v", it)
		}
		if it.EventTime == "" || it.Title == "" {
			t.Fatalf("节点缺少必要字段: %+v", it)
		}
	}
	if !derived {
		t.Fatal("应至少有一个派生节点")
	}

	// 手动记录
	when := ts.Add(-24 * time.Hour)
	id, err := AddContactEvent(db, a, "  一起吃饭  ", "聊了下换工作的事", when)
	if err != nil {
		t.Fatal(err)
	}
	if id <= 0 {
		t.Fatalf("AddContactEvent 应返回自增 id，实际 %d", id)
	}
	items, err = GetContactTimeline(db, a, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range items {
		if it.ID == id {
			found = true
			if it.Kind != "custom" || it.Source != "record" {
				t.Fatalf("手动节点字段不符: %+v", it)
			}
			if it.Title != "一起吃饭" || !strings.Contains(it.Detail, "换工作") {
				t.Fatalf("手动节点内容不符: %+v", it)
			}
			if it.EventTime != when.Format("2006-01-02 15:04:05") {
				t.Fatalf("事件时间格式化不符: %q", it.EventTime)
			}
		}
	}
	if !found {
		t.Fatal("时间线里找不到刚记录的事件")
	}

	// 入参校验
	if _, err = AddContactEvent(db, a, "   ", "", when); err == nil {
		t.Fatal("空标题应报错")
	}
	if _, err = AddContactEvent(db, 9999, "x", "", when); err == nil {
		t.Fatal("不存在的联系人应报错")
	}
	if _, err = AddContactEvent(db, a, "x", "", time.Now().Add(72*time.Hour)); err == nil {
		t.Fatal("晚于明天的事件时间应报错")
	}
	// 超长标题按 rune 截断
	long, err := AddContactEvent(db, a, strings.Repeat("事", timelineTitleMaxRunes+10), "", when)
	if err != nil {
		t.Fatal(err)
	}
	items, _ = GetContactTimeline(db, a, 100)
	for _, it := range items {
		if it.ID == long && !strings.HasSuffix(it.Title, "…") {
			t.Fatalf("超长标题应被截断并带省略号: %q", it.Title)
		}
	}

	// 删除（必须带对联系人 id）
	if err = DeleteContactEvent(db, 9999, id); err == nil {
		t.Fatal("用别的联系人 id 删事件应报错")
	}
	if err = DeleteContactEvent(db, a, id); err != nil {
		t.Fatal(err)
	}
	if err = DeleteContactEvent(db, a, id); err == nil {
		t.Fatal("重复删除应报错")
	}

	// 不存在的联系人明确报错，前端好给出「联系人不存在」而不是空白页
	if _, err = GetContactTimeline(db, 9999, 10); err == nil {
		t.Fatal("不存在的联系人应报错")
	}
}

func TestParseTimeLooseAndTruncateRunes(t *testing.T) {
	ok := []string{
		"2026-03-10T09:00:00+08:00",
		"2026-03-10 09:00:00",
		"2026-03-10T09:00:00",
		"2026-03-10",
	}
	for _, s := range ok {
		if _, good := parseTimeLoose(s); !good {
			t.Fatalf("parseTimeLoose(%q) 应能解析", s)
		}
	}
	bad := []string{"", "   ", "2026-03-10 09:00", "不是时间", "2026/03/10"}
	for _, s := range bad {
		if _, good := parseTimeLoose(s); good {
			t.Fatalf("parseTimeLoose(%q) 不应解析成功", s)
		}
	}
	if got := truncateRunes("abcdef", 3); got != "abc…" {
		t.Fatalf("truncateRunes 截断错误: %q", got)
	}
	if got := truncateRunes("  ab  ", 10); got != "ab" {
		t.Fatalf("truncateRunes 应去空白: %q", got)
	}
}

// ---------- 待跟进 ----------

func TestFollowupManualAndStatus(t *testing.T) {
	db := vaDB(t)
	a := regressionContact(t, db, "张三")

	if _, err := AddFollowup(db, a, "promise", "   ", "", ""); err == nil {
		t.Fatal("空内容应报错")
	}
	if _, err := AddFollowup(db, 9999, "promise", "还钱", "", ""); err == nil {
		t.Fatal("不存在的联系人应报错")
	}

	// 非法类型回落成 custom
	id, err := AddFollowup(db, a, "瞎写的", "记得回他消息", "", "")
	if err != nil {
		t.Fatal(err)
	}
	items, err := ListFollowups(db, "open", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("应有 1 条待跟进，实际 %d", len(items))
	}
	if items[0].ID != id || items[0].Kind != "custom" || items[0].Status != "open" {
		t.Fatalf("待跟进字段不符: %+v", items[0])
	}
	if items[0].Name != "张三" || items[0].KindLabel == "" {
		t.Fatalf("应带联系人名和类型中文标签: %+v", items[0])
	}

	// 同内容重复添加应去重复用同一条
	id2, err := AddFollowup(db, a, "custom", "记得回他消息", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id {
		t.Fatalf("重复添加应复用 id %d，实际 %d", id, id2)
	}
	if all, _ := ListFollowups(db, "all", 50); len(all) != 1 {
		t.Fatalf("去重后应只有 1 条，实际 %d 条", len(all))
	}

	// 带金额的钱款往来
	money, err := AddFollowup(db, a, "money", "借了他两千", "2000 元", "")
	if err != nil {
		t.Fatal(err)
	}
	if money == id {
		t.Fatal("不同内容不应被去重")
	}

	// 超长内容截断
	longID, err := AddFollowup(db, a, "custom", strings.Repeat("长", followupContentMax+20), "", "")
	if err != nil {
		t.Fatal(err)
	}
	all, _ := ListFollowups(db, "all", 50)
	for _, it := range all {
		if it.ID == longID && len([]rune(it.Content)) != followupContentMax {
			t.Fatalf("内容应截断到 %d 字，实际 %d", followupContentMax, len([]rune(it.Content)))
		}
	}

	// 状态流转
	if err = SetFollowupStatus(db, id, "done"); err != nil {
		t.Fatal(err)
	}
	if err = SetFollowupStatus(db, id, "不合法"); err == nil {
		t.Fatal("非法状态应报错")
	}
	if err = SetFollowupStatus(db, 9999, "done"); err == nil {
		t.Fatal("不存在的事项应报错")
	}
	open, _ := ListFollowups(db, "open", 50)
	for _, it := range open {
		if it.ID == id {
			t.Fatal("已完成的事项不应再出现在 open 列表")
		}
	}
	done, _ := ListFollowups(db, "done", 50)
	if len(done) != 1 || done[0].ID != id {
		t.Fatalf("done 列表应只有刚勾掉那条，实际 %+v", done)
	}
	// 重新加回同一条内容应把状态复位成 open
	if _, err = AddFollowup(db, a, "custom", "记得回他消息", "", ""); err != nil {
		t.Fatal(err)
	}
	open, _ = ListFollowups(db, "open", 50)
	back := false
	for _, it := range open {
		if it.ID == id {
			back = true
		}
	}
	if !back {
		t.Fatal("重复添加应把已完成的事项复位为 open")
	}

	// 非法筛选值
	if _, err = ListFollowups(db, "瞎写的", 10); err == nil {
		t.Fatal("非法状态筛选应报错")
	}
	// 空列表返回切片
	if got, err := ListFollowups(db, "ignored", 10); err != nil || got == nil {
		t.Fatalf("空结果应返回空切片, err=%v", err)
	}

	if err = DeleteFollowup(db, money); err != nil {
		t.Fatal(err)
	}
	if err = DeleteFollowup(db, money); err != nil {
		t.Fatal("DeleteFollowup 对不存在的 id 不应报错")
	}
}

func TestFollowupKindAndStatusValidators(t *testing.T) {
	for _, k := range []string{"question", "promise", "money", "custom"} {
		if !validFollowupKind(k) {
			t.Fatalf("%s 应是合法类型", k)
		}
		if followupKindLabel(k) == "" {
			t.Fatalf("%s 应有中文标签", k)
		}
	}
	if validFollowupKind("unknown") {
		t.Fatal("未知类型不应合法")
	}
	for _, s := range []string{"open", "done", "ignored"} {
		if !validFollowupStatus(s) {
			t.Fatalf("%s 应是合法状态", s)
		}
	}
	if validFollowupStatus("all") {
		t.Fatal("all 是筛选值不是状态")
	}
}

// v4.7.0：跟进截止日期 due_date 读写往返 + 格式校验
func TestFollowupDueDateRoundTrip(t *testing.T) {
	db := vaDB(t)
	a := regressionContact(t, db, "张三")

	// 带截止日期添加 → 回读应原样拿到
	id, err := AddFollowup(db, a, "promise", "下周还书", "", "2026-10-20")
	if err != nil {
		t.Fatal(err)
	}
	items, err := ListFollowups(db, "open", 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range items {
		if it.ID == id {
			found = true
			if it.DueDate != "2026-10-20" {
				t.Fatalf("due_date 往返不符，期望 2026-10-20，实际 %q", it.DueDate)
			}
		}
	}
	if !found {
		t.Fatal("带截止日期添加的事项未出现在 open 列表")
	}

	// 空截止日期合法（无截止）
	if _, err := AddFollowup(db, a, "custom", "无截止事项", "", ""); err != nil {
		t.Fatal(err)
	}

	// 非法格式必须报错，且不落库
	if _, err := AddFollowup(db, a, "custom", "非法日期", "", "2026-13-45"); err == nil {
		t.Fatal("非法 YYYY-MM-DD 应报错")
	}
	if _, err := AddFollowup(db, a, "custom", "非法日期2", "", "20261020"); err == nil {
		t.Fatal("无分隔符应报错")
	}
	if _, err := AddFollowup(db, a, "custom", "非法日期3", "", "10-20"); err == nil {
		t.Fatal("缺年份应报错")
	}

	// 同内容重复添加应刷新 due_date（ON CONFLICT 分支）
	if _, err := AddFollowup(db, a, "promise", "下周还书", "", "2026-11-01"); err != nil {
		t.Fatal(err)
	}
	items, _ = ListFollowups(db, "open", 50)
	for _, it := range items {
		if it.ID == id && it.DueDate != "2026-11-01" {
			t.Fatalf("重复添加应刷新 due_date 到 2026-11-01，实际 %q", it.DueDate)
		}
	}
}

// v4.7.0：isValidYMD 严格校验（零填充、月/日合法、回式一致）
func TestIsValidYMD(t *testing.T) {
	for _, ok := range []string{"2026-01-01", "2026-10-20", "2024-02-29", "2026-12-31"} {
		if !isValidYMD(ok) {
			t.Errorf("%q 应为合法日期", ok)
		}
	}
	for _, bad := range []string{"", "2026-1-2", "2026-13-01", "2026-02-30", "2025-02-29", "20261020", "10-20-2026", "2026-10-20 "} {
		if isValidYMD(bad) {
			t.Errorf("%q 应为非法日期", bad)
		}
	}
}

// v4.7.0：既有库缺 due_date 列时 ensureFollowupTables 幂等补列，且重复执行不报错
func TestFollowupDueDateIdempotentAlter(t *testing.T) {
	db := regressionDB(t)
	// 手工造一张旧架构表（无 due_date 列），模拟升级前的库
	if _, err := db.Exec(`CREATE TABLE followup_items (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		contact_id INTEGER NOT NULL,
		kind TEXT NOT NULL DEFAULT 'custom',
		content TEXT NOT NULL,
		amount TEXT NOT NULL DEFAULT '',
		source_msg_time TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'open',
		dedup_key TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatal(err)
	}
	if has, err := followupHasDueDate(db); err != nil || has {
		t.Fatalf("初始不应有 due_date 列，has=%v err=%v", has, err)
	}
	// 第一次 ensure：应补列
	if err := ensureFollowupTables(db); err != nil {
		t.Fatal(err)
	}
	if has, err := followupHasDueDate(db); err != nil || !has {
		t.Fatalf("ensure 后应有 due_date 列，has=%v err=%v", has, err)
	}
	// 第二次 ensure：幂等，不报错（不 bump user_version、不动 backup.go）
	if err := ensureFollowupTables(db); err != nil {
		t.Fatalf("重复 ensure 应幂等，实际报错: %v", err)
	}
	// 补列后可正常写入并回读 due_date
	a := regressionContact(t, db, "补列后测试")
	if _, err := AddFollowup(db, a, "custom", "升级后新增", "", "2026-12-25"); err != nil {
		t.Fatal(err)
	}
	items, err := ListFollowups(db, "open", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].DueDate != "2026-12-25" {
		t.Fatalf("补列后 due_date 往返异常: %+v", items)
	}
}

func TestFollowupEmailSection(t *testing.T) {
	if got := buildFollowupEmailSection(nil); got != "" {
		t.Fatalf("没有待跟进时不应产生邮件板块，实际 %q", got)
	}
	items := []FollowupItem{{
		Name:      "张三<script>",
		KindLabel: "承诺",
		Content:   "答应发资料 & 报价",
		Amount:    `2000"元"`,
	}, {
		Name: "李四", KindLabel: "待回复", Content: "问了周末",
		SourceTime: "2026-03-10 09:00",
	}}
	got := buildFollowupEmailSection(items)
	if !strings.Contains(got, "待跟进（2 项）") {
		t.Fatalf("板块标题不符: %s", got)
	}
	for _, evil := range []string{"<script>", "2000\"元\""} {
		if strings.Contains(got, evil) {
			t.Fatalf("邮件内容未转义，出现 %s", evil)
		}
	}
	if !strings.Contains(got, "&lt;script&gt;") || !strings.Contains(got, "答应发资料 &amp; 报价") {
		t.Fatalf("应看到转义后的文本: %s", got)
	}
	if !strings.Contains(got, "来源：2026-03-10 09:00") {
		t.Fatalf("应带来源时间: %s", got)
	}

	body := emailHeader("每日提醒") + `<p>正文</p>` + emailFooter
	merged := insertEmailSection(body, "<section/>")
	if !strings.Contains(merged, "<section/>") {
		t.Fatal("板块没插进去")
	}
	if strings.Index(merged, "<section/>") > strings.Index(merged, emailFooter) {
		t.Fatal("板块应插在页脚之前")
	}
	if insertEmailSection(body, "") != body {
		t.Fatal("空板块应原样返回")
	}
	if got := insertEmailSection("<p>无页脚</p>", "<section/>"); got != "<p>无页脚</p><section/>" {
		t.Fatalf("找不到页脚时应追加到末尾，实际 %q", got)
	}
}

// ---------- 日历订阅 ----------

func TestCalendarKeyAndICSPrimitives(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		k := newCalendarKey()
		if len(k) != calendarKeyBytes*2 {
			t.Fatalf("订阅密钥长度应为 %d，实际 %d", calendarKeyBytes*2, len(k))
		}
		if seen[k] {
			t.Fatal("订阅密钥重复了")
		}
		seen[k] = true
	}

	if got := icsEscape("  生日; 5月20日, 农历\n第二行\\反斜杠\t制表 "); got != `生日\; 5月20日\, 农历\n第二行\\反斜杠 制表` {
		t.Fatalf("icsEscape 转义错误: %q", got)
	}
	if got := icsFold("ABC"); got != "ABC" {
		t.Fatalf("短行不应折行: %q", got)
	}
	long := "SUMMARY:" + strings.Repeat("生", 60)
	folded := icsFold(long)
	for _, line := range strings.Split(folded, "\r\n") {
		if len(line) > 75 {
			t.Fatalf("折行后仍有超过 75 字节的行: %q", line)
		}
	}
	if !strings.Contains(folded, "\r\n ") {
		t.Fatal("长行应被折行且续行以空格开头")
	}
	if strings.ReplaceAll(folded, "\r\n ", "") != long {
		t.Fatal("折行应可无损还原")
	}

	if icsUID(7, "5月20日") != icsUID(7, "5月20日") {
		t.Fatal("UID 应稳定")
	}
	if icsUID(7, "5月20日") == icsUID(8, "5月20日") {
		t.Fatal("不同联系人的 UID 不应相同")
	}
	if !strings.HasSuffix(icsUID(7, "x"), "@wechat-profile-bot") {
		t.Fatalf("UID 域名后缀不符: %s", icsUID(7, "x"))
	}
}

func TestBuildCalendarICS(t *testing.T) {
	db := vaDB(t)
	a := regressionContact(t, db, "张三")
	profile := `{"summary":"朋友","basic_info":{"occupation":"","location":"","important_dates":["生日：5月20日","完全不认识的写法"]}}`
	if err := SaveProfile(db, a, profile, "朋友", "initial"); err != nil {
		t.Fatal(err)
	}
	// 没画像的联系人不应导致报错
	regressionContact(t, db, "李四")

	now := time.Date(2026, 3, 10, 9, 0, 0, 0, time.Local)
	body, err := BuildCalendarICS(db, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"BEGIN:VCALENDAR", "VERSION:2.0", "METHOD:PUBLISH", "END:VCALENDAR",
		"BEGIN:VEVENT", "END:VEVENT", "RRULE:FREQ=YEARLY", "DTSTART;VALUE=DATE:", "UID:",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("ICS 缺少 %q:\n%s", want, body)
		}
	}
	if strings.Count(body, "BEGIN:VEVENT") != 1 {
		t.Fatalf("只应导出 1 条可识别的日子，实际 %d 条:\n%s", strings.Count(body, "BEGIN:VEVENT"), body)
	}
	if !strings.Contains(body, "生日") || !strings.Contains(body, "张三") {
		t.Fatalf("事件标题应含联系人和类型:\n%s", body)
	}
	// 行分隔必须是 CRLF
	if strings.Contains(strings.ReplaceAll(body, "\r\n", ""), "\n") {
		t.Fatal("ICS 必须全程使用 CRLF 换行")
	}
	// 同一份数据重复生成结果一致（订阅续期不会产生重复事件）
	again, err := BuildCalendarICS(db, now)
	if err != nil {
		t.Fatal(err)
	}
	if again != body {
		t.Fatal("同样输入应生成同样的 ICS")
	}

	// 完全没有重要日子时也要给出合法的空日历
	db2 := vaDB(t)
	regressionContact(t, db2, "王五")
	empty, err := BuildCalendarICS(db2, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(empty, "BEGIN:VCALENDAR") || strings.Contains(empty, "BEGIN:VEVENT") {
		t.Fatalf("空日历不符预期:\n%s", empty)
	}
}

// ---------- 社交大盘 ----------

func TestSocialStatsPrimitives(t *testing.T) {
	if got := avgInt(nil); got != 0 {
		t.Fatalf("空集平均值应为 0，实际 %v", got)
	}
	if got := avgInt([]int64{1, 2, 6}); got != 3 {
		t.Fatalf("平均值计算错误: %v", got)
	}
	if got := medianInt(nil); got != 0 {
		t.Fatalf("空集中位数应为 0，实际 %v", got)
	}
	if got := medianInt([]int64{5, 1, 3}); got != 3 {
		t.Fatalf("奇数个中位数错误: %v", got)
	}
	if got := medianInt([]int64{4, 1, 3, 2}); got != 2.5 {
		t.Fatalf("偶数个中位数错误: %v", got)
	}

	// 空格与标点都是分隔符：哈哈 / hello / world / 好的123
	runs := cjkRuns("哈哈, hello world! 好的123")
	if len(runs) != 4 || runs[0] != "哈哈" || runs[1] != "hello" || runs[3] != "好的123" {
		t.Fatalf("cjkRuns 切分错误: %v", runs)
	}
	if got := extractPhrases(nil, 5); got == nil || len(got) != 0 {
		t.Fatalf("空输入应返回空切片，实际 %v", got)
	}
	texts := []string{"哈哈哈哈", "哈哈哈哈", "哈哈哈哈", "好的呀", "好的呀", "好的呀"}
	ph := extractPhrases(texts, 3)
	if len(ph) == 0 {
		t.Fatal("高频片段应能提取出来")
	}
	if len(ph) > 3 {
		t.Fatalf("应受 topN 限制，实际 %d 条", len(ph))
	}
	found := false
	for _, p := range ph {
		if p.Phrase == "哈哈哈" {
			found = true
			if p.Count != 6 {
				t.Fatalf("「哈哈哈」计数应为 6，实际 %d", p.Count)
			}
		}
		if p.Count < 3 {
			t.Fatalf("低于阈值的片段不应出现: %+v", p)
		}
	}
	if !found {
		t.Fatalf("应提取到「哈哈哈」，实际 %+v", ph)
	}
}

func TestComputeSocialStats(t *testing.T) {
	db := vaDB(t)
	a := regressionContact(t, db, "张三")
	b := regressionContact(t, db, "李四")
	now := time.Now()
	vaMsg(t, db, a, "other", "在吗，明天有空聊聊项目", now.Add(-3*time.Hour))
	vaMsg(t, db, a, "me", "有的，下午三点", now.Add(-3*time.Hour+4*time.Minute))
	vaMsg(t, db, a, "other", "行，那就这样定了", now.Add(-3*time.Hour+40*time.Minute))
	vaMsg(t, db, b, "me", "老照片找到了", now.Add(-72*time.Hour))
	// 很久以前的消息不应进入 7 天窗口
	vaMsg(t, db, b, "other", "远古消息", now.AddDate(-2, 0, 0))

	st, err := ComputeSocialStats(db, 7)
	if err != nil {
		t.Fatal(err)
	}
	if st == nil || st.Days != 7 {
		t.Fatalf("统计结果异常: %+v", st)
	}
	if st.TotalMessages != 4 {
		t.Fatalf("窗口内应有 4 条消息，实际 %d", st.TotalMessages)
	}
	if st.MyMessages != 2 || st.TheirMessages != 2 {
		t.Fatalf("收发条数不符: mine=%d theirs=%d", st.MyMessages, st.TheirMessages)
	}
	if st.ActiveContacts != 2 {
		t.Fatalf("活跃联系人应为 2，实际 %d", st.ActiveContacts)
	}
	if len(st.Hourly) != 24 || st.Hourly[0].Hour != 0 || st.Hourly[23].Hour != 23 {
		t.Fatalf("小时桶应固定 24 个: %+v", st.Hourly)
	}
	if len(st.Weekday) != 7 {
		t.Fatalf("星期桶应固定 7 个: %v", st.Weekday)
	}
	hourSum, weekSum := 0, 0
	for _, h := range st.Hourly {
		hourSum += h.Mine + h.Theirs
	}
	for _, n := range st.Weekday {
		weekSum += n
	}
	if hourSum != 4 || weekSum != 4 {
		t.Fatalf("分桶合计应等于总条数，实际 hour=%d week=%d", hourSum, weekSum)
	}
	if st.MyReplyAvgSec <= 0 || st.MyReplySamples != 1 {
		t.Fatalf("我的回复速度统计不符: avg=%v samples=%d", st.MyReplyAvgSec, st.MyReplySamples)
	}
	if st.MyReplyAvgSec != 240 {
		t.Fatalf("回复间隔应为 240 秒，实际 %v", st.MyReplyAvgSec)
	}
	if st.TheirReplySamples != 1 || st.TheirReplyAvgSec <= 0 {
		t.Fatalf("对方回复速度统计不符: avg=%v samples=%d", st.TheirReplyAvgSec, st.TheirReplySamples)
	}
	if len(st.Top) == 0 || st.Top[0].Name == "" {
		t.Fatalf("聊得最多的人不应为空: %+v", st.Top)
	}
	if st.Top == nil || st.Initiators == nil || st.MyPhrases == nil || st.TheirPhrases == nil {
		t.Fatal("切片字段不应为 nil")
	}
	if st.Truncated {
		t.Fatal("数据量很小不应触发截断")
	}
	if st.From == "" || st.GeneratedAt == "" {
		t.Fatal("应带上区间与生成时间")
	}

	// 非法天数走默认值，不报错
	def, err := ComputeSocialStats(db, 0)
	if err != nil {
		t.Fatal(err)
	}
	if def.Days != socialDefaultDays {
		t.Fatalf("days<=0 应回落到默认 %d，实际 %d", socialDefaultDays, def.Days)
	}
	huge, err := ComputeSocialStats(db, socialMaxDays+100)
	if err != nil {
		t.Fatal(err)
	}
	if huge.Days != socialMaxDays {
		t.Fatalf("days 应被夹到上限 %d，实际 %d", socialMaxDays, huge.Days)
	}
}

// ---------- 疑似重复联系人 ----------

func TestDuplicateNameScoring(t *testing.T) {
	if got := normalizeForMatch("  Wang 小明! 🎉 "); got != "wang小明" {
		t.Fatalf("normalizeForMatch 错误: %q", got)
	}
	if got := normalizeForMatch("！！！"); got != "" {
		t.Fatalf("纯符号应归一化为空串，实际 %q", got)
	}
	if got := levenshteinRunes(nil, []rune("abc")); got != 3 {
		t.Fatalf("空串编辑距离错误: %d", got)
	}
	if got := levenshteinRunes([]rune("张三"), []rune("张三")); got != 0 {
		t.Fatalf("相同串编辑距离应为 0，实际 %d", got)
	}
	if got := levenshteinRunes([]rune("张三"), []rune("张叁")); got != 1 {
		t.Fatalf("差一字编辑距离应为 1，实际 %d", got)
	}

	cases := []struct {
		a, b    string
		minWant int
		wantOK  bool
	}{
		{"张三", "张三", 100, true},
		{"张三", "张三三", 70, true},
		{"zhangsan", "ZhangSan", 100, true},
		{"张三", "李四", 0, false},
		{"张三", "", 0, false},
	}
	for _, c := range cases {
		score, reason := compareNames(normalizeForMatch(c.a), normalizeForMatch(c.b))
		if c.wantOK {
			if score < c.minWant || reason == "" {
				t.Fatalf("compareNames(%q,%q) = (%d,%q)，期望 >=%d 且有理由", c.a, c.b, score, reason, c.minWant)
			}
			continue
		}
		if score != 0 || reason != "" {
			t.Fatalf("compareNames(%q,%q) 应判为不相似，实际 (%d,%q)", c.a, c.b, score, reason)
		}
	}
}

func TestFindDuplicateContacts(t *testing.T) {
	db := vaDB(t)
	// GetOrCreateContact 会按名字复用，用备注区分出两个"同一个人"
	a := regressionContact(t, db, "张三")
	b := regressionContact(t, db, "张三 三")
	c := regressionContact(t, db, "完全无关的王五")
	if a == b {
		t.Fatal("测试前提不成立：两个联系人 id 相同")
	}
	if _, err := db.Exec(`UPDATE contacts SET remark = ? WHERE id = ?`, "张三", b); err != nil {
		t.Fatal(err)
	}
	vaMsg(t, db, a, "other", "你好", time.Date(2026, 3, 10, 9, 0, 0, 0, time.Local))

	res, err := FindDuplicateContacts(db)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 3 {
		t.Fatalf("应扫描 3 个联系人，实际 %d", res.Scanned)
	}
	if res.List == nil {
		t.Fatal("结果切片不应为 nil")
	}
	if res.Truncated {
		t.Fatal("联系人很少不应触发截断")
	}
	matched := false
	for _, p := range res.List {
		ids := map[int64]bool{p.A.ID: true, p.B.ID: true}
		if ids[c] {
			t.Fatalf("无关联系人不应被推荐合并: %+v", p)
		}
		if p.Reason == "" || p.Score < dupMinScore {
			t.Fatalf("推荐组合缺少理由或分数过低: %+v", p)
		}
		if p.SuggestedKeep != p.A.ID && p.SuggestedKeep != p.B.ID {
			t.Fatalf("建议保留的必须是二者之一: %+v", p)
		}
		if ids[a] && ids[b] {
			matched = true
			if p.A.MsgCount != 1 {
				t.Fatalf("应统计真实消息条数，实际 %d", p.A.MsgCount)
			}
		}
	}
	if !matched {
		t.Fatalf("同名联系人应被识别为疑似重复，实际 %+v", res.List)
	}

	// 已合并的联系人不参与比较
	if _, err := db.Exec(`UPDATE contacts SET merged_into = ? WHERE id = ?`, a, b); err != nil {
		t.Fatal(err)
	}
	res2, err := FindDuplicateContacts(db)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Scanned != 2 {
		t.Fatalf("已合并的联系人应被排除，实际扫描 %d 个", res2.Scanned)
	}
	for _, p := range res2.List {
		if p.A.ID == b || p.B.ID == b {
			t.Fatalf("已合并联系人不应再出现在建议里: %+v", p)
		}
	}

	// 只有一个联系人时直接返回空
	db2 := vaDB(t)
	regressionContact(t, db2, "独苗")
	solo, err := FindDuplicateContacts(db2)
	if err != nil {
		t.Fatal(err)
	}
	if len(solo.List) != 0 || solo.Scanned != 1 {
		t.Fatalf("单联系人不应有建议: %+v", solo)
	}
}

// ---------- 年度关系报告 ----------

func TestBuildAnnualReportAndRender(t *testing.T) {
	db := vaDB(t)
	a := regressionContact(t, db, "张三")
	b := regressionContact(t, db, "李四")
	year := 2025
	for i := 0; i < 5; i++ {
		vaMsg(t, db, a, "other", "哈哈哈今天真开心", time.Date(year, 3, 10+i, 9, 0, 0, 0, time.Local))
		vaMsg(t, db, a, "me", "是啊哈哈哈哈", time.Date(year, 3, 10+i, 9, 30, 0, 0, time.Local))
	}
	vaMsg(t, db, b, "me", "在吗", time.Date(year, 8, 1, 20, 0, 0, 0, time.Local))
	// 上一年的消息不应计入
	vaMsg(t, db, b, "other", "去年的事", time.Date(year-1, 12, 31, 23, 0, 0, 0, time.Local))
	if err := SaveProfile(db, a, `{"summary":"好朋友"}`, "好朋友", "初次生成"); err != nil {
		t.Fatal(err)
	}

	rep, err := BuildAnnualReport(db, year)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Year != year {
		t.Fatalf("年份不符: %d", rep.Year)
	}
	if rep.TotalMessages != 11 {
		t.Fatalf("全年应有 11 条消息，实际 %d", rep.TotalMessages)
	}
	if rep.MyMessages != 6 || rep.TheirMessages != 5 {
		t.Fatalf("收发条数不符: mine=%d theirs=%d", rep.MyMessages, rep.TheirMessages)
	}
	if len(rep.Months) != 12 {
		t.Fatalf("月份桶应固定 12 个，实际 %d", len(rep.Months))
	}
	if rep.Months[2].Month != 3 || rep.Months[2].Total != 10 {
		t.Fatalf("3 月统计不符: %+v", rep.Months[2])
	}
	if rep.Months[7].Total != 1 {
		t.Fatalf("8 月统计不符: %+v", rep.Months[7])
	}
	if rep.BusiestMonth != 3 {
		t.Fatalf("最活跃的月份应是 3 月，实际 %d", rep.BusiestMonth)
	}
	if rep.BusiestDay != "2025-03-10" && !strings.HasPrefix(rep.BusiestDay, "2025-03-1") {
		t.Fatalf("最活跃的一天不符: %q", rep.BusiestDay)
	}
	if rep.BusiestDayCnt != 2 {
		t.Fatalf("最活跃一天应有 2 条，实际 %d", rep.BusiestDayCnt)
	}
	if rep.ActiveContacts != 2 {
		t.Fatalf("聊过的人应为 2，实际 %d", rep.ActiveContacts)
	}
	if rep.ActiveDays != 6 {
		t.Fatalf("有记录的天数应为 6，实际 %d", rep.ActiveDays)
	}
	if len(rep.Top) == 0 || rep.Top[0].Name == "" || rep.Top[0].Total != 10 {
		t.Fatalf("聊得最多的人不符: %+v", rep.Top)
	}
	if rep.LongestSilence <= 0 {
		t.Fatalf("3 月到 8 月之间应有明显沉默期，实际 %d 天", rep.LongestSilence)
	}
	if rep.GeneratedAt == "" {
		t.Fatal("应带生成时间")
	}
	if rep.Months == nil || rep.Top == nil || rep.Emotions == nil || rep.Intimacy == nil || rep.Keywords == nil || rep.Events == nil {
		t.Fatal("切片字段不应为 nil")
	}
	// 情绪表没数据时应明确标记不可用，而不是给出全 0 曲线
	if rep.EmotionOn && len(rep.Emotions) == 0 {
		t.Fatal("emotionAvailable 与实际数据不一致")
	}
	if rep.Truncated {
		t.Fatal("数据量很小不应触发截断")
	}

	// 非法年份回落到当前年
	fallback, err := BuildAnnualReport(db, 1900)
	if err != nil {
		t.Fatal(err)
	}
	if fallback.Year != time.Now().Year() {
		t.Fatalf("非法年份应回落到当前年，实际 %d", fallback.Year)
	}

	// 可分享长页
	page := RenderReportHTML(rep)
	for _, want := range []string{"<!DOCTYPE html>", "年度关系报告", "全年消息", "</html>"} {
		if !strings.Contains(page, want) {
			t.Fatalf("长页缺少 %q", want)
		}
	}
	if !strings.Contains(page, "2025") {
		t.Fatal("长页应包含年份")
	}
	// 联系人名字里的 HTML 必须转义
	evil := regressionContact(t, db, "<img src=x onerror=alert(1)>")
	vaMsg(t, db, evil, "other", "嘿嘿", time.Date(year, 5, 5, 9, 0, 0, 0, time.Local))
	rep2, err := BuildAnnualReport(db, year)
	if err != nil {
		t.Fatal(err)
	}
	page2 := RenderReportHTML(rep2)
	if strings.Contains(page2, "<img src=x") {
		t.Fatal("长页未转义联系人名字，存在 XSS 风险")
	}
	if !strings.Contains(page2, "&lt;img") {
		t.Fatal("应看到转义后的联系人名字")
	}
}

func TestReportHelpers(t *testing.T) {
	if got := atoiSafe("abc"); got != 0 {
		t.Fatalf("atoiSafe 非法输入应返回 0，实际 %d", got)
	}
	if got := atoiSafe(" 42 "); got != 42 {
		t.Fatalf("atoiSafe 解析错误: %d", got)
	}
	if got := maxInt(3, 7); got != 7 {
		t.Fatalf("maxInt 错误: %d", got)
	}
	if got := maxInt(7, 3); got != 7 {
		t.Fatalf("maxInt 错误: %d", got)
	}
	if got := formatReportDate(""); got != "" {
		t.Fatalf("空串应原样返回，实际 %q", got)
	}
	if got := formatReportDate("2025-03-10T09:00:00+08:00"); got != "2025-03-10" {
		t.Fatalf("formatReportDate 错误: %q", got)
	}
}

// ---------- 零侵入：增值表缺失时既有路径不受影响 ----------

func TestValueAddedTablesMissingDoesNotBreakCore(t *testing.T) {
	// 只跑 InitDB，不建任何增值表，模拟老库刚升级上来的状态
	db := regressionDB(t)
	a := regressionContact(t, db, "张三")
	vaMsg(t, db, a, "other", "老库消息", time.Date(2026, 3, 10, 9, 0, 0, 0, time.Local))

	if _, err := ListTags(db); err == nil {
		t.Fatal("标签表不存在时 ListTags 应报错（由调用方降级），而不是 panic")
	}
	if _, err := ListFollowups(db, "open", 10); err == nil {
		t.Fatal("待跟进表不存在时应报错而不是 panic")
	}
	// 时间线是「派生节点 + 手动事件」的合集，手动事件表缺失时只降级掉手动部分，
	// 派生节点照常给出，绝不能报错让详情页整块打不开
	if items, err := GetContactTimeline(db, a, 10); err != nil {
		t.Fatalf("事件表不存在时时间线应降级而不是报错: %v", err)
	} else if len(items) == 0 {
		t.Fatal("事件表不存在时仍应给出派生节点")
	}
	// 只读的统计类功能不依赖增值表，必须照常工作
	if _, err := SearchMessages(db, SearchOptions{Query: "老库"}); err != nil {
		t.Fatalf("搜索不应依赖增值表: %v", err)
	}
	if _, err := ComputeSocialStats(db, 30); err != nil {
		t.Fatalf("社交大盘不应依赖增值表: %v", err)
	}
	if _, err := FindDuplicateContacts(db); err != nil {
		t.Fatalf("重复推荐不应依赖增值表: %v", err)
	}
	if _, err := BuildAnnualReport(db, 2026); err != nil {
		t.Fatalf("年度报告不应依赖增值表: %v", err)
	}
	if _, err := BuildCalendarICS(db, time.Now()); err != nil {
		t.Fatalf("日历订阅不应依赖增值表: %v", err)
	}
}
