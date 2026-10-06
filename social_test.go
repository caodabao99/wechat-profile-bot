package main

// social.go 的独立单元测试：统计/分词纯函数的边界与确定性，
// 以及 ComputeSocialStats 的 DB 聚合（消息量、活跃联系人、days 归一/封顶）。
// 全部确定性、不调模型、不改生产代码。

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAvgInt(t *testing.T) {
	if got := avgInt(nil); got != 0 {
		t.Errorf("空切片 avgInt 应为 0，got %v", got)
	}
	if got := avgInt([]int64{}); got != 0 {
		t.Errorf("空切片 avgInt 应为 0，got %v", got)
	}
	if got := avgInt([]int64{2, 4}); got != 3 {
		t.Errorf("avgInt([2,4])=%v，期望 3", got)
	}
	if got := avgInt([]int64{-2, 2}); got != 0 {
		t.Errorf("avgInt 含负值=%v，期望 0", got)
	}
	if got := avgInt([]int64{7}); got != 7 {
		t.Errorf("单元素 avgInt=%v，期望 7", got)
	}
}

func TestMedianInt(t *testing.T) {
	if got := medianInt(nil); got != 0 {
		t.Errorf("空切片 medianInt 应为 0，got %v", got)
	}
	if got := medianInt([]int64{5}); got != 5 {
		t.Errorf("单元素 medianInt=%v，期望 5", got)
	}
	// 奇数个：取中间值（输入乱序也应正确，函数内部复制后排序）
	if got := medianInt([]int64{3, 1, 2}); got != 2 {
		t.Errorf("奇数 medianInt=%v，期望 2", got)
	}
	// 偶数个：取中间两数均值
	if got := medianInt([]int64{4, 1, 2, 3}); got != 2.5 {
		t.Errorf("偶数 medianInt=%v，期望 2.5", got)
	}
	// 不修改入参顺序（内部是副本排序）
	in := []int64{9, 1, 5}
	_ = medianInt(in)
	if !reflect.DeepEqual(in, []int64{9, 1, 5}) {
		t.Errorf("medianInt 不应修改入参，got %v", in)
	}
}

func TestCjkRuns(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"hello你好世界,abc123!", []string{"hello你好世界", "abc123"}}, // 字母/数字/汉字都是 IsLetter/IsDigit，连成一段
		{"纯,汉,字", []string{"纯", "汉", "字"}},                     // 标点切开
		{"   ", nil},
		{"one  two", []string{"one", "two"}}, // 空格切开，数字字母归段
	}
	for _, c := range cases {
		got := cjkRuns(c.in)
		if len(got) != len(c.want) {
			t.Errorf("cjkRuns(%q)=%v，期望 %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("cjkRuns(%q)=%v，期望 %v", c.in, got, c.want)
				break
			}
		}
	}
}

func TestExtractPhrases(t *testing.T) {
	// 空输入
	if got := extractPhrases(nil, 5); len(got) != 0 {
		t.Errorf("空输入应返回空，got %v", got)
	}
	// 频次 <3 的片段不纳入；重复足够多次的短语应出现
	texts := []string{"哈哈哈", "哈哈哈", "哈哈哈", "加油鸭", "加油鸭", "加油鸭"}
	got := extractPhrases(texts, 5)
	if len(got) == 0 {
		t.Fatalf("高频短语未被抽出，got %v", got)
	}
	for _, p := range got {
		if p.Count < 3 {
			t.Errorf("出现频次 <3 的短语 %q count=%d", p.Phrase, p.Count)
		}
	}
	// 停用词应被过滤
	for _, sw := range []string{"为什么", "怎么样"} {
		for _, p := range got {
			if p.Phrase == sw {
				t.Errorf("停用词 %q 不应出现在结果里", sw)
			}
		}
	}
	// topN 上限
	if got2 := extractPhrases(texts, 1); len(got2) > 1 {
		t.Errorf("topN=1 却返回 %d 条", len(got2))
	}
	// 确定性：同输入两次得到同一集合（注：实现用 sort.Slice 且仅在
	// (频次,长度) 并列时才可能因 map 遍历序交换顺序，故断言“同一集合”而非“同序”）
	toSet := func(ps []PhraseStat) map[string]int {
		m := map[string]int{}
		for _, p := range ps {
			m[p.Phrase] = p.Count
		}
		return m
	}
	if a, b := toSet(extractPhrases(texts, 5)), toSet(extractPhrases(texts, 5)); !reflect.DeepEqual(a, b) {
		t.Errorf("extractPhrases 两次结果集合不一致：%v vs %v", a, b)
	}
}

