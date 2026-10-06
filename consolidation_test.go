package main

// v6.3 §P5 Memory Consolidation 验收。
//
// 钉死：
//  1. 冲突检测：同一联系人单值型事实有 >1 active 时，被识别并给出建议。
//  2. 陈旧检测：last_seen 超期的 active 非用户事实被识别。
//  3. 近重复检测：同 (type,key) 下 "跑步" vs "喜欢跑步" 被标为 near_dup。
//  4. ApplyConsolidation：保留 keepID，其余变 superseded。
//  5. MarkFactsStale：变 stale。
//  6. isNearDuplicate 单元测试：子串/大 Jaccard/小 Jaccard 路径。

import (
	"testing"
	"time"
)

func TestIsNearDuplicate(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"跑步", "跑步", true},
		{"跑步", "喜欢跑步", true},     // 子串
		{"篮球", "打篮球", true},      // 子串
		{"北京", "上海", false},      // 完全不同
		{"供应链经理", "物流经理", false}, // 部分重叠但 Jaccard 低
		{"", "", true},
		{"a", "", true}, // 空串被 TrimSpace 后 Contains 命中
	}
	for _, c := range cases {
		got := isNearDuplicate(c.a, c.b)
		if got != c.want {
			t.Errorf("isNearDuplicate(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestDetectConflict(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "冲突对象")
	now := time.Now().Format(time.RFC3339)

	// 手动插入两条 active occupation（绕过 deriveFacts 的 UNIQUE 约束——不同 value）
	dbMu.Lock()
	db.Exec(`INSERT INTO profile_facts (contact_id, fact_type, fact_key, fact_value, status, confidence, source_type, last_seen, first_seen, updated_at, created_at)
		VALUES (?, 'occupation', '', '律师', 'active', 0.9, 'user', ?, ?, ?, ?)`, id, now, now, now, now)
	db.Exec(`INSERT INTO profile_facts (contact_id, fact_type, fact_key, fact_value, status, confidence, source_type, last_seen, first_seen, updated_at, created_at)
		VALUES (?, 'occupation', '', '创业者', 'active', 0.6, 'ai', ?, ?, ?, ?)`, id, now, now, now, now)
	dbMu.Unlock()

	p, err := BuildConsolidationProposal(db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if p.Summary.Conflicts == 0 {
		t.Fatalf("应检测到至少 1 条 occupation 冲突，实得 %d", p.Summary.Conflicts)
	}
	// 找到对应 item
	var found bool
	for _, item := range p.Items {
		if item.Kind == KindConflict && item.ContactID == id && item.FactType == "occupation" {
			found = true
			if len(item.Facts) < 2 {
				t.Fatalf("冲突 item 应含 ≥2 条事实，实得 %d", len(item.Facts))
			}
		}
	}
	if !found {
		t.Fatal("未检测到 occupation 冲突")
	}
}

func TestDetectStale(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "陈旧对象")
	old := time.Now().AddDate(0, 0, -(staleThresholdDays + 10)).Format(time.RFC3339)
	now := time.Now().Format(time.RFC3339)

	dbMu.Lock()
	db.Exec(`INSERT INTO profile_facts (contact_id, fact_type, fact_key, fact_value, status, confidence, source_type, last_seen, first_seen, updated_at, created_at)
		VALUES (?, 'interest', '', '围棋', 'active', 0.6, 'ai', ?, ?, ?, ?)`, id, old, old, now, now)
	dbMu.Unlock()

	p, err := BuildConsolidationProposal(db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if p.Summary.Stales == 0 {
		t.Fatalf("应检测到至少 1 条 stale interest，实得 %d", p.Summary.Stales)
	}
	var found bool
	for _, item := range p.Items {
		if item.Kind == KindStale && item.ContactID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("未检测到 stale 事实")
	}
}

func TestDetectNearDuplicate(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "重复对象")
	now := time.Now().Format(time.RFC3339)

	dbMu.Lock()
	db.Exec(`INSERT INTO profile_facts (contact_id, fact_type, fact_key, fact_value, status, confidence, source_type, last_seen, first_seen, updated_at, created_at)
		VALUES (?, 'interest', '', '跑步', 'active', 0.7, 'ai', ?, ?, ?, ?)`, id, now, now, now, now)
	db.Exec(`INSERT INTO profile_facts (contact_id, fact_type, fact_key, fact_value, status, confidence, source_type, last_seen, first_seen, updated_at, created_at)
		VALUES (?, 'interest', '', '喜欢跑步', 'active', 0.6, 'ai', ?, ?, ?, ?)`, id, now, now, now, now)
	dbMu.Unlock()

	p, err := BuildConsolidationProposal(db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if p.Summary.Duplicates == 0 {
		t.Fatalf("应检测到 interest '跑步' ≈ '喜欢跑步' 近重复，实得 duplicates=%d", p.Summary.Duplicates)
	}
	var found bool
	for _, item := range p.Items {
		if item.Kind == KindDuplicate && item.ContactID == id && item.FactType == "interest" {
			found = true
		}
	}
	if !found {
		t.Fatal("未检测到 interest 近重复")
	}
}

func TestApplyConsolidationSupersede(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "执行对象")
	now := time.Now().Format(time.RFC3339)

	dbMu.Lock()
	res1, _ := db.Exec(`INSERT INTO profile_facts (contact_id, fact_type, fact_key, fact_value, status, confidence, source_type, last_seen, first_seen, updated_at, created_at)
		VALUES (?, 'interest', '', '阅读', 'active', 0.8, 'ai', ?, ?, ?, ?)`, id, now, now, now, now)
	res2, _ := db.Exec(`INSERT INTO profile_facts (contact_id, fact_type, fact_key, fact_value, status, confidence, source_type, last_seen, first_seen, updated_at, created_at)
		VALUES (?, 'interest', '', '看书', 'active', 0.6, 'ai', ?, ?, ?, ?)`, id, now, now, now, now)
	dbMu.Unlock()
	keepID, _ := res1.LastInsertId()
	dropID, _ := res2.LastInsertId()

	if err := ApplyConsolidation(db, []int64{keepID, dropID}, keepID); err != nil {
		t.Fatal(err)
	}
	// keepID 仍 active
	var status string
	dbMu.Lock()
	db.QueryRow(`SELECT status FROM profile_facts WHERE id=?`, keepID).Scan(&status)
	dbMu.Unlock()
	if status != "active" {
		t.Fatalf("keepID status=%q want active", status)
	}
	// dropID 变 superseded
	dbMu.Lock()
	db.QueryRow(`SELECT status FROM profile_facts WHERE id=?`, dropID).Scan(&status)
	dbMu.Unlock()
	if status != "superseded" {
		t.Fatalf("dropID status=%q want superseded", status)
	}
}

func TestMarkFactsStale(t *testing.T) {
	db := regressionDB(t)
	id := regressionContact(t, db, "stale执行")
	now := time.Now().Format(time.RFC3339)

	dbMu.Lock()
	res, _ := db.Exec(`INSERT INTO profile_facts (contact_id, fact_type, fact_key, fact_value, status, confidence, source_type, last_seen, first_seen, updated_at, created_at)
		VALUES (?, 'personality', '', '开朗', 'active', 0.6, 'ai', ?, ?, ?, ?)`, id, now, now, now, now)
	dbMu.Unlock()
	fid, _ := res.LastInsertId()

	if err := MarkFactsStale(db, []int64{fid}); err != nil {
		t.Fatal(err)
	}
	var status string
	dbMu.Lock()
	db.QueryRow(`SELECT status FROM profile_facts WHERE id=?`, fid).Scan(&status)
	dbMu.Unlock()
	if status != "stale" {
		t.Fatalf("status=%q want stale", status)
	}
}
