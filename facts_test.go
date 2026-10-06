package main

// facts.go 的独立单元测试：deriveFacts 画像解析（易错），boolToInt、tableExistsLocked，
// 以及 ExtractProfileFacts/GetFacts 的 active/retired 生命周期与幂等。
// 全部确定性、不调模型、不改生产代码。

import (
	"reflect"
	"testing"
)

func TestBoolToInt(t *testing.T) {
	if boolToInt(true) != 1 || boolToInt(false) != 0 {
		t.Errorf("boolToInt 错误：true=%d false=%d", boolToInt(true), boolToInt(false))
	}
}

func TestTableExistsLocked(t *testing.T) {
	db := regressionDB(t)
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "profile_facts") {
		t.Error("已建表 profile_facts 应被识别为存在")
	}
	if tableExistsLocked(db, "no_such_table_xyz") {
		t.Error("不存在的表却被识别为存在")
	}
}

func TestDeriveFacts(t *testing.T) {
	// 空 / 非法 JSON → 空集且不 panic
	if got := deriveFacts(""); got != nil {
		t.Errorf("空画像应返回 nil，got %v", got)
	}
	if got := deriveFacts("{坏JSON"); got != nil {
		t.Errorf("非法 JSON 应返回 nil，got %v", got)
	}
	if got := deriveFacts("{}"); len(got) != 0 {
		t.Errorf("空对象应返回空集，got %v", got)
	}

	pj := `{
		"basic_info":{"occupation":"教师","location":"北京","important_dates":["生日: 5月20日","结婚纪念日"]},
		"personality":["开朗"],
		"interests":["篮球"],
		"important_facts":["养了一只猫"],
		"communication_style":{"frequent_phrases":["么么哒"]},
		"relationship":{"closeness":"很亲密"}
	}`
	facts := deriveFacts(pj)

	find := func(typ, key string) (profileFact, bool) {
		for _, f := range facts {
			if f.Type == typ && f.Key == key {
				return f, true
			}
		}
		return profileFact{}, false
	}
	// 职业/城市
	if f, ok := find("occupation", ""); !ok || f.Value != "教师" {
		t.Errorf("未正确抽出职业：got %+v ok=%v", f, ok)
	}
	if f, ok := find("location", ""); !ok || f.Value != "北京" {
		t.Errorf("未正确抽出城市：got %+v ok=%v", f, ok)
	}
	// "生日: 5月20日" 应按冒号拆 key/value
	if f, ok := find("important_date", "生日"); !ok || f.Value != "5月20日" {
		t.Errorf("带冒号的重要日子拆分错误：got %+v ok=%v", f, ok)
	}
	// 无冒号的整串当 value，key 为空
	if f, ok := find("important_date", ""); !ok || f.Value != "结婚纪念日" {
		t.Errorf("无冒号的重要日子处理错误：got %+v ok=%v", f, ok)
	}
	// 性格/兴趣/重要事实/口头禅/亲密度
	for _, tc := range []struct{ typ, val string }{
		{"personality", "开朗"},
		{"interest", "篮球"},
		{"important_fact", "养了一只猫"},
		{"phrase", "么么哒"},
		{"closeness", "很亲密"},
	} {
		found := false
		for _, f := range facts {
			if f.Type == tc.typ && f.Value == tc.val {
				found = true
			}
		}
		if !found {
			t.Errorf("未抽出 %s=%s", tc.typ, tc.val)
		}
	}
	// 值会被 TrimSpace，纯空白项不纳入
	if f, ok := find("location", ""); ok && f.Value != "北京" {
		t.Errorf("location 值应已去空白：got %q", f.Value)
	}

	// 确定性：同一份画像两次派生结果全等
	if !reflect.DeepEqual(facts, deriveFacts(pj)) {
		t.Error("deriveFacts 对同一输入不确定")
	}
}

func TestExtractProfileFactsLifecycle(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "事实测试对象")

	setProfile := func(pj string) {
		t.Helper()
		if _, err := db.Exec(`UPDATE contacts SET profile_json=? WHERE id=?`, pj, cid); err != nil {
			t.Fatal(err)
		}
	}

	// 初始画像含职业 + 城市两条事实
	setProfile(`{"basic_info":{"occupation":"医生","location":"上海"}}`)
	active, _, err := ExtractProfileFacts(db, cid)
	if err != nil {
		t.Fatal(err)
	}
	if active != 2 {
		t.Errorf("初次抽取 active=%d，期望 2", active)
	}
	got, err := GetFacts(db, cid, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("GetFacts(active) 长度=%d，期望 2", len(got))
	}
	for _, f := range got {
		if f.Status != "active" {
			t.Errorf("includeRetired=false 却返回非 active 事实：%+v", f)
		}
	}

	// 幂等：同画像再抽一次，active 数不变、无重复堆积
	active2, _, err := ExtractProfileFacts(db, cid)
	if err != nil {
		t.Fatal(err)
	}
	if active2 != 2 {
		t.Errorf("二次抽取 active=%d，期望仍为 2（幂等无重复）", active2)
	}

	// 画像删掉城市 → 该事实转 retired，职业留 active
	setProfile(`{"basic_info":{"occupation":"医生"}}`)
	active3, retired3, err := ExtractProfileFacts(db, cid)
	if err != nil {
		t.Fatal(err)
	}
	if active3 != 1 || retired3 != 1 {
		t.Errorf("删项后 active=%d retired=%d，期望 1/1", active3, retired3)
	}
	only, err := GetFacts(db, cid, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 || only[0].Value != "医生" {
		t.Errorf("includeRetired=false 应只剩职业事实，got %+v", only)
	}
	all, err := GetFacts(db, cid, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("includeRetired=true 应含 retired，期望 2 条，got %d", len(all))
	}
	var sawRetired bool
	for _, f := range all {
		if f.Status == "retired" && f.Value == "上海" {
			sawRetired = true
		}
	}
	if !sawRetired {
		t.Error("未在 includeRetired=true 结果中找到 retired 的上海事实")
	}
}

func TestGetFactsEmptyContact(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "无事实对象")
	got, err := GetFacts(db, cid, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("无事实联系人应返回空切片，got %d 条", len(got))
	}
}
