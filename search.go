package main

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
)

// 聊天记录全文搜索（网页端增值功能）：关键词 + 联系人 + 时间范围，可选覆盖归档表。
//
// 设计原则——对现有功能零侵入：
//   - 只读 messages / messages_archive / contacts，不写任何表、不建索引不改表结构
//   - 归档表可能不存在（ensureArchiveTables 初始化失败或旧库未升级），此时自动降级为只搜活跃表
//   - LIKE 走全表扫描，因此对 limit/offset/关键词数量都设了硬上限，避免一次请求把库拖死

const (
	searchMaxKeywords   = 5     // 空格分隔的关键词个数上限（AND 关系）
	searchMaxLimit      = 200   // 单页返回条数上限
	searchMaxOffset     = 10000 // 翻页深度上限，防止 OFFSET 全表扫穿
	searchSnippetRadius = 24    // 命中片段在关键词两侧各保留的字符数
)

// SearchOptions 搜索条件
type SearchOptions struct {
	Query          string // 关键词，空格分隔多个（AND）
	ContactID      int64  // 0 = 全部联系人
	From           string // "2026-01-01" 或 RFC3339；空 = 不限
	To             string // 同上，含当天
	IncludeArchive bool   // 是否一并搜索归档表
	Offset         int
	Limit          int
	Cursor         string // keyset 游标（安全编码，非空时优先于 Offset，翻页成本恒定）
	IncludeTotal   bool   // §11.1 默认 false：不执行全表 COUNT(*)（深分页主开销），hasMore 改由「多取一条」推断；仅显式要求时置 true
}

// SearchHit 单条命中
type SearchHit struct {
	ID          int64   `json:"id"`
	ContactID   int64   `json:"contactId"`
	ContactName string  `json:"contactName"`
	Sender      string  `json:"sender"`
	Content     string  `json:"content"`
	Snippet     string  `json:"snippet"`
	MsgTime     string  `json:"msgTime"`
	Archived    bool    `json:"archived"`
	Relevance   float64 `json:"relevance"` // FTS 路径携带（越大越相关）；LIKE 降级路径为 0。默认排序仍按时间倒序。
}

// SearchResult 搜索结果
type SearchResult struct {
	Query  string      `json:"query"`
	Total  int         `json:"total"`
	Offset int         `json:"offset"`
	Limit  int         `json:"limit"`
	List   []SearchHit `json:"list"`
	TookMs int64       `json:"tookMs"`
	// ArchiveSkipped 为 true 表示请求了搜归档但归档表不可用，结果只含活跃消息
	ArchiveSkipped bool `json:"archiveSkipped"`
	// NextCursor/HasMore 供 keyset 翻页：客户端把 NextCursor 回传即可取下一页。
	NextCursor string `json:"nextCursor,omitempty"`
	HasMore    bool   `json:"hasMore"`
}

