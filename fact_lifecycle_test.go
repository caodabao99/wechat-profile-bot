package main

// OS 2.0 Phase 2（Temporal Memory / FACT LIFECYCLE）专项测试。
// 覆盖：5.4 时效取代 supersede、5.5 用户确认最高优先级、5.6 valid_from 时效、5.2 证据评估字段。
// 与既有 facts_test.go / phase4_9_test.go 的 active/retired 及 confidence 数值公式并存，不改变其行为。

import (
	"database/sql"
	"testing"
)

// 5.4：单值型事实换值 → 旧值被取代（superseded）并回填 superseded_by / valid_until；
// 当前态视图只含新值，历史视图含两者。
func TestFactSupersedeOnValueChange(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "变更对象")
	if err := SaveProfile(db, cid, `{"basic_info":{"occupation":"医生"}}`, "x", "i"); err != nil {
		t.Fatal(err)
	}
	if err := SaveProfile(db, cid, `{"basic_info":{"occupation":"律师"}}`, "y", "i"); err != nil {
		t.Fatal(err)
	}

	var docStatus, docValidUntil string
	var supBy sql.NullInt64
	if err := db.QueryRow(`SELECT status, valid_until, superseded_by FROM profile_facts WHERE contact_id=? AND fact_value='医生'`, cid).
		Scan(&docStatus, &docValidUntil, &supBy); err != nil {
		t.Fatal(err)
	}
	if docStatus != "superseded" {
		t.Fatalf("旧职业应被取代为 superseded, got %s", docStatus)
	}
	if docValidUntil == "" {
		t.Error("superseded 事实应写入 valid_until 失效时间")
	}
	if !supBy.Valid {
		t.Fatal("superseded_by 应回填指向新事实")
	}
	var lawyerID int64
	if err := db.QueryRow(`SELECT id FROM profile_facts WHERE contact_id=? AND fact_value='律师'`, cid).Scan(&lawyerID); err != nil {
		t.Fatal(err)
	}
	if supBy.Int64 != lawyerID {
		t.Errorf("superseded_by=%d 应指向律师 %d", supBy.Int64, lawyerID)
	}

	cur, err := GetFacts(db, cid, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range cur {
		if f.Value == "医生" {
			t.Errorf("当前态视图不应含被取代的旧值：%+v", f)
		}
	}
	var sawLawyer bool
	for _, f := range cur {
		if f.Value == "律师" && f.Status == "active" {
			sawLawyer = true
		}
	}
	if !sawLawyer {
		t.Error("当前态视图应含新值律师")
	}
	all, err := GetFacts(db, cid, true)
	if err != nil {
		t.Fatal(err)
	}
	var sawDoc bool
	for _, f := range all {
		if f.Value == "医生" && f.Status == "superseded" {
			sawDoc = true
		}
	}
	if !sawDoc {
		t.Error("历史视图应含 superseded 的医生")
	}
}

// 5.4 边界：多值集合型事实（兴趣）的移除属「消失」而非「取代」，应保持 retired。
func TestFactRemovedSetStaysRetired(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "兴趣集合")
	if err := SaveProfile(db, cid, `{"interests":["跑步","游泳"]}`, "x", "i"); err != nil {
		t.Fatal(err)
	}
	if err := SaveProfile(db, cid, `{"interests":["跑步"]}`, "y", "i"); err != nil {
		t.Fatal(err)
	}
	var st string
	if err := db.QueryRow(`SELECT status FROM profile_facts WHERE contact_id=? AND fact_value='游泳'`, cid).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != "retired" {
		t.Errorf("移除的兴趣应保持 retired（非 superseded）, got %s", st)
	}
}

