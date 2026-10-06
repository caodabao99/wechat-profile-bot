package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestRecordCalibrationValidates(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()

	if _, err := RecordCalibration(db, "", FeedbackRiskInaccurate, "manual", now); err == nil {
		t.Fatalf("空目标应报错")
	}
	if _, err := RecordCalibration(db, "42", "不存在的反馈", "manual", now); err == nil {
		t.Fatalf("未知反馈类型应报错")
	}
	id, err := RecordCalibration(db, "42", FeedbackRiskInaccurate, "manual", now)
	if err != nil || id <= 0 {
		t.Fatalf("合法反馈应入库，id=%d err=%v", id, err)
	}
	list, err := ListCalibrations(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("应恰有 1 条，实得 %d", len(list))
	}
	// weight 必须是「小而确定」的软步长，符号由反馈类型推出（risk 不准确 → 负）。
	if list[0].Weight >= 0 || math.Abs(math.Abs(list[0].Weight)-calibrationStepWeight) > 1e-9 {
		t.Fatalf("risk_inaccurate 权重应为 -%.2f，实得 %v", calibrationStepWeight, list[0].Weight)
	}
}

func TestGetCalibrationWeightBoundedAndDecays(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()
	target := "1001"
	// 5 条同类（frequency 负向）→ 原始 -1.0，但聚合被限幅到 -cap。
	for i := 0; i < 5; i++ {
		if _, err := RecordCalibration(db, target, FeedbackContactLowFrequency, "manual", now); err != nil {
			t.Fatal(err)
		}
	}
	fresh := GetCalibrationWeight(db, target, "frequency", now)
	if fresh < -calibrationAggregateCap-1e-9 {
		t.Fatalf("聚合权重不得突破 cap，实得 %v", fresh)
	}
	if fresh >= 0 {
		t.Fatalf("frequency 负反馈应得负权重，实得 %v", fresh)
	}
	// 时间推移后同一批记录影响力衰减（禁一次反馈永久改变算法）。
	later := GetCalibrationWeight(db, target, "frequency", now.AddDate(0, 0, 120))
	if !(math.Abs(later) < math.Abs(fresh)) {
		t.Fatalf("120 天后权重应显著衰减：fresh=%v later=%v", fresh, later)
	}
	// 未知目标 → 0（回到系统默认）。
	if w := GetCalibrationWeight(db, "不存在", "frequency", now); w != 0 {
		t.Fatalf("无校准应返回 0，实得 %v", w)
	}
}

// TestCalibrationNeverOverwritesCoreRule 钉死 §9.2 铁律：校准只微调 strategy_score，绝不改 Priority。
func TestCalibrationNeverOverwritesCoreRule(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()
	cid := int64(777)
	mk := func() []DecisionCandidate {
		return []DecisionCandidate{{ContactID: cid, Priority: 50, ReasonCodes: []string{reasonReconnecting}}}
	}

	base := mk()
	applyCalibrationToCandidates(db, base, now)
	if base[0].Priority != 50 {
		t.Fatalf("无校准时 Priority 必须不变")
	}
	if base[0].StrategyScore != 50 {
		t.Fatalf("无校准（strategy_score 未叠加历史）应等于 Priority，实得 %d", base[0].StrategyScore)
	}

	// 用户标记「这个关系非常重要」→ importance 正向。
	if _, err := RecordCalibration(db, strconv.FormatInt(cid, 10), FeedbackRelationshipKey, "manual", now); err != nil {
		t.Fatal(err)
	}
	adj := mk()
	applyCalibrationToCandidates(db, adj, now)
	if adj[0].Priority != 50 {
		t.Fatalf("校准绝不允许改核心 Priority，实得 %d", adj[0].Priority)
	}
	if adj[0].StrategyScore <= 50 {
		t.Fatalf("正向重要性校准应上调 strategy_score，实得 %d", adj[0].StrategyScore)
	}
}

func TestResetCalibrationRestoreDefault(t *testing.T) {
	db := regressionDB(t)
	now := time.Now()
	if _, err := RecordCalibration(db, "555", FeedbackReminderTooOften, "manual", now); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordCalibration(db, "666", FeedbackSuggestionUnsuit, "manual", now); err != nil {
		t.Fatal(err)
	}
	n, err := ResetCalibration(db, "")
	if err != nil || n != 2 {
		t.Fatalf("恢复默认应删除全部 2 条，实得 n=%d err=%v", n, err)
	}
	list, _ := ListCalibrations(db)
	if len(list) != 0 {
		t.Fatalf("恢复默认后应为空，实得 %d", len(list))
	}
	if w := GetCalibrationWeight(db, "555", "reminder", now); w != 0 {
		t.Fatalf("恢复默认后权重应为 0，实得 %v", w)
	}
}

func TestCalibrationAPIEndToEnd(t *testing.T) {
	db := regressionDB(t)
	cfg := &Config{APIToken: "test-rel-token"}
	s := &apiServer{db: db, cfg: cfg, sessions: &webSessionStore{sessions: map[string]time.Time{}}}

	// POST 记录。
	wr := callAPI(s, http.MethodPost, "/api/calibration", `{"target":"9527","feedback":"relationship_important","source":"api"}`)
	if wr.Code != http.StatusOK {
		t.Fatalf("POST calibration 应 200，实得 %d %s", wr.Code, wr.Body.String())
	}
	// 非法反馈 → 400（非 500）。
	if wb := callAPI(s, http.MethodPost, "/api/calibration", `{"target":"9527","feedback":"乱填"}`); wb.Code != http.StatusBadRequest {
		t.Fatalf("非法反馈应 400，实得 %d", wb.Code)
	}
	// GET 列表。
	wg := callAPI(s, http.MethodGet, "/api/calibration", "")
	if wg.Code != http.StatusOK {
		t.Fatalf("GET calibration 应 200，实得 %d", wg.Code)
	}
	var gl struct {
		Items []CalibrationRecord `json:"items"`
	}
	json.Unmarshal(wg.Body.Bytes(), &gl)
	if len(gl.Items) != 1 {
		t.Fatalf("列表应含 1 条，实得 %d", len(gl.Items))
	}
	// POST 恢复默认。
	wo := callAPI(s, http.MethodPost, "/api/calibration/reset", `{}`)
	if wo.Code != http.StatusOK {
		t.Fatalf("reset 应 200，实得 %d %s", wo.Code, wo.Body.String())
	}
}
