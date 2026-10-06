package main

// 蓝图 §9 P1：Personal Calibration（个性化校准）。
//
// 铁律——不直接修改核心规则（§9.2）：
//   - 用户的五类反馈只落一张独立的 personal_calibration_profile 表，绝不改 Decision Engine 的
//     确定性 Priority、绝不动评分权重/规则本身。
//   - soft adjustment：每条反馈只是一个「小而有界」的权重，读取时按时间指数衰减、并对聚合值限幅，
//     因此**一次反馈绝不可能永久改变算法**（影响力会随不重复反馈自然消退）。
//   - 可恢复默认（§9.3）：删除校准记录即回到系统默认行为。
//
// 数据来源单一：这张表是「用户主观校准」的唯一真相，与 Action Ledger（客观结果）互不覆盖。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// §9.1 五类用户反馈标记（可扩展；每一项映射到一个 target_type 与影响方向）。
const (
	FeedbackRiskInaccurate      = "risk_inaccurate"        // 这个风险不准确
	FeedbackContactLowFrequency = "contact_low_frequency"  // 这个人不需要频繁联系
	FeedbackRelationshipKey     = "relationship_important" // 这个关系非常重要
	FeedbackSuggestionUnsuit    = "suggestion_unsuitable"  // 这个建议不合适
	FeedbackReminderTooOften    = "reminder_too_frequent"  // 这个提醒太频繁
)

// calibrationTargetType 反馈类型 → (target_type, 权重符号)。target_type 与 DB CHECK 一一对应。
func calibrationTargetType(feedback string) (targetType string, sign float64, ok bool) {
	switch feedback {
	case FeedbackRiskInaccurate:
		return "risk", -1, true
	case FeedbackContactLowFrequency:
		return "frequency", -1, true
	case FeedbackRelationshipKey:
		return "importance", +1, true
	case FeedbackSuggestionUnsuit:
		return "suggestion", -1, true
	case FeedbackReminderTooOften:
		return "reminder", -1, true
	default:
		return "", 0, false
	}
}

// soft adjustment 参数（全部有界，确保绝不盖过核心规则、绝不永久化）。
const (
	calibrationStepWeight    = 0.20 // 单次反馈的初始权重幅度（小）
	calibrationAggregateCap  = 0.50 // 同一 (target,type) 聚合权重的绝对上限（软，绝不翻盘核心）
	calibrationHalfLifeDays  = 30.0 // 影响力半衰期：不复述即自然衰减 → 禁一次反馈永久改算法
	calibrationMaxRawRecords = 500  // 防滥用总量上限（超过则由调用方择机清理，本层不硬删）
)

// CalibrationRecord 一条校准反馈（§9.2 字段：target/feedback/timestamp/weight/source）。
type CalibrationRecord struct {
	ID         int64   `json:"id"`
	Target     string  `json:"target"`
	TargetType string  `json:"target_type"`
	Feedback   string  `json:"feedback"`
	Weight     float64 `json:"weight"` // 该条落库的有符号权重
	Source     string  `json:"source"` // manual / session / ...
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
}

// calibrationDecay 按存在天数做指数衰减：半衰期 calibrationHalfLifeDays 后影响力减半。
func calibrationDecay(ageDays float64) float64 {
	if ageDays <= 0 {
		return 1
	}
	return math.Pow(0.5, ageDays/calibrationHalfLifeDays)
}

// ensureCalibrationTable 运行期守门：老库尚未跑到 v27 时，惰性建表（幂等，绝不 DROP）。
func ensureCalibrationTable(db *sql.DB) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	if tableExistsLocked(db, "personal_calibration_profile") {
		return nil
	}
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS personal_calibration_profile (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		target      TEXT NOT NULL,
		target_type TEXT NOT NULL CHECK(target_type IN ('risk','frequency','importance','suggestion','reminder')),
		feedback    TEXT NOT NULL,
		weight      REAL NOT NULL DEFAULT 0,
		source      TEXT NOT NULL DEFAULT 'manual',
		created_at  TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
	)`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_calibration_target ON personal_calibration_profile(target, target_type)`)
	return err
}