func TestPhrasesFromProfiles(t *testing.T) {
	// 空 / 非法 JSON 优雅降级
	if got := phrasesFromProfiles(nil, 5); len(got) != 0 {
		t.Errorf("空输入应返回空，got %v", got)
	}
	if got := phrasesFromProfiles([]string{"{坏JSON", ""}, 5); len(got) != 0 {
		t.Errorf("非法/空 JSON 应被跳过，got %v", got)
	}
	pj := `{"communicationStyle":{"frequentPhrases":["么么哒","  ","超长超长的口头禅超过十二个字符的就不该被统计进来"]}}`
	got := phrasesFromProfiles([]string{pj, pj}, 5)
	if len(got) == 0 {
		t.Fatalf("应抽出画像里的口头禅，got %v", got)
	}
	found := false
	for _, p := range got {
		if p.Phrase == "么么哒" {
			found = true
			if p.Count != 2 { // 两份画像各出现一次 → 计数 2
				t.Errorf("么么哒 计数=%d，期望 2", p.Count)
			}
		}
		if len([]rune(p.Phrase)) > 12 {
			t.Errorf("超过 12 字的短语不应被纳入：%q", p.Phrase)
		}
		if strings.TrimSpace(p.Phrase) == "" {
			t.Error("空短语不应出现")
		}
	}
	if !found {
		t.Errorf("未抽出预期的口头禅：got %v", got)
	}
}

func TestComputeSocialStatsWindowAndAggregation(t *testing.T) {
	db := regressionDB(t)
	c1 := regressionContact(t, db, "甲")
	c2 := regressionContact(t, db, "乙")

	now := time.Now()
	// 全部落在默认 30 天窗口内
	seedMessage(t, db, c1, "me", "在吗", "h1", now.Add(-1*time.Hour))
	seedMessage(t, db, c1, "other", "在的呀", "h2", now.Add(-2*time.Hour))
	seedMessage(t, db, c2, "me", "好的", "h3", now.Add(-3*time.Hour))
	// 一条久远的（400 天前），默认 30 天窗口应排除
	seedMessage(t, db, c1, "other", "很久以前", "h4", now.AddDate(0, 0, -400))

	// days<=0 → 归一到 socialDefaultDays
	st, err := ComputeSocialStats(db, 0)
	if err != nil {
		t.Fatal(err)
	}
	if st.Days != socialDefaultDays {
		t.Errorf("days<=0 应归一为 %d，got %d", socialDefaultDays, st.Days)
	}
	// 3 条近期消息进入，400 天前的被排除
	if st.TotalMessages != 3 {
		t.Errorf("窗口内消息数=%d，期望 3（久远消息应被 days 窗口排除）", st.TotalMessages)
	}
	if st.MyMessages != 2 || st.TheirMessages != 1 {
		t.Errorf("me/other 分布错误：mine=%d theirs=%d，期望 2/1", st.MyMessages, st.TheirMessages)
	}
	if st.ActiveContacts != 2 {
		t.Errorf("活跃联系人数=%d，期望 2", st.ActiveContacts)
	}
	if len(st.Hourly) != 24 || len(st.Weekday) != 7 {
		t.Errorf("Hourly/Weekday 维度长度错误：%d/%d", len(st.Hourly), len(st.Weekday))
	}

	// days 超过上限 → 封顶，并纳入那条久远消息
	stBig, err := ComputeSocialStats(db, socialMaxDays+9999)
	if err != nil {
		t.Fatal(err)
	}
	if stBig.Days != socialMaxDays {
		t.Errorf("days 应封顶为 %d，got %d", socialMaxDays, stBig.Days)
	}
	if stBig.TotalMessages != 4 {
		t.Errorf("放大窗口后应含全部 4 条，got %d", stBig.TotalMessages)
	}

	// 空库零值不报错
	empty := regressionDB(t)
	stEmpty, err := ComputeSocialStats(empty, 30)
	if err != nil {
		t.Fatal(err)
	}
	if stEmpty.TotalMessages != 0 || stEmpty.ActiveContacts != 0 {
		t.Errorf("空库应为零值，got total=%d contacts=%d", stEmpty.TotalMessages, stEmpty.ActiveContacts)
	}
}
