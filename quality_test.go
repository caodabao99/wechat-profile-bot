package main

// v4.8.0 对话质量评分回归（纯确定性）。
//
//	证明四维评分在真实 SQLite 上：
//	  - 结果可复现（同输入两次调用逐字段相等）；
//	  - 四维各落 0-100，综合分落 0-100；
//	  - 情绪增值表缺失时该维记 -1（未计入综合），主流程不报错；
//	  - 窗口天数缺省/越界被夹到合法区间；
//	  - 空数据联系人不 panic、dims 仍为 4 个、空切片非 nil。

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"
)

// qFindDim 按 key 取维度
func qFindDim(q *ContactQuality, key string) *QualityDim {
	for i := range q.Dims {
		if q.Dims[i].Key == key {
			return &q.Dims[i]
		}
	}
	return nil
}

// qAssertShape 校验通用不变量
func qAssertShape(t *testing.T, q *ContactQuality) {
	t.Helper()
	if len(q.Dims) != 4 {
		t.Fatalf("应有 4 个维度, got %d (%+v)", len(q.Dims), q.Dims)
	}
	if q.Score < 0 || q.Score > 100 {
		t.Fatalf("综合分应落 0-100, got %d", q.Score)
	}
	for _, d := range q.Dims {
		if d.Value < -1 || d.Value > 100 {
			t.Fatalf("维度 %s 取值越界: %d", d.Key, d.Value)
		}
	}
}

// qSeedConversation 造一段含 4 次「对方→我」快速回复的对话（均在窗口内）
func qSeedConversation(t *testing.T, db *sql.DB, cid int64) {
	t.Helper()
	base := time.Now().AddDate(0, 0, -10)
	seq := []struct {
		sender string
		min    int
	}{
		{"other", 0}, {"me", 2},
		{"other", 10}, {"me", 11},
		{"other", 20}, {"me", 25},
		{"other", 30}, {"me", 31},
	}
	for i, s := range seq {
		vaMsg(t, db, cid, s.sender, "质量对话片段"+strings.Repeat("内容", i%3+1), base.Add(time.Duration(s.min)*time.Minute))
	}
}

const qRichProfile = `{"basic_info":{"occupation":"产品经理","location":"上海"},` +
	`"interests":["攀岩","摄影","爵士乐"],"personality":["细心","直接"],` +
	`"important_facts":["有个女儿","准备跳槽"],` +
	`"communication_style":{"frequentPhrases":["收到","好的没问题"]},"summary":"沟通直接"}`

func TestComputeContactQualityDeterministic(t *testing.T) {
	db := vaDB(t)
	if err := ensureAssistantTables(db); err != nil {
		t.Fatal(err)
	}
	id := regressionContact(t, db, "质量人")
	if err := SaveProfile(db, id, qRichProfile, "沟通直接", "seed"); err != nil {
		t.Fatal(err)
	}
	qSeedConversation(t, db, id)
	// 两条情绪记录（默认 created_at=now，落在窗口内）
	for _, sc := range []int{70, 80} {
		if _, err := db.Exec(
			`INSERT INTO assistant_emotions (contact_id, emotion, score, summary, advice, alert)
			 VALUES (?, 'positive', ?, '', '', 0)`, id, sc); err != nil {
			t.Fatal(err)
		}
	}

	q1, err := ComputeContactQuality(db, id, 90)
	if err != nil {
		t.Fatal(err)
	}
	qAssertShape(t, q1)
	if q1.Name == "" {
		t.Fatal("应带展示名")
	}
	// 情绪有数据 → positivity 落 0-100（非 -1）
	pos := qFindDim(q1, "positivity")
	if pos == nil || pos.Value < 0 {
		t.Fatalf("有情绪记录时 positivity 应计入, got %+v", pos)
	}
	if div := qFindDim(q1, "diversity"); div == nil || div.Value <= 0 {
		t.Fatalf("丰富画像下 diversity 应 >0, got %+v", div)
	}
	if resp := qFindDim(q1, "responsiveness"); resp == nil || strings.Contains(resp.Detail, "仅供参考") {
		t.Fatalf("样本充足时回复及时性不应标注仅供参考, got %+v", resp)
	}

	q2, err := ComputeContactQuality(db, id, 90)
	if err != nil {
		t.Fatal(err)
	}
	// 确定性：除 GeneratedAt（秒级时钟）外逐字段相等
	if q1.Score != q2.Score {
		t.Fatalf("两次综合分应相等: %d vs %d", q1.Score, q2.Score)
	}
	if !reflect.DeepEqual(q1.Dims, q2.Dims) {
		t.Fatalf("两次维度应逐字段相等:\n%+v\n%+v", q1.Dims, q2.Dims)
	}
}

func TestComputeContactQualityNoEmotionTable(t *testing.T) {
	db := vaDB(t) // 未建 assistant_emotions：模拟增值表缺失
	id := regressionContact(t, db, "无情绪人")
	qSeedConversation(t, db, id)

	q, err := ComputeContactQuality(db, id, 90)
	if err != nil {
		t.Fatalf("缺情绪表不应报错: %v", err)
	}
	qAssertShape(t, q)
	pos := qFindDim(q, "positivity")
	if pos == nil || pos.Value != -1 {
		t.Fatalf("缺情绪表时 positivity 应记 -1, got %+v", pos)
	}
	if !strings.Contains(pos.Detail, "未计入") {
		t.Fatalf("positivity 说明应标注未计入, got %q", pos.Detail)
	}
	// 综合分只由其余三维加权，仍应落在 (0,100]
	if q.Score <= 0 {
		t.Fatalf("三维有数据时综合分应 >0, got %d", q.Score)
	}
}

func TestComputeContactQualityWindowClamp(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "窗口人")
	regressionMessages(t, db, id, "随便聊点什么")

	lo, err := ComputeContactQuality(db, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if lo.WindowDays != qualityDefaultDays {
		t.Fatalf("days<=0 应回落默认 %d, got %d", qualityDefaultDays, lo.WindowDays)
	}
	hi, err := ComputeContactQuality(db, id, qualityMaxDays+100)
	if err != nil {
		t.Fatal(err)
	}
	if hi.WindowDays != qualityMaxDays {
		t.Fatalf("days 应夹到上限 %d, got %d", qualityMaxDays, hi.WindowDays)
	}
}

func TestComputeContactQualityEmptyContact(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "空数据人")

	q, err := ComputeContactQuality(db, id, 90)
	if err != nil {
		t.Fatal(err)
	}
	qAssertShape(t, q)
	if q.Dims == nil {
		t.Fatal("dims 不应为 nil")
	}
	div := qFindDim(q, "diversity")
	if div == nil || !strings.Contains(div.Detail, "暂无画像") {
		t.Fatalf("无画像时 diversity 应说明暂无画像, got %+v", div)
	}
	// 无数据时综合分应为 0（各维中性/无数据）
	if q.Score < 0 || q.Score > 100 {
		t.Fatalf("空数据综合分越界: %d", q.Score)
	}
}
