package main

// V7 §21 Data Health 2.0：统一的子系统体检单（零 LLM、纯确定性、只读为主）。
//
// 与 datareport.go 互补：data-report 给的是体积/覆盖率等**数字**，这里给的是每个子系统
// 一个 OK/WARN/ERROR **等级** + 一句可诊断说明，并标注该项是否「可重建」。
//
// 设计铁律：
//   - 单一来源：等级判定读的是各子系统**真实运行信号**（user_version、FTS 开关、表在否、
//     孤儿计数、最近备份），绝不另立口径；派生/缓存表清单复用 backup.go 的 derivedTables。
//   - Rebuild 只能重建 derived/cache，绝不删除 core 数据：重建即「清空派生表 + 重算日标 +
//     重建 FTS」，下次访问缺则自愈（与恢复末尾同一套已验证语义）。核心表永不出现在可重建集合里。
//   - 只读体检一趟锁内顺序读完即释放；单条子系统查询失败只降级为 WARN/ERROR 说明，整页不 500。

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 数据健康等级三态（对齐蓝图 §21）。
const (
	HealthStatusOK    = "OK"
	HealthStatusWarn  = "WARN"
	HealthStatusError = "ERROR"
)

// backupStaleAfter：距最近一次成功备份超过此时长视为 WARN（备份属运维新鲜度，不是数据错误）。
const backupStaleAfter = 7 * 24 * time.Hour

// HealthCheck 一个子系统的数据健康条目。
type HealthCheck struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Detail      string `json:"detail"`
	Rebuildable bool   `json:"rebuildable"` // 仅 derived/cache 子系统为 true
}

// DataHealth 数据健康总览响应体。
type DataHealth struct {
	GeneratedAt string        `json:"generatedAt"`
	Version     string        `json:"version"`
	Checks      []HealthCheck `json:"checks"`
	Summary     string        `json:"summary"` // 总体等级：任一 ERROR→ERROR，否则任一 WARN→WARN，否则 OK
}

