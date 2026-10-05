package main

// export_api.go —— v4.5.0 C：关系数据可携（全量 JSON 导出，可选脱敏）。
//
// 与二进制 SQLite 备份互补：备份是"整库还原"用途、格式私有；本导出是"人可读、可迁移、
// 可审计"的开放 JSON，回应用户对敏感关系数据的掌控诉求。纯本地、只读、零模型、零费用。
//
//   GET /api/data/export?redact=0|1&include=archive,derived
//
// 认证与 IP 白名单由上层统一中间件处理，与其它写端点一致。
// 流式输出：逐表查询、逐行编码直写响应体，避免一次性把全库驻留内存；
// 单连接池铁律：dbMu 只包住"查询+迭代+Close"，其间只往 http 写、绝不对同一 db 执行写操作。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// routeData 分发 /api/data/*，目前仅 export 一个子路径（GET），其余 404/405，镜像 routeInsight 深度校验。
func (s *apiServer) routeData(w http.ResponseWriter, r *http.Request, sub []string) {
	if len(sub) != 1 || sub[0] != "export" {
		writeErr(w, http.StatusNotFound, "未知接口: /api/data/"+strings.Join(sub, "/"))
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}
	s.hDataExport(w, r)
}

// exportAlwaysSkip 永不导出：备份审计日志本身（含内部路径/时间，非用户数据）。
var exportAlwaysSkip = map[string]bool{"backup_log": true}

// exportArchiveTables 仅当 include 含 archive 时导出。
var exportArchiveTables = map[string]bool{"messages_archive": true}

// exportCacheTables 单行派生缓存/趋势表，仅当 include 含 derived 时导出（都可自愈重建）。
var exportCacheTables = map[string]bool{
	"weekly_plan_cache": true, "life_state_cache": true, "life_projection_cache": true,
	"network_insight_cache": true, "self_portrait_cache": true, "intervention_cache": true,
	"briefing_cache": true, "insight_trend_history": true,
}

// redactSensitiveCols 脱敏时会被替换为 *** 的"正文类"列（跨表通用，按列名匹配）。
var redactSensitiveCols = map[string]bool{
	"content": true, "message": true, "text": true, "draft": true, "reason": true,
	"detail": true, "snippet": true, "raw": true, "value": true, "answer": true,
	"question": true, "title": true, "profile_json": true, "summary": true, "note": true,
	"remark": true,
}

// shouldExportTable 判定某表在当前 include 选项下是否纳入导出。
func shouldExportTable(name string, wantArchive, wantDerived bool) bool {
	if strings.HasPrefix(name, "sqlite_") || strings.Contains(name, "_fts") {
		return false // FTS5 虚表及其影子表：非源数据
	}
	if exportAlwaysSkip[name] {
		return false
	}
	if exportArchiveTables[name] {
		return wantArchive
	}
	if exportCacheTables[name] {
		return wantDerived
	}
	return true
}

// hDataExport 流式导出全库为单个 JSON 附件。
func (s *apiServer) hDataExport(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	q := r.URL.Query()
	redact := q.Get("redact") == "1" || strings.EqualFold(q.Get("redact"), "true")
	include := q.Get("include")
	wantArchive := strings.Contains(include, "archive")
	wantDerived := strings.Contains(include, "derived")

	// 1) 先列出待导出表名（一次取尽，锁外决策）。
	names, err := s.listExportTables()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "枚举数据表失败: "+err.Error())
		return
	}
	var tables []string
	for _, n := range names {
		if shouldExportTable(n, wantArchive, wantDerived) {
			tables = append(tables, n)
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="wechat-profile-export-%s.json"`, now.Format("20060102")))
	w.WriteHeader(http.StatusOK)

	// 2) 手写外层对象，逐表流式写。任一写失败即中断（头已发出，无法再改状态码）。
	if _, err := fmt.Fprintf(w, `{"exportedAt":%q,"version":%q,"redacted":%v,"tables":{`,
		now.Format(time.RFC3339), appVersion, redact); err != nil {
		return
	}
	for ti, tbl := range tables {
		if ti > 0 {
			if _, err := w.Write([]byte(",")); err != nil {
				return
			}
		}
		keyB, _ := json.Marshal(tbl)
		if _, err := fmt.Fprintf(w, `%s:`, keyB); err != nil {
			return
		}
		if err := s.streamTable(w, tbl, redact); err != nil {
			// 中途失败：尽量补一个空数组闭合，保证整体 JSON 结构可被解析。
			_, _ = w.Write([]byte("[]"))
			return
		}
	}
	_, _ = w.Write([]byte("}}"))
}

// listExportTables 取 sqlite_master 里所有普通表名（含虚表，过滤在调用方做）。
func (s *apiServer) listExportTables() ([]string, error) {
	dbMu.Lock()
	defer dbMu.Unlock()
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			out = append(out, n)
		}
	}
	return out, nil
}

// streamTable 查询单表并逐行编码写入。表名来自 sqlite_master，非用户输入，直接拼接安全。
func (s *apiServer) streamTable(w http.ResponseWriter, table string, redact bool) error {
	dbMu.Lock()
	rows, err := s.db.Query(`SELECT * FROM "` + table + `"`)
	if err != nil {
		dbMu.Unlock()
		return err
	}
	defer func() { rows.Close(); dbMu.Unlock() }()

	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	// contacts 脱敏需按 id 生成伪名，先探测是否有 id 列。
	idIdx := -1
	for i, c := range cols {
		if strings.EqualFold(c, "id") {
			idIdx = i
			break
		}
	}
	isContacts := table == "contacts"

	if _, err := w.Write([]byte("[")); err != nil {
		return err
	}
	first := true
	raws := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for rows.Next() {
		for i := range raws {
			ptrs[i] = &raws[i]
		}
		if rows.Scan(ptrs...) != nil {
			continue
		}
		obj := make(map[string]any, len(cols))
		var pseudo string
		if isContacts && redact && idIdx >= 0 {
			pseudo = fmt.Sprintf("联系人#%v", toExportScalar(raws[idIdx]))
		}
		for i, c := range cols {
			v := toExportScalar(raws[i])
			if redact {
				if isContacts && (strings.EqualFold(c, "name") || strings.EqualFold(c, "nickname") || strings.EqualFold(c, "remark")) {
					v = pseudo // 姓名/备注→伪名，保留结构可关联
					obj[c] = v
					continue
				}
				if redactSensitiveCols[c] {
					if vs, ok := v.(string); ok && vs != "" {
						v = "***" // 正文类一律抹除，保留计数/时间戳/结构
					}
				}
			}
			obj[c] = v
		}
		b, err := json.Marshal(obj)
		if err != nil {
			continue
		}
		if !first {
			if _, err := w.Write([]byte(",")); err != nil {
				return err
			}
		}
		first = false
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	_, err2 := w.Write([]byte("]"))
	return err2
}

// toExportScalar 把 database/sql 扫出的原始值归一成 JSON 友好类型：
//
//	[]byte→string、int64/float64/bool/string/nil 原样、time.Time→RFC3339，其余转字符串。
func toExportScalar(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(x)
	case int64:
		return x
	case float64:
		return x
	case bool:
		return x
	case string:
		return x
	case time.Time:
		return x.Format(time.RFC3339)
	default:
		return fmt.Sprintf("%v", x)
	}
}