// searchKeywords 拆分并规范化关键词
func searchKeywords(q string) ([]string, error) {
	fields := strings.Fields(q)
	if len(fields) == 0 {
		return nil, fmt.Errorf("请输入搜索关键词")
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(out) >= searchMaxKeywords {
			break
		}
		if r := utf8.RuneCountInString(f); r < 1 || r > 64 {
			continue
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("关键词无效")
	}
	return out, nil
}

// escapeLike 转义 LIKE 通配符，让用户输入的 % _ \ 按字面匹配
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// archiveTableReady 归档表是否存在且可读（旧库/初始化失败时降级）
func archiveTableReady(db *sql.DB) bool {
	dbMu.Lock()
	defer dbMu.Unlock()
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='messages_archive'`).Scan(&n)
	return err == nil && n > 0
}

// normalizeSearchBound 把用户给的日期/时间边界补成带本地时区偏移的 RFC3339。
// msg_time 存的就是带偏移的 RFC3339，而 strftime('%s', '2026-02-18') 会被 SQLite 当成 UTC 零点，
// 直接拿裸日期比较会让整个时间窗平移 8 小时：当天凌晨的消息漏掉，次日凌晨的反而被算进来。
// 解析不了就原样返回（交给 SQLite 自己判断），绝不吞掉用户的输入。
func normalizeSearchBound(s string, isEnd bool) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) == 10 {
		t, err := time.ParseInLocation("2006-01-02", s, time.Local)
		if err != nil {
			return s
		}
		if isEnd {
			t = time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, time.Local)
		}
		return t.Format(time.RFC3339)
	}
	// 19 位且不带时区（"2026-02-18T09:00:00" 或 "2026-02-18 09:00:00"）
	norm := strings.Replace(s, " ", "T", 1)
	if len(norm) == 19 && !strings.Contains(norm, "Z") &&
		!strings.ContainsAny(norm[10:], "+-") {
		if t, err := time.ParseInLocation("2006-01-02T15:04:05", norm, time.Local); err == nil {
			return t.Format(time.RFC3339)
		}
	}
	return s
}

// clampSearchOptions 归一化分页参数（默认值与硬上限），FTS 与 LIKE 两条路径共用。
func clampSearchOptions(opt SearchOptions) SearchOptions {
	if opt.Limit <= 0 {
		opt.Limit = 50
	}
	if opt.Limit > searchMaxLimit {
		opt.Limit = searchMaxLimit
	}
	if opt.Offset < 0 {
		opt.Offset = 0
	}
	if opt.Offset > searchMaxOffset {
		opt.Offset = searchMaxOffset
	}
	return opt
}

// searchCursor 是 keyset 游标载荷：按 (msg_unix, id, archived) 全序定位「下一页」。
// archived 纳入是为了在 messages 与 messages_archive 之间消除 id 跨表重号的歧义
// （两表各自 AUTOINCREMENT，同一 id 可能各有一条）。
type searchCursor struct {
	Mu int64 `json:"m"`
	ID int64 `json:"i"`
	Ar int   `json:"a"`
}

// encodeSearchCursor 把游标安全编码成不透明 base64url 串（规格 11.3：不得直传客户端任意 SQL 字段）。
func encodeSearchCursor(c searchCursor) string {
	b, _ := json.Marshal(c)
	return base64.URLEncoding.EncodeToString(b)
}

// decodeSearchCursor 解析并校验游标；非法输入返回错误 → 上层转 400，绝不据此拼 SQL。
// 解出的三个值一律以占位符参数化绑定，游标内容不可能成为 SQL 结构。
func decodeSearchCursor(s string) (searchCursor, error) {
	raw, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		return searchCursor{}, fmt.Errorf("cursor 无效")
	}
	var c searchCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return searchCursor{}, fmt.Errorf("cursor 无效")
	}
	return c, nil
}

// SearchMessages 执行搜索：优先走 FTS5（含 ≥3 字的关键词），否则/失败自动降级为 LIKE 全表扫。
// 所有 rows 迭代都在 dbMu 锁内完成（单连接 + WAL 的硬约束）。
func SearchMessages(db *sql.DB, opt SearchOptions) (*SearchResult, error) {
	keywords, err := searchKeywords(opt.Query)
	if err != nil {
		return nil, err
	}
	opt = clampSearchOptions(opt)

	ftsKws, likeKws := partitionFTSKeywords(keywords)
	// 只要有一个 ≥3 字关键词且活跃表 FTS 可用，就走 FTS 主查询；剩余的 <3 字关键词以 LIKE 追加过滤。
	if len(ftsKws) > 0 && ftsMessagesEnabled.Load() {
		res, fErr := searchViaFTS(db, opt, keywords, ftsKws, likeKws)
		if fErr == nil {
			return res, nil
		}
		// FTS 查询任意失败（虚表损坏、MATCH 语法边界等）都不能让搜索挂掉：降级 LIKE 重来。
		slog.Warn("FTS 搜索失败，降级为 LIKE", "err", fErr)
	}
	return searchViaLike(db, opt, keywords)
}

// searchViaFTS 用 messages_fts / messages_archive_fts 做主检索，联系人/时间/短词过滤在基表上。
// 默认按时间倒序（与 LIKE 路径、与升级前行为一致）；rank 仅作为附带的相关度信息返回。
func searchViaFTS(db *sql.DB, opt SearchOptions, keywords, ftsKws, likeKws []string) (*SearchResult, error) {
	start := time.Now()
	match := ftsPhraseMatch(ftsKws)
	from := normalizeSearchBound(opt.From, false)
	to := normalizeSearchBound(opt.To, true)

	// base 仅接受白名单常量（messages / messages_archive），fts 名由其派生，拼接安全。
	buildSide := func(base string, archivedFlag int) (string, []interface{}) {
		fts := base + "_fts"
		where := []string{fts + " MATCH ?"}
		args := []interface{}{match}
		for _, kw := range likeKws {
			where = append(where, `m.content LIKE ? ESCAPE '\'`)
			args = append(args, "%"+escapeLike(kw)+"%")
		}
		if opt.ContactID > 0 {
			where = append(where, "m.contact_id = ?")
			args = append(args, opt.ContactID)
		}
		if from != "" {
			where = append(where, `m.msg_time IS NOT NULL AND m.msg_time != '' AND strftime('%s', m.msg_time) >= strftime('%s', ?)`)
			args = append(args, from)
		}
		if to != "" {
			where = append(where, `m.msg_time IS NOT NULL AND m.msg_time != '' AND strftime('%s', m.msg_time) <= strftime('%s', ?)`)
			args = append(args, to)
		}
		sel := `SELECT m.id, m.contact_id, m.sender, m.content, COALESCE(m.msg_time,'') AS msg_time, ` +
			fmt.Sprintf("%d AS archived, ", archivedFlag) + fts + `.rank AS rk, COALESCE(m.msg_unix,0) AS mu
			FROM ` + fts + `
			JOIN ` + base + ` m ON m.id = ` + fts + `.rowid
			WHERE ` + strings.Join(where, " AND ")
		return sel, args
	}

	buildUnion := func(withArchive bool) (string, []interface{}) {
		sel, args := buildSide("messages", 0)
		if withArchive {
			aSel, aArgs := buildSide("messages_archive", 1)
			sel += " UNION ALL " + aSel // 两段子查询列名/列序一致，直接并
			args = append(args, aArgs...)
		}
		return sel, args
	}

	archiveSkipped := opt.IncludeArchive && !ftsArchiveEnabled.Load()

	dbMu.Lock()
	defer dbMu.Unlock()

	// 归档表在检查之后被删/损坏时，报错降级为只搜活跃表重试一次（同锁内不可递归，必须内联）。
	runQuery := func(withArchive bool) (int, []SearchHit, string, bool, error) {
		union, args := buildUnion(withArchive)
		total := 0
		// §11.1 默认不执行全表 COUNT(*)（深分页/大结果集下它是主要开销）；仅 includeTotal=true 时算。
		if opt.IncludeTotal {
			if err := db.QueryRow(`SELECT COUNT(*) FROM (`+union+`) u`, args...).Scan(&total); err != nil {
				return 0, nil, "", false, err
			}
		}
		// 排序键改用 msg_unix（避免每行 strftime 计算）；跨 messages+archive 用 (mu,id,archived)
		// 作全序，游标据此定位下一页。无 cursor 时保留旧 offset 语义向后兼容。
		pageSQL := `SELECT u.id, u.contact_id, u.sender, u.content, u.msg_time, u.archived, u.rk, u.mu,
				COALESCE(c.remark,''), COALESCE(c.name,'')
			FROM (` + union + `) u
			LEFT JOIN contacts c ON c.id = u.contact_id`
		pageArgs := append([]interface{}{}, args...)
		hasCursor := opt.Cursor != ""
		if hasCursor {
			cu, cerr := decodeSearchCursor(opt.Cursor)
			if cerr != nil {
				return 0, nil, "", false, cerr
			}
			pageSQL += ` WHERE (u.mu < ?) OR (u.mu = ? AND u.id < ?) OR (u.mu = ? AND u.id = ? AND u.archived < ?)`
			pageArgs = append(pageArgs, cu.Mu, cu.Mu, cu.ID, cu.Mu, cu.ID, cu.Ar)
		}
		pageSQL += ` ORDER BY u.mu DESC, u.id DESC, u.archived DESC`
		// 一律多取一条以判定 hasMore，替代旧「靠 total 推断」——从而默认无需 COUNT(*)。
		pageSQL += ` LIMIT ?`
		pageArgs = append(pageArgs, opt.Limit+1)
		if !hasCursor {
			pageSQL += ` OFFSET ?`
			pageArgs = append(pageArgs, opt.Offset) // 旧 offset 语义向后兼容（§11.4 deprecated）
		}
		rows, err := db.Query(pageSQL, pageArgs...)
		if err != nil {
			return 0, nil, "", false, err
		}
		defer rows.Close()
		list := []SearchHit{}
		var cursors []searchCursor
		for rows.Next() {
			var h SearchHit
			var remark string
			var archived int
			var rk float64
			var mu int64
			if err := rows.Scan(&h.ID, &h.ContactID, &h.Sender, &h.Content, &h.MsgTime, &archived, &rk, &mu, &remark, &h.ContactName); err != nil {
				return 0, nil, "", false, err
			}
			h.Archived = archived == 1
			h.Relevance = roundRelevance(rk)
			if strings.TrimSpace(remark) != "" {
				h.ContactName = remark + "（" + h.ContactName + "）"
			}
			h.Snippet = buildSnippet(h.Content, keywords)
			list = append(list, h)
			cursors = append(cursors, searchCursor{Mu: mu, ID: h.ID, Ar: archived})
		}
		if err := rows.Err(); err != nil {
			return 0, nil, "", false, err
		}
		nextCursor, hasMore := "", false
		if len(list) > opt.Limit {
			hasMore = true
			list = list[:opt.Limit]
			nextCursor = encodeSearchCursor(cursors[opt.Limit-1])
		} else if len(list) > 0 {
			// 末批也回带游标：客户端「首屏 offset、加载更多切 cursor」可无缝衔接。
			nextCursor = encodeSearchCursor(cursors[len(list)-1])
		}
		return total, list, nextCursor, hasMore, nil
	}

	wantArchive := opt.IncludeArchive && !archiveSkipped
	total, list, nextCursor, hasMore, err := runQuery(wantArchive)
	if err != nil && wantArchive {
		archiveSkipped = true
		if total, list, nextCursor, hasMore, err = runQuery(false); err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	return &SearchResult{
		Query:          strings.Join(keywords, " "),
		Total:          total,
		Offset:         opt.Offset,
		Limit:          opt.Limit,
		List:           list,
		TookMs:         time.Since(start).Milliseconds(),
		ArchiveSkipped: archiveSkipped,
		NextCursor:     nextCursor,
		HasMore:        hasMore,
	}, nil
}