// 5.5：用户确认事实优先级最高，后续派生既不降级也不改写，且始终出现在当前态视图。
func TestFactUserConfirmationWins(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "确认对象")
	if err := SaveProfile(db, cid, `{"interests":["咖啡"]}`, "x", "i"); err != nil {
		t.Fatal(err)
	}
	fs, err := GetFacts(db, cid, false)
	if err != nil {
		t.Fatal(err)
	}
	var coffeeID int64
	for _, f := range fs {
		if f.Value == "咖啡" {
			coffeeID = f.ID
		}
	}
	if coffeeID == 0 {
		t.Fatal("未找到咖啡事实")
	}
	if err := ConfirmFact(db, coffeeID); err != nil {
		t.Fatal(err)
	}
	// 画像改口：把咖啡换成茶，用户确认的咖啡不应被自动降级
	if err := SaveProfile(db, cid, `{"interests":["茶"]}`, "y", "i"); err != nil {
		t.Fatal(err)
	}
	var status, sourceType, confType string
	var conf float64
	if err := db.QueryRow(`SELECT status, source_type, confidence_type, confidence FROM profile_facts WHERE id=?`, coffeeID).
		Scan(&status, &sourceType, &confType, &conf); err != nil {
		t.Fatal(err)
	}
	if status != "confirmed" || sourceType != "user" || confType != "user_confirmed" {
		t.Errorf("用户确认事实应受保护：status=%s source=%s ctype=%s", status, sourceType, confType)
	}
	if conf != 1.0 {
		t.Errorf("用户确认事实置信度应为 1.0, got %f", conf)
	}
	cur, err := GetFacts(db, cid, false)
	if err != nil {
		t.Fatal(err)
	}
	var sawCoffee bool
	for _, f := range cur {
		if f.Value == "咖啡" && f.Status == "confirmed" {
			sawCoffee = true
		}
	}
	if !sawCoffee {
		t.Error("用户确认事实应出现在当前态视图（GetFacts includeRetired=false）")
	}
	// ConfirmFact 对不存在的 id 应返回 sql.ErrNoRows
	if err := ConfirmFact(db, 999999); err != sql.ErrNoRows {
		t.Errorf("对不存在事实应返回 ErrNoRows, got %v", err)
	}
}

// 5.6：active 事实应带 valid_from 生效时间。
func TestFactValidFromPopulated(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "时效对象")
	if err := SaveProfile(db, cid, `{"basic_info":{"location":"深圳"}}`, "x", "i"); err != nil {
		t.Fatal(err)
	}
	fs, err := GetFacts(db, cid, false)
	if err != nil {
		t.Fatal(err)
	}
	var ok bool
	for _, f := range fs {
		if f.Value == "深圳" {
			if f.ValidFrom == "" {
				t.Error("active 事实应写 valid_from")
			}
			if f.SourceType != "ai" {
				t.Errorf("派生事实 source_type 应为 ai, got %s", f.SourceType)
			}
			ok = true
		}
	}
	if !ok {
		t.Fatal("未找到深圳事实")
	}
}

// 5.2 + 向后兼容：证据带 match_type/is_direct_support/support_strength，事实带 confidence_type/evidence_strength；
// 且数值 confidence 仍沿用旧公式（3 条直接证据 → ≈0.84），与 phase4_9_test.go 锁定的区间一致。
func TestFactEvidenceAssessment(t *testing.T) {
	db := regressionDB(t)
	cid := regressionContact(t, db, "证据评估")
	if err := SaveProfile(db, cid, `{"interests":["跑步"],"summary":"喜欢跑步"}`, "x", "i"); err != nil {
		t.Fatal(err)
	}
	regressionMessages(t, db, cid, "今天去跑步了", "周末也跑步", "一起跑步吗", "吃饭了吗")
	if _, _, err := RebuildFactsAndEvidence(db, cid); err != nil {
		t.Fatal(err)
	}
	fs, err := GetFacts(db, cid, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if f.Type == "interest" && f.Value == "跑步" {
			if len(f.Evidence) != 3 {
				t.Fatalf("应挂 3 条证据, got %d", len(f.Evidence))
			}
			if f.Confidence < 0.8 || f.Confidence > 0.85 {
				t.Fatalf("数值置信度应≈0.84（公式向后兼容）, got %f", f.Confidence)
			}
			if f.ConfidenceType != "multi_evidence" {
				t.Errorf("≥2 直接证据应为 multi_evidence, got %s", f.ConfidenceType)
			}
			if f.EvidenceStrength != 1.0 {
				t.Errorf("全部直接支撑 → evidence_strength 应为 1.0, got %f", f.EvidenceStrength)
			}
			for _, ev := range f.Evidence {
				if ev.MatchType != "exact" || !ev.IsDirectSupport {
					t.Errorf("证据应判定为 exact 直接支撑：%+v", ev)
				}
				if ev.MessageID == 0 || ev.Snippet == "" {
					t.Fatal("证据缺少 messageId 或 snippet")
				}
			}
			return
		}
	}
	t.Fatal("未找到跑步事实")
}