// countTable 返回某表行数与存在性；调用方须已持 dbMu。表缺失 → present=false, rows=0。
func countTable(db *sql.DB, table string) (rows int, present bool) {
	if !tableExistsLocked(db, table) {
		return 0, false
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM "` + table + `"`).Scan(&rows); err != nil {
		return 0, true // 表在但计数异常：以 present=true、rows 不可信处理，由调用方按语义降级
	}
	return rows, true
}

// BuildDataHealth 计算 §21 数据健康总览。任何子系统的查询失败都只反映为该条 WARN/ERROR，
// 函数本身不返回错误、绝不 panic——体检页在任何库状态下都能渲染。
func BuildDataHealth(db *sql.DB, now time.Time) *DataHealth {
	dh := &DataHealth{
		GeneratedAt: now.Format("2006-01-02 15:04:05"),
		Version:     appVersion,
		Checks:      []HealthCheck{},
	}
	add := func(name, status, detail string, rebuildable bool) {
		dh.Checks = append(dh.Checks, HealthCheck{Name: name, Status: status, Detail: detail, Rebuildable: rebuildable})
	}

	dbMu.Lock()
	defer dbMu.Unlock()

	// 1) Database：user_version 必须与程序支持的终点一致（单一来源 = backupCurrentDBVer）。
	var uv int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&uv); err != nil {
		add("Database", HealthStatusError, "无法读取数据库版本："+err.Error(), false)
	} else {
		switch {
		case uv == backupCurrentDBVer:
			add("Database", HealthStatusOK, "schema 版本 v"+hNum(uv)+"，与当前程序一致", false)
		case uv < backupCurrentDBVer:
			add("Database", HealthStatusWarn, "schema 版本 v"+hNum(uv)+" 落后（迁移应在启动时自动完成）", false)
		default:
			add("Database", HealthStatusError, "schema 版本 v"+hNum(uv)+" 高于本程序支持（来自更新版本）", false)
		}
	}

	// 2) FTS：不可用不是错误——搜索会自动降级 LIKE，功能仍工作。
	if ftsMessagesEnabled.Load() {
		add("FTS", HealthStatusOK, "全文索引可用", true)
	} else {
		add("FTS", HealthStatusWarn, "全文索引不可用，检索降级为 LIKE（功能不受影响）", true)
	}

	// 3) History Source：messages 是核心源表，缺失即致命；归档表缺失只说明从未归档（正常）。
	_, msgOK := countTable(db, "messages")
	_, archOK := countTable(db, "messages_archive")
	switch {
	case !msgOK:
		add("History Source", HealthStatusError, "核心消息表缺失", false)
	case !archOK:
		add("History Source", HealthStatusOK, "消息表可用；归档表尚未创建（从未归档，正常）", false)
	default:
		add("History Source", HealthStatusOK, "消息 + 归档并表口径可用", false)
	}

	// 派生子系统的共同基线：有没有源消息（决定「空派生表」是 WARN 还是 OK）。
	messagesPresent := msgOK
	var msgRows int
	if messagesPresent {
		msgRows, _ = countTable(db, "messages")
	}

	// 4) Facts：派生可信画像事实。有源消息却零事实 → 可重建，标 WARN。
	facts, factsPresent := countTable(db, "profile_facts")
	switch {
	case !factsPresent:
		add("Facts", HealthStatusWarn, "事实表尚未创建（访问时自愈重建）", true)
	case facts == 0 && msgRows > 0:
		add("Facts", HealthStatusWarn, "有 "+hNum(msgRows)+" 条消息但暂无事实，可重建", true)
	default:
		add("Facts", HealthStatusOK, hNum(facts)+" 条画像事实", true)
	}

	// 5) Evidence：证据链完整性——孤儿证据（fact_id 指向不存在的事实）属数据错误。
	ev, evPresent := countTable(db, "profile_fact_evidence")
	if !evPresent {
		add("Evidence", HealthStatusWarn, "证据表尚未创建（访问时自愈重建）", true)
	} else {
		var orphans int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM profile_fact_evidence
			 WHERE fact_id NOT IN (SELECT id FROM profile_facts)`).Scan(&orphans); err != nil {
			add("Evidence", HealthStatusWarn, "无法校验孤儿证据："+err.Error(), true)
		} else if orphans > 0 {
			add("Evidence", HealthStatusError, hNum(orphans)+" 条证据指向已不存在的事实（孤儿），可重建修复", true)
		} else {
			add("Evidence", HealthStatusOK, hNum(ev)+" 条证据，无孤儿", true)
		}
	}

	// 6) Metrics：日粒度互动聚合，有源消息却空 → 可重建，标 WARN。
	metrics, metricsPresent := countTable(db, "relationship_daily_metrics")
	switch {
	case !metricsPresent:
		add("Metrics", HealthStatusWarn, "指标表尚未创建（访问时自愈重建）", true)
	case metrics == 0 && msgRows > 0:
		add("Metrics", HealthStatusWarn, "有消息但指标为空，可重建", true)
	default:
		add("Metrics", HealthStatusOK, hNum(metrics)+" 行日指标", true)
	}

	// 7) Action Ledger：真实用户行为账本，不可重建；缺失说明迁移未落库。
	_, alPresent := countTable(db, "relationship_action_log")
	if !alPresent {
		add("Action Ledger", HealthStatusWarn, "行动账本表尚未创建", false)
	} else {
		al, _ := countTable(db, "relationship_action_log")
		add("Action Ledger", HealthStatusOK, hNum(al)+" 条行动记录（真实行为，不参与重建）", false)
	}

	// 8) Memory：FACT 生命周期完整性——status 只允许 active/retired，越界值说明数据漂移。
	if !factsPresent {
		add("Memory", HealthStatusWarn, "画像记忆（事实）表尚未创建", true)
	} else {
		var bad int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM profile_facts WHERE status NOT IN ('active','retired')`).Scan(&bad); err != nil {
			add("Memory", HealthStatusWarn, "无法校验记忆状态："+err.Error(), true)
		} else if bad > 0 {
			add("Memory", HealthStatusError, hNum(bad)+" 条事实状态非法（应为 active/retired）", true)
		} else {
			var active int
			db.QueryRow(`SELECT COUNT(*) FROM profile_facts WHERE status='active'`).Scan(&active)
			add("Memory", HealthStatusOK, hNum(active)+" 条活跃记忆事实", true)
		}
	}

	// 9) AI Cache：时效缓存，空也 OK（命中即重建）。
	_, cachePresent := countTable(db, "ai_response_cache")
	if !cachePresent {
		add("AI Cache", HealthStatusWarn, "AI 缓存表尚未创建", true)
	} else {
		ac, _ := countTable(db, "ai_response_cache")
		add("AI Cache", HealthStatusOK, hNum(ac)+" 条 AI 响应缓存", true)
	}

	// 10) LLM Usage：调用量审计日志，不可重建；缺失说明模块未初始化。
	_, usagePresent := countTable(db, "llm_call_log")
	if !usagePresent {
		add("LLM Usage", HealthStatusWarn, "调用量日志表尚未创建", false)
	} else {
		lu, _ := countTable(db, "llm_call_log")
		add("LLM Usage", HealthStatusOK, hNum(lu)+" 条模型调用记录", false)
	}

	// 11) Backup：最近一次成功备份的新鲜度（运维信号，非数据错误）。
	if !tableExistsLocked(db, "backup_log") {
		add("Backup", HealthStatusWarn, "尚无备份记录", false)
	} else {
		var at string
		var ok int
		err := db.QueryRow(`SELECT created_at, COALESCE(success,0) FROM backup_log ORDER BY id DESC LIMIT 1`).Scan(&at, &ok)
		switch {
		case err != nil:
			add("Backup", HealthStatusWarn, "无法读取备份记录："+err.Error(), false)
		case ok != 1:
			add("Backup", HealthStatusWarn, "最近一次备份未成功", false)
		default:
			if t, perr := time.ParseInLocation("2006-01-02 15:04:05", at, time.Local); perr == nil && now.Sub(t) > backupStaleAfter {
				add("Backup", HealthStatusWarn, "最近成功备份已超过 7 天："+at, false)
			} else {
				add("Backup", HealthStatusOK, "最近成功备份："+at, false)
			}
		}
	}

	dh.Summary = overallHealthStatus(dh.Checks)
	return dh
}

// overallHealthStatus 汇总总体等级：任一 ERROR→ERROR，否则任一 WARN→WARN，否则 OK。
func overallHealthStatus(checks []HealthCheck) string {
	warn := false
	for _, c := range checks {
		switch c.Status {
		case HealthStatusError:
			return HealthStatusError
		case HealthStatusWarn:
			warn = true
		}
	}
	if warn {
		return HealthStatusWarn
	}
	return HealthStatusOK
}

// RebuildDerived 只重建 derived/cache：清空派生表 + 重算日标 + 重建 FTS，下次访问缺则自愈。
// 绝不触碰任何 core 数据（derivedTables 清单本身就不含核心表）。返回被清空的派生表数量。
// 与恢复末尾（backup.go）同一套已验证语义：evidence 先于 facts（FK 安全），容忍 no such table。
func RebuildDerived(db *sql.DB) int {
	cleared := 0
	func() {
		dbMu.Lock()
		defer dbMu.Unlock()
		for _, t := range derivedTables {
			if _, err := db.Exec(`DELETE FROM ` + t); err != nil {
				if !strings.Contains(err.Error(), "no such table") {
					continue
				}
			}
			cleared++
		}
	}()
	// 立即重算两项高频派生信号，让体检紧随其后读到 OK（内部各自取锁，放在清表锁之外）。
	_, _ = RebuildDailyMetrics(db, 0)
	dbMu.Lock()
	rebuildFTS(db)
	dbMu.Unlock()
	return cleared
}

// hDataHealth GET /api/system/data-health：§21 数据健康总览。
func (s *apiServer) hDataHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	writeJSON(w, http.StatusOK, BuildDataHealth(s.db, time.Now()))
}

// hDataHealthRebuild POST /api/system/data-health/rebuild：只重建 derived/cache。
// 核心子系统名一律拒绝——体检页可以「修复」派生/缓存，但绝不能删除用户真实数据。
func (s *apiServer) hDataHealthRebuild(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("target"))
	if target != "" && !strings.EqualFold(target, "derived") && !strings.EqualFold(target, "all") {
		// 明确拒绝任何指定单一子系统（尤其核心）的重建请求，只放行「派生/缓存」整体重建。
		writeErr(w, http.StatusBadRequest, "只能重建派生/缓存数据（target=derived）；核心数据不可重建或删除")
		return
	}
	cleared := RebuildDerived(s.db)
	writeJSON(w, http.StatusOK, map[string]interface{}{"clearedDerivedTables": cleared, "rebuilt": "derived+metrics+fts"})
}

// hNum 是本包内 strconv.Itoa 的短别名（体检明细里频繁拼整数）。
func hNum(n int) string { return strconv.Itoa(n) }