// roundRelevance 把 FTS5 rank（升序=越靠前越相关）转成「越大越相关」的相关度，保留 6 位小数。
func roundRelevance(rank float64) float64 {
	v := -rank
	return float64(int64(v*1e6+0.5)) / 1e6
}

// searchViaLike 原有的 LIKE 全表扫描路径：作为 FTS 不可用（未建立/短词/降级）时的检索方式。
// keywords 与已归一化的 opt 由调用方传入（避免重复解析、两路径行为一致）。
func searchViaLike(db *sql.DB, opt SearchOptions, keywords []string) (*SearchResult, error) {
	start := time.Now()

	// 单表条件（messages 与 messages_archive 同构，别名不同而已）
	buildCond := func(alias string) (string, []interface{}) {
		cond := []string{"1=1"}
		var args []interface{}
		for _, kw := range keywords {
			cond = append(cond, fmt.Sprintf(`%s.content LIKE ? ESCAPE '\'`, alias))
			args = append(args, "%"+escapeLike(kw)+"%")
		}
		if opt.ContactID > 0 {
			cond = append(cond, alias+".contact_id = ?")
			args = append(args, opt.ContactID)
		}
		if from := normalizeSearchBound(opt.From, false); from != "" {
			// msg_time 存的是 RFC3339 字符串，比较一律先 strftime 转 epoch，绝不能字典序比
			cond = append(cond, fmt.Sprintf(`%s.msg_time IS NOT NULL AND %s.msg_time != '' AND strftime('%%s', %s.msg_time) >= strftime('%%s', ?)`, alias, alias, alias))
			args = append(args, from)
		}
		if to := normalizeSearchBound(opt.To, true); to != "" {
			// 只给日期时按"含当天"处理：normalizeSearchBound 已把上界推到 23:59:59
			cond = append(cond, fmt.Sprintf(`%s.msg_time IS NOT NULL AND %s.msg_time != '' AND strftime('%%s', %s.msg_time) <= strftime('%%s', ?)`, alias, alias, alias))
			args = append(args, to)
		}
		return strings.Join(cond, " AND "), args
	}

	// buildUnion 组装活跃表（+ 可选归档表）的 UNION 子查询。
	// messages 与 messages_archive 同构，条件构造逻辑复用 buildCond。
	buildUnion := func(withArchive bool) (string, []interface{}) {
		activeCond, activeArgs := buildCond("m")
		union := `SELECT m.id, m.contact_id, m.sender, m.content, m.msg_time, 0 AS archived, COALESCE(m.msg_unix,0) AS mu
			FROM messages m WHERE ` + activeCond
		args := append([]interface{}{}, activeArgs...)
		if withArchive {
			archCond, archArgs := buildCond("a")
			union += ` UNION ALL SELECT a.id, a.contact_id, a.sender, a.content, a.msg_time, 1 AS archived, COALESCE(a.msg_unix,0) AS mu
				FROM messages_archive a WHERE ` + archCond
			args = append(args, archArgs...)
		}
		return union, args
	}

	archiveSkipped := opt.IncludeArchive && !archiveTableReady(db)

	dbMu.Lock()
	defer dbMu.Unlock()

	// 极端情况下归档表在检查之后被删掉：查询报错时降级为只搜活跃表重试一次。
	// 注意必须在本函数内重试（不能递归调用自己），dbMu 不可重入，递归会死锁。
	runQuery := func(withArchive bool) (int, []SearchHit, string, bool, error) {
		union, args := buildUnion(withArchive)
		total := 0
		// §11.1 默认不执行全表 COUNT(*)（深分页/大结果集下它是主要开销）；仅 includeTotal=true 时算。
		if opt.IncludeTotal {
			if err := db.QueryRow(`SELECT COUNT(*) FROM (`+union+`) u`, args...).Scan(&total); err != nil {
				return 0, nil, "", false, err
			}
		}
		// 与 FTS 路径一致：排序键用 msg_unix，跨双表 用 (mu,id,archived) 全序；有 cursor 走 keyset。
		pageSQL := `SELECT u.id, u.contact_id, u.sender, u.content, COALESCE(u.msg_time,''), u.archived, u.mu,
				COALESCE(c.remark,''), COALESCE(c.name,'')
			FROM (` + union + `) u
			LEFT JOIN contacts c ON c.id = u.contact_id`
		pageArgs := append([]interface{}{}, args...)
		hasCursor := opt.Cursor != ""
		if hasCursor {
			cu, cerr := decodeSearchCursor(opt.Cursor)
			if cerr != nil {
				return 0, nil, "", false, cerr
			}
			pageSQL += ` WHERE (u.mu < ?) OR (u.mu = ? AND u.id < ?) OR (u.mu = ? AND u.id = ? AND u.archived < ?)`
			pageArgs = append(pageArgs, cu.Mu, cu.Mu, cu.ID, cu.Mu, cu.ID, cu.Ar)
		}
		pageSQL += ` ORDER BY u.mu DESC, u.id DESC, u.archived DESC`
		// 一律多取一条以判定 hasMore，替代旧「靠 total 推断」——从而默认无需 COUNT(*)。
		pageSQL += ` LIMIT ?`
		pageArgs = append(pageArgs, opt.Limit+1)
		if !hasCursor {
			pageSQL += ` OFFSET ?`
			pageArgs = append(pageArgs, opt.Offset) // 旧 offset 语义向后兼容（§11.4 deprecated）
		}
		rows, err := db.Query(pageSQL, pageArgs...)
		if err != nil {
			return 0, nil, "", false, err
		}
		defer rows.Close()
		list := []SearchHit{}
		var cursors []searchCursor
		for rows.Next() {
			var h SearchHit
			var remark string
			var archived int
			var mu int64
			if err := rows.Scan(&h.ID, &h.ContactID, &h.Sender, &h.Content, &h.MsgTime, &archived, &mu, &remark, &h.ContactName); err != nil {
				return 0, nil, "", false, err
			}
			h.Archived = archived == 1
			if strings.TrimSpace(remark) != "" {
				h.ContactName = remark + "（" + h.ContactName + "）"
			}
			h.Snippet = buildSnippet(h.Content, keywords)
			list = append(list, h)
			cursors = append(cursors, searchCursor{Mu: mu, ID: h.ID, Ar: archived})
		}
		if err := rows.Err(); err != nil {
			return 0, nil, "", false, err
		}
		nextCursor, hasMore := "", false
		if len(list) > opt.Limit {
			hasMore = true
			list = list[:opt.Limit]
			nextCursor = encodeSearchCursor(cursors[opt.Limit-1])
		} else if len(list) > 0 {
			// 末批也回带游标：客户端「首屏 offset、加载更多切 cursor」可无缝衔接。
			nextCursor = encodeSearchCursor(cursors[len(list)-1])
		}
		return total, list, nextCursor, hasMore, nil
	}

	wantArchive := opt.IncludeArchive && !archiveSkipped
	total, list, nextCursor, hasMore, err := runQuery(wantArchive)
	if err != nil && wantArchive {
		archiveSkipped = true
		if total, list, nextCursor, hasMore, err = runQuery(false); err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}

	return &SearchResult{
		Query:          strings.Join(keywords, " "),
		Total:          total,
		Offset:         opt.Offset,
		Limit:          opt.Limit,
		List:           list,
		TookMs:         time.Since(start).Milliseconds(),
		ArchiveSkipped: archiveSkipped,
		NextCursor:     nextCursor,
		HasMore:        hasMore,
	}, nil
}

// buildSnippet 截取首个命中关键词附近的片段，前后加省略号
func buildSnippet(content string, keywords []string) string {
	content = strings.Join(strings.Fields(content), " ")
	lower := strings.ToLower(content)
	pos := -1
	for _, kw := range keywords {
		if i := strings.Index(lower, strings.ToLower(kw)); i >= 0 && (pos < 0 || i < pos) {
			pos = i
		}
	}
	runes := []rune(content)
	if pos < 0 {
		if len(runes) <= searchSnippetRadius*2 {
			return content
		}
		return string(runes[:searchSnippetRadius*2]) + "…"
	}
	// 字节偏移转 rune 偏移
	start := utf8.RuneCountInString(content[:pos])
	from := start - searchSnippetRadius
	if from < 0 {
		from = 0
	}
	to := start + utf8.RuneCountInString(keywords[0]) + searchSnippetRadius
	if to > len(runes) {
		to = len(runes)
	}
	out := string(runes[from:to])
	if from > 0 {
		out = "…" + out
	}
	if to < len(runes) {
		out += "…"
	}
	return out
}
