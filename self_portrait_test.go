package main

// 高阶洞察 · Phase B 自我关系画像回归。
// 覆盖：先开口率(全局/类别)、单向关系计数、投入失衡均值、精力迁移、
//       社交风格标签规则、空数据稳健、整层重算 + 缓存幂等。

import (
	"fmt"
	"testing"
	"time"
)

// rawFixture 构造一份最小 lifeRaw，只填 buildSelfPortrait 会读到的字段。
func rawFixture(active map[int64]bool, lifeCount, mine30, other30 map[int64]int,
	category map[int64]string, catLife map[string]int) *lifeRaw {
	return &lifeRaw{
		active: active, lifeCount: lifeCount, mine30: mine30, other30: other30,
		category: category, catLife: catLife, catRecent: map[string]int{},
	}
}

func hasStyleTag(tags []StyleTag, key string) bool {
	for _, t := range tags {
		if t.Key == key {
			return true
		}
	}
	return false
}

func TestBuildSelfPortraitRatesAndOneWay(t *testing.T) {
	now := time.Now()
	raw := rawFixture(
		map[int64]bool{1: true},
		map[int64]int{1: 10},
		map[int64]int{1: 9}, // 我发 9
		map[int64]int{1: 1}, // 对方发 1
		map[int64]string{1: "朋友"},
		map[string]int{"家人": 50, "朋友": 10},
	)
	sp := buildSelfPortrait(now, raw)

	if got := sp.InitiationRate; got != 90 {
		t.Errorf("你先开口率应为 90, got %v", got)
	}
	if got := sp.CategoryInitiation["朋友"]; got != 90 {
		t.Errorf("朋友类先开口率应为 90, got %v", got)
	}
	if sp.OneWayCount != 1 {
		t.Errorf("9:1 单向应计 1 段, got %d", sp.OneWayCount)
	}
	if sp.ImbalanceAvg != 80 {
		t.Errorf("平均失衡度应为 80, got %v", sp.ImbalanceAvg)
	}
	if sp.ActiveContacts != 1 {
		t.Errorf("近期往来人数应为 1, got %d", sp.ActiveContacts)
	}
	if sp.EnergyShares["朋友"] != 100 {
		t.Errorf("近期精力应全在朋友, got %v", sp.EnergyShares)
	}
	// 精力迁移：朋友涨、家人跌都应被识别
	if len(sp.Migration) == 0 {
		t.Error("应产出精力迁移洞察")
	}
	// 风格标签：主动 + 深耕 + 偏向朋友
	for _, k := range []string{"proactive", "deep", "lean"} {
		if !hasStyleTag(sp.StyleTags, k) {
			t.Errorf("应有 %s 风格标签, got %+v", k, sp.StyleTags)
		}
	}
}

func TestBuildSelfPortraitPassiveTag(t *testing.T) {
	now := time.Now()
	raw := rawFixture(
		map[int64]bool{2: true},
		map[int64]int{2: 10},
		map[int64]int{2: 2}, // 我发很少
		map[int64]int{2: 8}, // 对方主导
		map[int64]string{2: "同事"},
		map[string]int{"同事": 10},
	)
	sp := buildSelfPortrait(now, raw)
	if sp.InitiationRate != 20 {
		t.Errorf("先开口率应为 20, got %v", sp.InitiationRate)
	}
	if !hasStyleTag(sp.StyleTags, "passive") {
		t.Errorf("低先开口率应打被动型标签, got %+v", sp.StyleTags)
	}
	// 对方主导、且我发 <5 → 不计单向
	if sp.OneWayCount != 0 {
		t.Errorf("被动不应计单向, got %d", sp.OneWayCount)
	}
}

func TestBuildSelfPortraitSkipsNeverChatted(t *testing.T) {
	now := time.Now()
	raw := rawFixture(
		map[int64]bool{1: true, 2: true},
		map[int64]int{1: 0, 2: 4}, // 1 从没聊过 → 跳过
		map[int64]int{1: 0, 2: 2},
		map[int64]int{1: 0, 2: 2},
		map[int64]string{1: "朋友", 2: "朋友"},
		map[string]int{},
	)
	sp := buildSelfPortrait(now, raw)
	if sp.ActiveContacts != 1 {
		t.Errorf("只应统计聊过天的联系人, got %d", sp.ActiveContacts)
	}
	// 2:2 完全均衡 → 先开口 50，不计单向
	if sp.InitiationRate != 50 {
		t.Errorf("均衡应为 50, got %v", sp.InitiationRate)
	}
	if sp.OneWayCount != 0 {
		t.Errorf("均衡往来不该计单向, got %d", sp.OneWayCount)
	}
}

func TestBuildSelfPortraitEmptyRobust(t *testing.T) {
	now := time.Now()
	sp := buildSelfPortrait(now, rawFixture(map[int64]bool{}, nil, nil, nil, nil, nil))
	if sp == nil {
		t.Fatal("不应返回 nil")
	}
	if sp.InitiationRate != 0 || sp.ActiveContacts != 0 || sp.OneWayCount != 0 {
		t.Errorf("空数据应全零, got %+v", sp)
	}
	if len(sp.StyleTags) != 0 || len(sp.Insights) != 0 {
		t.Errorf("空数据不应有标签/洞察, got %+v", sp)
	}
}

func TestComputeSelfPortraitIntegration(t *testing.T) {
	db := regressionAssistantDB(t)
	now := time.Now()
	if err := ComputeSelfPortrait(db, now); err != nil {
		t.Fatal(err)
	}
	sp, genAt, err := GetCachedSelfPortrait(db)
	if err != nil || sp == nil {
		t.Fatalf("应读回缓存: sp=%v err=%v", sp, err)
	}
	if IsSelfPortraitStale(genAt, now) {
		t.Error("刚生成不应过期")
	}
	if !IsSelfPortraitStale(genAt, now.Add(8*24*time.Hour)) {
		t.Error("8 天后应过期")
	}
}

func TestComputeSelfPortraitEndToEndWithMetrics(t *testing.T) {
	db := regressionAssistantDB(t)
	cid := regressionContact(t, db, "自我画像对象")
	now := time.Now()
	// 近 30 天内种一份明显"我主动"的往来（mine30/other30 来自消息，非度量表）
	for i := 0; i < 8; i++ {
		seedMessage(t, db, cid, "me", "在吗", fmt.Sprintf("sp-me-%d", i), now.AddDate(0, 0, -2))
	}
	seedMessage(t, db, cid, "other", "在的", "sp-other-0", now.AddDate(0, 0, -2))
	if err := ComputeSelfPortrait(db, now); err != nil {
		t.Fatal(err)
	}
	sp, _, err := GetCachedSelfPortrait(db)
	if err != nil || sp == nil {
		t.Fatal(err)
	}
	if sp.ActiveContacts < 1 {
		t.Errorf("有往来数据应识别出活跃联系人, got %d", sp.ActiveContacts)
	}
	if sp.InitiationRate < 65 {
		t.Errorf("8:1 我主动应先开口率应偏高, got %v", sp.InitiationRate)
	}
}