// RecordCalibration 记录一条用户反馈（§9.1）。反馈类型必须合法；weight 由类型确定性推出（有界），
// 调用方不能塞入任意大权重——从根上保证「soft」。绝不写任何核心规则表。
func RecordCalibration(db *sql.DB, target, feedback, source string, now time.Time) (int64, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return 0, fmt.Errorf("校准目标不能为空")
	}
	targetType, sign, ok := calibrationTargetType(feedback)
	if !ok {
		return 0, fmt.Errorf("未知的反馈类型: %q", feedback)
	}
	if len(target) > 200 {
		target = target[:200]
	}
	if source == "" {
		source = "manual"
	}
	if err := ensureCalibrationTable(db); err != nil {
		return 0, err
	}
	w := sign * calibrationStepWeight
	nowStr := now.Format(time.RFC3339)
	dbMu.Lock()
	defer dbMu.Unlock()
	res, err := db.Exec(`INSERT INTO personal_calibration_profile
		(target, target_type, feedback, weight, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		target, targetType, feedback, w, source, nowStr, nowStr)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// GetCalibrationWeight 读某 (target, targetType) 的聚合软权重：各条衰减后求和并限幅到
// [-cap, cap]。表缺失/无记录 → 返回 0（等价于「无个性化，用系统默认」），绝不报错阻断核心链路。
func GetCalibrationWeight(db *sql.DB, target, targetType string, now time.Time) float64 {
	var rows *sql.Rows
	err := func() error {
		dbMu.Lock()
		defer dbMu.Unlock()
		if !tableExistsLocked(db, "personal_calibration_profile") {
			return sql.ErrNoRows
		}
		r, e := db.Query(`SELECT weight, created_at FROM personal_calibration_profile
			WHERE target = ? AND target_type = ?`, target, targetType)
		if e != nil {
			return e
		}
		rows = r
		return nil
	}()
	if err != nil || rows == nil {
		return 0
	}
	defer rows.Close()
	sum := 0.0
	for rows.Next() {
		var w float64
		var created string
		if rows.Scan(&w, &created) != nil {
			continue
		}
		age := now.Sub(parseCalibTime(created, now)).Hours() / 24
		sum += w * calibrationDecay(age)
	}
	if sum > calibrationAggregateCap {
		sum = calibrationAggregateCap
	}
	if sum < -calibrationAggregateCap {
		sum = -calibrationAggregateCap
	}
	return roundF(sum, 4)
}

func parseCalibTime(s string, now time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t
	}
	return now // 解析不了就当此刻（保守：不打折衰减，但仍受 cap 约束）
}

// ListCalibrations 返回全部校准记录（供 UI 展示与「恢复默认」预览）。
func ListCalibrations(db *sql.DB) ([]CalibrationRecord, error) {
	out := []CalibrationRecord{}
	dbMu.Lock()
	defer dbMu.Unlock()
	if !tableExistsLocked(db, "personal_calibration_profile") {
		return out, nil
	}
	rows, err := db.Query(`SELECT id, target, target_type, feedback, weight, source, created_at, updated_at
		FROM personal_calibration_profile ORDER BY id DESC LIMIT ?`, calibrationMaxRawRecords)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c CalibrationRecord
		if err := rows.Scan(&c.ID, &c.Target, &c.TargetType, &c.Feedback,
			&c.Weight, &c.Source, &c.CreatedAt, &c.UpdatedAt); err != nil {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// ResetCalibration 恢复系统默认（§9.3）：target 为空则清空全部，否则清某目标。
// 只删本表记录——核心规则从未被改过，故删完即回到默认行为。
func ResetCalibration(db *sql.DB, target string) (int64, error) {
	if err := ensureCalibrationTable(db); err != nil {
		return 0, err
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	var res sql.Result
	var err error
	if t := strings.TrimSpace(target); t != "" {
		res, err = db.Exec(`DELETE FROM personal_calibration_profile WHERE target = ?`, t)
	} else {
		res, err = db.Exec(`DELETE FROM personal_calibration_profile`)
	}
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// applyCalibrationToCandidates 把个性化软权重叠加到 strategy_score（只读地，绝不改 Priority、
// 不重排）。对每个候选读其联系人的 frequency/importance 校准，映射为一个有界 ±整数 nudge。
func applyCalibrationToCandidates(db *sql.DB, cands []DecisionCandidate, now time.Time) {
	for i := range cands {
		if cands[i].StrategyScore == 0 {
			cands[i].StrategyScore = cands[i].Priority
		}
		tid := strconv.FormatInt(cands[i].ContactID, 10)
		freqW := GetCalibrationWeight(db, tid, "frequency", now)
		impW := GetCalibrationWeight(db, tid, "importance", now)
		// frequency 负权重（"不需要频繁联系"）→ 下调；importance 正权重（"非常重要"）→ 上调。
		nudge := int(math.Round((impW + freqW) * 4)) // cap=0.5 → nudge ∈ [-4,4]，很小
		if nudge > 4 {
			nudge = 4
		}
		if nudge < -4 {
			nudge = -4
		}
		if nudge != 0 {
			cands[i].StrategyScore += nudge
			note := "已按你的个性化校准微调（软调整，不改核心规则）"
			if cands[i].StrategyNote == "" {
				cands[i].StrategyNote = note
			} else {
				cands[i].StrategyNote = cands[i].StrategyNote + "；" + note
			}
		}
	}
}

// —— HTTP（§9）——

func (s *apiServer) routeCalibration(w http.ResponseWriter, r *http.Request, sub []string) {
	// POST /api/calibration/reset → 恢复默认
	if len(sub) == 1 && sub[0] == "reset" && r.Method == http.MethodPost {
		var body struct {
			Target string `json:"target"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body) // 允许空体=清全部
		n, err := ResetCalibration(s.db, body.Target)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "恢复默认失败")
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"reset": true, "removed": n})
		return
	}
	if len(sub) != 0 {
		writeErr(w, http.StatusNotFound, "未知接口: /api/calibration/"+strings.Join(sub, "/"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		list, err := ListCalibrations(s.db)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取校准失败")
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"items": list})
	case http.MethodPost:
		var body struct {
			Target   string `json:"target"`
			Feedback string `json:"feedback"`
			Source   string `json:"source"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体解析失败")
			return
		}
		id, err := RecordCalibration(s.db, body.Target, body.Feedback, body.Source, time.Now())
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"id": id, "recorded": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET/POST")
	}
}
