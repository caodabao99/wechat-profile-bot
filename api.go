package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// profileInFlight 记录正在进行画像生成的 contactID，避免短时间内重复触发
var profileInFlight sync.Map

// backgroundProfileTimeout 后台画像生成的最长执行时间。
// LLM 客户端单次超时 60s、失败重试一次，最坏约 122s；后台任务用的是脱离请求的
// context（请求返回后 r.Context() 就被取消了），必须自带超时，否则 LLM 端挂住时
// goroutine 会永久占用 profileInFlight 里的槽位，该联系人再也无法触发画像更新。
const backgroundProfileTimeout = 3 * time.Minute

// maxMessagesPageLimit 单次分页最多返回的消息条数。
// limit 由客户端传入，不设上限时一个请求就能把整库消息拉走（桌面端远程模式
// 会在内存里一次性展开），既拖垮服务端也可能打爆客户端。
const maxMessagesPageLimit = 500

// ingestAnalyzeTimeout 意图分析的硬上限。
// 桌面端粘贴聊天记录后同步等这个结果，但 LLM 最坏要 ~122s；超时后仍然返回
// 已完成的入库结果，把错误单独放进 intentError 字段，不至于让用户以为整次导入失败。
const ingestAnalyzeTimeout = 100 * time.Second

// apiServer 桌面端远程调用的 REST API 服务
type apiServer struct {
	db       *sql.DB
	llm      *LLMClient
	cfg      *Config
	client   *ILinkClient        // 微信连接状态查询；CLI/测试场景可为 nil
	sessions *webSessionStore    // 网页端 2FA 通过后颁发的会话
	trusted  *trustedClientStore // 可信客户端长效令牌（免重复登录）
	guard    *securityGuard      // 登录失败计数 + IP 永久封禁 + 安全日志
	ingestRL *rateLimiter        // /api/ingest 限流，防止 token 泄露后被刷爆 LLM 账单
}

// clientIP 从请求中提取直连客户端 IP（去掉端口，兼容 IPv4/IPv6）。
// 注意：这是 TCP 连接对端地址，无法伪造；反代场景下它是反代服务器的 IP。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ipInList 判断 IP（ipStr 为其字符串形式）是否命中 IP/CIDR 列表。
func ipInList(ip net.IP, ipStr string, list []string) bool {
	for _, entry := range list {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		// CIDR 段
		if strings.Contains(entry, "/") {
			if _, cidr, err := net.ParseCIDR(entry); err == nil && cidr.Contains(ip) {
				return true
			}
			continue
		}
		// 单 IP
		if entry == ipStr {
			return true
		}
		if allowed := net.ParseIP(entry); allowed != nil && allowed.Equal(ip) {
			return true
		}
	}
	return false
}

// realClientIP 返回真实访客 IP。
// 安全规则：只有 TCP 直连对端（clientIP）在可信代理列表里时，才采纳
// X-Forwarded-For；否则该头可被任意外部客户端伪造，必须直接忽略。
// 采纳时从 XFF 链最右侧（离服务最近）向左跳过所有可信代理，第一个非可信
// 地址即真实访客——这样即使访客在 XFF 左侧伪造 IP，单级反代追加真实地址后
// 伪造项也不会被采用（nginx $proxy_add_x_forwarded_for 即此结构）。
func realClientIP(r *http.Request, trustedProxies []string) string {
	direct := clientIP(r)
	directIP := net.ParseIP(direct)
	if directIP == nil || !ipInList(directIP, direct, trustedProxies) {
		return direct
	}
	// XFF: "访客, 代理1, 代理2"，最右是离本服务最近的一跳（已用 direct 验证可信）
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	chain := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			chain = append(chain, p)
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		ip := net.ParseIP(chain[i])
		if ip != nil && ipInList(ip, chain[i], trustedProxies) {
			continue
		}
		return chain[i] // 第一个非可信跳：真实访客（含"无法解析"的异常值也返回，交由白名单拒绝）
	}
	if len(chain) > 0 {
		return chain[0] // 整条链全是可信代理，退而取最左
	}
	return direct
}

func (s *apiServer) realIP(r *http.Request) string {
	return realClientIP(r, s.cfg.TrustedProxies)
}

// checkIPWhitelist 检查真实访客 IP 是否在白名单内。
// whitelist 为空时不限制（返回 true）；非空时必须命中其中任一 IP 或 CIDR 段。
func checkIPWhitelist(r *http.Request, whitelist, trustedProxies []string) bool {
	if len(whitelist) == 0 {
		return true
	}
	ipStr := realClientIP(r, trustedProxies)
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	return ipInList(ip, ipStr, whitelist)
}

// startAPIServer 启动 HTTP API 服务（供 Windows 桌面版远程调用）
func startAPIServer(db *sql.DB, llm *LLMClient, client *ILinkClient, cfg *Config, port int) *http.Server {
	s := &apiServer{
		db:       db,
		llm:      llm,
		client:   client,
		cfg:      cfg,
		sessions: initWebSessionStore(),
		trusted:  initTrustedClientStore(),
		guard:    newSecurityGuard(),
		// /api/ingest 每被调用一次就真实消耗一次 LLM 额度。apiToken 一旦泄露，
		// 没有上限的调用次数意味着账单可以在几小时内被刷爆。这里按 token（无 token
		// 时按 IP）限制每分钟请求数，正常使用（人工粘贴聊天记录）远达不到这个量。
		ingestRL: newRateLimiter(ingestRateLimit, ingestRateWindow),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", s.route)
	// 网页管理界面：静态资源不走认证（页面本身无敏感数据，数据接口都在 /api/ 下受 Token 保护）。
	// ServeMux 按最长前缀匹配，/api/ 请求仍进入带认证的 s.route。
	mux.HandleFunc("/assets/", handleAssets)
	mux.HandleFunc("/", handleWebUI)
	srv := &http.Server{
		Addr: fmt.Sprintf(":%d", port),
		// 安全头 + IP 封禁拦截包在最外层：封禁要覆盖网页界面和静态资源，
		// 不能只拦 /api/，否则被封的 IP 还能加载页面反复试。
		Handler: withSecurity(mux, s.guard, cfg.TrustedProxies),
		// ReadHeaderTimeout 是防 slowloris 慢速攻击的关键。默认值 0 表示永不超时，
		// 攻击者只发一半请求头就能永久占住连接 + goroutine + 文件描述符，而 IP 白名单
		// 和 token 检查都发生在"请求头收完整之后"，根本管不到这一层。
		// 请求头只有几百字节，10 秒发不完的一定是恶意连接；正常客户端毫秒级完成，无感。
		ReadHeaderTimeout: 10 * time.Second,
		// IdleTimeout 回收 keep-alive 空闲连接，防止长期运行后 fd 累积到 too many open files
		IdleTimeout: 120 * time.Second,
		// 刻意不设 ReadTimeout / WriteTimeout，设了会直接搞坏现有功能：
		//   ReadTimeout  —— 备份导入允许 200MB 上传（backupMaxUpload），手机慢网络传不完就被掐断
		//   WriteTimeout —— 同步 LLM 调用很慢：画像生成最坏 ~122s（超时 60s × 重试 + 等 2s），
		//                   「重新生成画像」要连调两次约 244s，意图分析上限 100s
		// 这两个阶段的取消由各请求自己的 ctx（r.Context()）负责，客户端断开会正常传播。
	}
	go func() {
		slog.Info("API 服务已启动", "addr", fmt.Sprintf("http://0.0.0.0:%d/api/", port))
		slog.Info("网页管理界面", "addr", fmt.Sprintf("http://0.0.0.0:%d/", port))
		if len(cfg.APIWhitelist) > 0 {
			slog.Info("API 访问白名单已启用", "allow", cfg.APIWhitelist)
		} else {
			slog.Info("API 访问白名单未启用（不限制来源 IP）")
		}
		if len(cfg.TrustedProxies) > 0 {
			slog.Info("可信反向代理已配置", "proxies", cfg.TrustedProxies,
				"hint", "白名单/封禁按 X-Forwarded-For 真实访客 IP 判定；非可信直连来源的 XFF 头将被忽略")
		}
		if cfg.APIToken != "" {
			slog.Info("API 认证: Bearer Token 已启用")
		} else {
			slog.Warn("API 未设置认证 Token，任何知道地址的人都能调用！")
		}
		if bans := s.guard.ListBans(); len(bans) > 0 {
			slog.Warn("当前有 IP 处于永久封禁状态", "count", len(bans),
				"hint", "解封: wechat-profile-bot --unban <ip>；查看: --list-bans")
		}
		slog.Info("登录失败封禁已启用",
			"threshold", maxAuthFailures, "action", "永久封禁",
			"securityLog", securityLogPath())
		slog.Info("ingest 接口限流已启用", "limit", ingestRateLimit, "window", ingestRateWindow.String())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("API 服务错误", "err", err)
		}
	}()
	return srv
}

// withSecurity 在业务路由外层套两道防护：安全响应头 + IP 封禁拦截。
// 封禁检查放在最前面，比 IP 白名单和 token 校验都早——已被封的 IP 连一次业务
// 处理都不该消耗（否则封禁就失去了"止血"的意义）。
func withSecurity(next http.Handler, g *securityGuard, trustedProxies []string) http.Handler {
	return securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := realClientIP(r, trustedProxies)
		if g.IsBanned(ip) {
			g.RecordBannedHit(ip, r.URL.Path)
			writeErr(w, http.StatusForbidden, "该 IP 已被永久封禁，如为误封请在服务器执行 --unban 解封")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// writeJSON 输出 JSON 响应
func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// writeErr 输出错误响应
func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeProfileErr 把画像生成的错误映射成合适的 HTTP 状态码。
// ErrProfileBusy 表示同一联系人已有画像任务在跑，属于请求冲突而非服务端故障，
// 用 409 让桌面端能提示「正在生成中，请稍后」，而不是笼统的「服务器错误」。
func writeProfileErr(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrProfileBusy) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeErr(w, http.StatusInternalServerError, err.Error())
}

// route 路由分发（含 IP 白名单 + Bearer Token 认证）
func (s *apiServer) route(w http.ResponseWriter, r *http.Request) {
	// 1. IP 白名单检查（优先于认证，避免被未授权 IP 探测 token）
	if !checkIPWhitelist(r, s.cfg.APIWhitelist, s.cfg.TrustedProxies) {
		s.guard.RecordDenied(s.realIP(r), r.URL.Path, "whitelist")
		writeErr(w, http.StatusForbidden, "该 IP 未在白名单内，访问被拒绝")
		return
	}

	p := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/"), "/")
	parts := strings.Split(p, "/")

	// /api/auth/* 是网页登录专用通道：不经过 Bearer 认证（第一因素 token 放在请求体），
	// 但上面的 IP 白名单仍然生效。登录失败计数与 IP 封禁在 routeAuth 内部处理。
	if parts[0] == "auth" {
		s.routeAuth(w, r, parts[1:])
		return
	}

	// /api/calendar.ics 是日历订阅地址：手机/电脑日历客户端无法携带 Authorization 头，
	// 改由 URL 上的独立订阅密钥鉴权（见 calendar.go）。上面的 IP 白名单仍然生效。
	if p == "calendar.ics" {
		s.routeCalendarICS(w, r)
		return
	}

	// 2. 认证检查（status 接口也要求认证，防止端口扫描探测）
	//    接受两种凭证：
	//    a) 网页会话令牌 —— 网页端通过 apiToken + TOTP 双因素后颁发，有期限可吊销
	//    b) apiToken 本身 —— Windows 桌面端远程模式使用
	if s.cfg.APIToken != "" {
		tok := bearerToken(r)
		if !s.sessions.valid(tok) && !tokenMatches(tok, s.cfg.APIToken) {
			// 只审计不计数：桌面端 token 配错会高频重试，计入封禁会把用户自己封死
			s.guard.RecordDenied(s.realIP(r), r.URL.Path, "unauthorized")
			writeErr(w, http.StatusUnauthorized, "未授权：请先在网页完成 Token + 2FA 登录，或在请求头携带有效的 apiToken")
			return
		}
	}

	switch {
	case p == "status":
		s.hStatus(w, r)
	case parts[0] == "contacts" && len(parts) == 1 && r.Method == http.MethodGet:
		s.hListContacts(w, r)
	case parts[0] == "contacts" && len(parts) == 1 && r.Method == http.MethodPost:
		s.hCreateContact(w, r)
	case parts[0] == "contacts" && len(parts) == 2 && parts[1] == "resolve" && r.Method == http.MethodPost:
		s.hResolveContact(w, r)
	case parts[0] == "contacts" && len(parts) >= 2:
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "无效的联系人ID")
			return
		}
		s.routeContact(w, r, id, parts[2:])
	case parts[0] == "merge" && len(parts) == 1 && r.Method == http.MethodPost:
		s.hMerge(w, r)
	case parts[0] == "merge" && len(parts) == 2 && parts[1] == "undo" && r.Method == http.MethodPost:
		s.hUndoMerge(w, r)
	case parts[0] == "merge" && len(parts) == 2 && parts[1] == "logs" && r.Method == http.MethodGet:
		s.hMergeLogs(w, r)
	case parts[0] == "backup" && len(parts) == 2 && parts[1] == "export" &&
		(r.Method == http.MethodGet || r.Method == http.MethodPost):
		// GET 导出明文备份；POST 可在请求体里带口令，导出「密钥文件已加密」的备份
		s.hBackupExport(w, r)
	case parts[0] == "backup" && len(parts) == 2 && parts[1] == "import" && r.Method == http.MethodPost:
		s.hBackupImport(w, r)
	case parts[0] == "backup" && len(parts) == 2 && parts[1] == "logs" && r.Method == http.MethodGet:
		s.hBackupLogs(w, r)
	case parts[0] == "assistant":
		s.routeAssistant(w, r, parts[1:])
	case parts[0] == "archive":
		s.routeArchive(w, r, parts[1:])
	case parts[0] == "trusted":
		s.routeTrusted(w, r, parts[1:])
	case parts[0] == "tags":
		s.routeTags(w, r, parts[1:])
	case parts[0] == "search":
		s.routeSearch(w, r, parts[1:])
	case parts[0] == "insights":
		s.routeInsights(w, r, parts[1:])
	case parts[0] == "insight":
		s.routeInsight(w, r, parts[1:])
	case parts[0] == "relationships":
		s.routeRelationships(w, r, parts[1:])
	case parts[0] == "life":
		s.routeLife(w, r, parts[1:])
	case parts[0] == "data":
		s.routeData(w, r, parts[1:])
	case parts[0] == "system":
		s.routeSystem(w, r, parts[1:])
	case parts[0] == "ingest" && r.Method == http.MethodPost:
		s.hIngest(w, r)
	default:
		writeErr(w, http.StatusNotFound, "未知接口: "+r.URL.Path)
	}
}

// routeContact /api/contacts/{id}/... 子路由
func (s *apiServer) routeContact(w http.ResponseWriter, r *http.Request, id int64, sub []string) {
	if len(sub) == 0 {
		switch r.Method {
		case http.MethodGet:
			s.hGetContact(w, r, id)
		case http.MethodDelete:
			s.hDeleteContact(w, r, id)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "不支持的方法")
		}
		return
	}
	switch sub[0] {
	case "messages":
		s.hGetMessages(w, r, id)
	case "stats":
		s.hGetStats(w, r, id)
	case "history":
		s.hGetHistory(w, r, id)
	case "remark":
		s.hSetRemark(w, r, id)
	case "name":
		s.hSetName(w, r, id)
	case "profile":
		s.hEditProfile(w, r, id)
	case "supplement":
		s.hSupplement(w, r, id)
	case "regenerate":
		s.hRegenerate(w, r, id)
	case "rewrite", "review-draft", "profile-changes":
		s.hAssistance(w, r, id, sub[0])
	case "analyze":
		s.hAnalyze(w, r, id)
	case "tags":
		s.routeContactTags(w, r, id)
	case "timeline":
		s.routeContactTimeline(w, r, id)
	case "rehearsal":
		s.routeContactRehearsal(w, r, id, sub[1:])
	case "facts":
		s.routeContactFacts(w, r, id, sub[1:])
	case "connections":
		s.routeContactConnections(w, r, id)
	case "trend":
		s.routeContactTrend(w, r, id)
	case "quality":
		s.routeContactQuality(w, r, id)
	case "achievements":
		s.routeContactAchievements(w, r, id)
	case "timing":
		s.hContactTiming(w, r, id)
	case "rhythm":
		s.hContactRhythm(w, r, id)
	case "heatmap":
		s.hContactHeatmap(w, r, id)
	case "mirror":
		s.hContactMirror(w, r, id)
	case "topics":
		s.routeContactTopics(w, r, id, sub[1:])
	case "ask":
		s.routeContactAsk(w, r, id)
	case "summary":
		s.routeContactSummary(w, r, id)
	case "narrative":
		s.hContactNarrative(w, r, id)
	default:
		writeErr(w, http.StatusNotFound, "未知接口")
	}
}

func (s *apiServer) hEditProfile(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		writeErr(w, 405, "请使用 PUT")
		return
	}
	if id <= 0 {
		writeErr(w, 400, "无效的联系人ID")
		return
	}
	var input struct {
		Profile json.RawMessage `json:"profile"`
		Base    *string         `json:"baseProfileJson"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeErr(w, 400, "请求格式错误: "+err.Error())
		return
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		writeErr(w, 400, "请求只能包含一个 JSON 对象")
		return
	}
	if input.Base == nil || len(input.Profile) == 0 || bytes.Equal(bytes.TrimSpace(input.Profile), []byte("null")) {
		writeErr(w, 400, "必须提供 profile 对象和 baseProfileJson")
		return
	}
	// 避免模型兼容解析器静默丢弃用户填写的重要日期。
	var shape struct {
		BasicInfo struct {
			ImportantDates []string `json:"important_dates"`
		} `json:"basic_info"`
	}
	if err := json.Unmarshal(input.Profile, &shape); err != nil {
		writeErr(w, 400, "重要日期必须为字符串列表")
		return
	}
	var profile Profile
	pd := json.NewDecoder(bytes.NewReader(input.Profile))
	pd.DisallowUnknownFields()
	if err := pd.Decode(&profile); err != nil {
		writeErr(w, 400, "画像字段格式错误: "+err.Error())
		return
	}
	for key := range profile.IntentPatterns {
		if strings.TrimSpace(key) == "" {
			writeErr(w, 400, "意图名称不能为空")
			return
		}
	}
	if err := EditProfile(s.db, id, profile, *input.Base); err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeErr(w, 404, "联系人不存在")
		case errors.Is(err, ErrProfileConflict), errors.Is(err, ErrProfileStale):
			writeErr(w, 409, err.Error())
		default:
			writeErr(w, 500, "保存画像失败")
		}
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- 联系人查询 ----

// contactJSON 联系人 API 输出结构
type contactJSON struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	Remark         string   `json:"remark"`
	ProfileJSON    string   `json:"profileJson,omitempty"`
	ProfileSummary string   `json:"profileSummary"`
	OtherMsgCount  int      `json:"otherMsgCount"`
	LastUpdated    string   `json:"lastUpdated"`
	CreatedAt      string   `json:"createdAt"`
	MergedInto     int64    `json:"mergedInto"`
	MergeCount     int      `json:"mergeCount"`
	Aliases        []string `json:"aliases,omitempty"`
	// tags 不带 omitempty：键必须恒存在且是数组，网页端直接读 c.tags.length，
	// 少一个键或给 null 都会抛 TypeError 让整个页面白屏
	Tags []ContactTag `json:"tags"`
}

func toContactJSON(c *Contact) contactJSON {
	return contactJSON{
		ID: c.ID, Name: c.Name, Remark: c.Remark,
		ProfileSummary: c.ProfileSummary, OtherMsgCount: c.OtherMsgCount,
		LastUpdated: c.LastUpdated, CreatedAt: c.CreatedAt,
		MergedInto: c.MergedInto, MergeCount: c.MergeCount, Aliases: c.Aliases,
		Tags: []ContactTag{},
	}
}

func (s *apiServer) hListContacts(w http.ResponseWriter, r *http.Request) {
	includeMerged := r.URL.Query().Get("includeMerged") == "1"
	q := r.URL.Query()
	tagIDs := parseTagIDs(q.Get("tags"))
	// 带分页/搜索参数时走分页响应 {list,total,offset,limit}；裸调用保持返回数组，
	// 兼容桌面端远程模式（它一次性拿全量在本地过滤）。
	paged := q.Get("paged") == "1" || q.Get("q") != "" || q.Get("offset") != "" || q.Get("limit") != "" || len(tagIDs) > 0
	if !paged {
		contacts, err := GetAllContacts(s.db, includeMerged)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out := make([]contactJSON, 0, len(contacts))
		for i := range contacts {
			out = append(out, toContactJSON(&contacts[i]))
		}
		// 全量分支也要带标签：网页端联系人列表、标签筛选条都依赖 tags 字段
		attachContactTags(s.db, out)
		writeJSON(w, 200, out)
		return
	}

	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	contacts, total, err := GetContactsPageFiltered(s.db, includeMerged, q.Get("q"), tagIDs, offset, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > 200 {
		limit = 30 // 与 GetContactsPage 的默认/上限保持一致
	}
	out := make([]contactJSON, 0, len(contacts))
	for i := range contacts {
		out = append(out, toContactJSON(&contacts[i]))
	}
	attachContactTags(s.db, out)
	writeJSON(w, 200, map[string]interface{}{
		"list": out, "total": total, "offset": offset, "limit": limit,
	})
}

func (s *apiServer) hGetContact(w http.ResponseWriter, r *http.Request, id int64) {
	c, err := GetContactByIDWithMerged(s.db, id)
	if err != nil {
		writeErr(w, 404, "联系人不存在")
		return
	}
	logs, _ := GetMergeLogsForTarget(s.db, id)
	cj := toContactJSON(c)
	cj.ProfileJSON = c.ProfileJSON
	cj.MergeCount = len(logs)
	if tags, terr := GetContactTags(s.db, id); terr == nil {
		cj.Tags = tags
	}
	writeJSON(w, 200, cj)
}

func (s *apiServer) hGetMessages(w http.ResponseWriter, r *http.Request, id int64) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	if limit > maxMessagesPageLimit {
		limit = maxMessagesPageLimit
	}

	type msgJSON struct {
		ID      int64  `json:"id"`
		Sender  string `json:"sender"`
		Content string `json:"content"`
		MsgTime string `json:"msgTime"`
	}

	// 优先走 keyset 分页（带 beforeId）：避免深页 OFFSET 全表扫。
	// 未带 beforeId 时保留旧的 offset 语义（向后兼容其它调用方 / 桌面远程模式）。
	if v := q.Get("beforeId"); v != "" {
		beforeID, err := strconv.ParseInt(v, 10, 64)
		if err != nil || beforeID < 0 {
			writeErr(w, http.StatusBadRequest, "beforeId 必须是非负整数")
			return
		}
		msgs, err := GetMessagesPageBefore(s.db, id, beforeID, limit)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out := make([]msgJSON, 0, len(msgs))
		for _, m := range msgs {
			out = append(out, msgJSON{m.ID, m.Sender, m.Content, m.Timestamp.Format("2006-01-02 15:04:05")})
		}
		writeJSON(w, 200, out)
		return
	}

	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	msgs, err := GetMessagesPage(s.db, id, offset, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]msgJSON, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, msgJSON{m.ID, m.Sender, m.Content, m.Timestamp.Format("2006-01-02 15:04:05")})
	}
	writeJSON(w, 200, out)
}

func (s *apiServer) hGetStats(w http.ResponseWriter, r *http.Request, id int64) {
	stats, err := GetContactStats(s.db, id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, stats)
}

func (s *apiServer) hGetHistory(w http.ResponseWriter, r *http.Request, id int64) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := GetProfileHistory(s.db, id, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, list)
}

// ---- 联系人写操作 ----

func readBody(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, 400, "请求体解析失败: "+err.Error())
		return false
	}
	return true
}

func (s *apiServer) hCreateContact(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, 400, "名称不能为空")
		return
	}
	id, err := GetOrCreateContact(s.db, req.Name)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]int64{"id": id})
}

func (s *apiServer) hResolveContact(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !readBody(w, r, &req) {
		return
	}
	id, viaAlias, err := ResolveContactID(s.db, req.Name)
	if err != nil {
		writeErr(w, 404, "联系人不存在")
		return
	}
	writeJSON(w, 200, map[string]interface{}{"id": id, "viaAlias": viaAlias})
}

func (s *apiServer) hSetRemark(w http.ResponseWriter, r *http.Request, id int64) {
	var req struct {
		Remark string `json:"remark"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if err := UpdateContactRemark(s.db, id, req.Remark); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	RecordContactEvent(s.db, id, "remark", "修改备注", "备注改为「"+strings.TrimSpace(req.Remark)+"」", time.Now())
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *apiServer) hSetName(w http.ResponseWriter, r *http.Request, id int64) {
	var req struct {
		Name string `json:"name"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if err := UpdateContactName(s.db, id, req.Name); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	RecordContactEvent(s.db, id, "renamed", "修改昵称", "昵称改为「"+strings.TrimSpace(req.Name)+"」", time.Now())
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *apiServer) hDeleteContact(w http.ResponseWriter, r *http.Request, id int64) {
	if err := DeleteContactByID(s.db, id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- LLM 操作（服务端执行，桌面端无需 API Key） ----

func (s *apiServer) hSupplement(w http.ResponseWriter, r *http.Request, id int64) {
	var req struct {
		Note string `json:"note"`
	}
	if !readBody(w, r, &req) {
		return
	}
	contact, err := GetContactByID(s.db, id)
	if err != nil {
		writeErr(w, 404, "联系人不存在")
		return
	}
	if err := SupplementProfile(r.Context(), s.db, s.llm, id, contact.Name, req.Note); err != nil {
		writeProfileErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *apiServer) hRegenerate(w http.ResponseWriter, r *http.Request, id int64) {
	contact, err := GetContactByID(s.db, id)
	if err != nil {
		writeErr(w, 404, "联系人不存在")
		return
	}
	msgs, err := GetAllMessages(s.db, id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := GenerateOrUpdateProfile(r.Context(), s.db, s.llm, id, contact.Name, msgs); err != nil {
		writeProfileErr(w, err)
		return
	}
	updated, _ := GetContactByID(s.db, id)
	writeJSON(w, 200, map[string]string{"ok": "true", "summary": updated.ProfileSummary})
}

func (s *apiServer) hAnalyze(w http.ResponseWriter, r *http.Request, id int64) {
	var req struct {
		Message string `json:"message"`
	}
	if !readBody(w, r, &req) {
		return
	}
	msgs, err := GetRecentMessages(s.db, id, 50)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	result, err := AnalyzeIntent(r.Context(), s.db, s.llm, id, req.Message, msgs)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, result)
}

// ---- 合并 ----

func (s *apiServer) hMerge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceID      int64 `json:"sourceId"`
		TargetID      int64 `json:"targetId"`
		UseSourceName bool  `json:"useSourceName"`
		Regenerate    bool  `json:"regenerate"`
	}
	if !readBody(w, r, &req) {
		return
	}
	result, err := MergeContacts(s.db, req.SourceID, req.TargetID, MergeOptions{
		UseSourceNameAsDisplay: req.UseSourceName,
		RegenerateProfile:      req.Regenerate,
	})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 合并后异步重生成画像（去重）
	if req.Regenerate {
		if _, loaded := profileInFlight.LoadOrStore(req.TargetID, true); !loaded {
			go func() {
				defer profileInFlight.Delete(req.TargetID)
				// 用脱离请求的 context：响应写完后 r.Context() 立即取消，
				// 直接传它会让后台任务刚起步就被打断。
				ctx, cancel := context.WithTimeout(context.Background(), backgroundProfileTimeout)
				defer cancel()

				target, err := GetContactByID(s.db, req.TargetID)
				if err != nil {
					slog.Error("合并后重生成画像失败: 读取联系人", "err", err)
					return
				}
				msgs, err := GetAllMessages(s.db, req.TargetID)
				if err != nil {
					slog.Error("合并后重生成画像失败: 读取消息", "err", err)
					return
				}
				if err := GenerateOrUpdateProfile(ctx, s.db, s.llm, req.TargetID, target.Name, msgs); err != nil {
					slog.Error("合并后重生成画像失败", "err", err)
				}
			}()
		}
	}
	writeJSON(w, 200, result)
}

func (s *apiServer) hUndoMerge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LogID int64 `json:"logId"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if err := UndoMerge(s.db, req.LogID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *apiServer) hMergeLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if targetIDStr := q.Get("targetId"); targetIDStr != "" {
		targetID, err := strconv.ParseInt(targetIDStr, 10, 64)
		if err != nil {
			writeErr(w, 400, "无效的 targetId")
			return
		}
		logs, err := GetMergeLogsForTarget(s.db, targetID)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, logs)
		return
	}
	logs, err := GetMergeLogs(s.db, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, logs)
}

// ---- 聊天记录识别（核心接口） ----

// IngestOutcome 识别结果（bot 和 API 共用）
type IngestOutcome struct {
	Contact          *Contact
	ParsedCount      int
	NewCount         int
	ViaAlias         bool
	ProfileTriggered bool
	Messages         []Message
}

// ingestAndStore 解析聊天记录、推断联系人、存库、按需触发画像更新
func ingestAndStore(db *sql.DB, cfg *Config, llm *LLMClient, text string) (*IngestOutcome, error) {
	messages := ParseClipboard(text, cfg.MyName)
	var valid []Message
	for _, m := range messages {
		if strings.TrimSpace(m.Content) != "" {
			valid = append(valid, m)
		}
	}
	if len(valid) == 0 {
		return nil, fmt.Errorf("未能解析出有效聊天记录")
	}

	contactName := InferContactName(valid, cfg.MyName)
	if contactName == "" {
		return nil, fmt.Errorf("无法识别对方昵称")
	}

	cid, viaAlias, err := ResolveContactID(db, contactName)
	if err != nil {
		return nil, fmt.Errorf("解析联系人失败: %w", err)
	}

	newCount, err := SaveMessages(db, cid, valid)
	if err != nil {
		return nil, fmt.Errorf("保存消息失败: %w", err)
	}

	contact, err := GetContactByID(db, cid)
	if err != nil {
		return nil, fmt.Errorf("读取联系人失败: %w", err)
	}

	outcome := &IngestOutcome{
		Contact: contact, ParsedCount: len(valid),
		NewCount: newCount, ViaAlias: viaAlias, Messages: valid,
	}

	// 隐式反馈：若本次有 sender="me" 的新消息入库，自动标记该联系人的 open 建议为已执行
	if newCount > 0 {
		hasMe := false
		for _, m := range valid {
			if m.Sender == "me" {
				hasMe = true
				break
			}
		}
		if hasMe {
			// 先刷新日指标再取 trend_before：SaveMessages 不自动重算聚合（惰性自愈）。
			// 基线必须包含「触发已联系判定的这次发消息本身」，
			// 否则 14 天后回测时今天的数据又被算进 trend_after，凭空造出回暖。
			if _, err := RebuildDailyMetrics(db, cid); err != nil {
				slog.Warn("隐式反馈：刷新日指标失败", "contactId", cid, "err", err)
			}
			autoMarkSuggestionActed(db, cid, time.Now())
		}
	}

	// 达到阈值则后台更新画像（去重：同一联系人已有生成任务则跳过，避免并发浪费 LLM 调用）
	if ShouldGenerateProfile(db, cid) || ShouldUpdateProfile(db, cid) {
		if _, loaded := profileInFlight.LoadOrStore(cid, true); !loaded {
			outcome.ProfileTriggered = true
			go func() {
				defer profileInFlight.Delete(cid)
				// 用脱离请求的 context：响应写完后 r.Context() 立即取消，
				// 直接透传会让后台任务刚起步就被打断；但必须自带超时，
				// 否则 LLM 端挂住时 profileInFlight 里的槽位会被永久占用，
				// 该联系人再也无法触发画像更新。
				ctx, cancel := context.WithTimeout(context.Background(), backgroundProfileTimeout)
				defer cancel()

				allMsgs, err := GetAllMessages(db, cid)
				if err != nil {
					slog.Error("后台生成画像失败: 读取消息", "err", err)
					return
				}
				if err := GenerateOrUpdateProfile(ctx, db, llm, cid, contact.Name, allMsgs); err != nil {
					slog.Error("后台生成画像失败", "err", err)
				}
			}()
		}
	}
	return outcome, nil
}

// hIngest 桌面端粘贴聊天记录的入口：解析 + 存库 + 可选意图分析
func (s *apiServer) hIngest(w http.ResponseWriter, r *http.Request) {
	// 限流：ingest 每被调用一次就真实消耗一次 LLM 额度，是全站最"贵"的接口。
	// apiToken 一旦泄露，没有次数上限就意味着账单能在几小时内被脚本刷爆。
	// 按 token 维度计数（同一 token 的不同来源共享额度），取不到 token 时退回按 IP。
	// 正常用法（人工粘贴聊天记录）一分钟也就几次，120 次的上限碰不到。
	rlKey := bearerToken(r)
	if rlKey == "" {
		rlKey = s.realIP(r)
	}
	if ok, retry := s.ingestRL.Allow(rlKey); !ok {
		s.guard.RecordDenied(s.realIP(r), "/api/ingest", "触发限流")
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		writeErr(w, http.StatusTooManyRequests,
			fmt.Sprintf("请求过于频繁（每分钟最多 %d 次），请 %d 秒后重试", ingestRateLimit, retry))
		return
	}

	var req struct {
		Text    string `json:"text"`
		Analyze bool   `json:"analyze"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeErr(w, 400, "text 不能为空")
		return
	}

	outcome, err := ingestAndStore(s.db, s.cfg, s.llm, req.Text)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	resp := map[string]interface{}{
		"contactId":        outcome.Contact.ID,
		"contactName":      outcome.Contact.Name,
		"displayName":      displayName(outcome.Contact),
		"parsedCount":      outcome.ParsedCount,
		"newCount":         outcome.NewCount,
		"otherMsgCount":    outcome.Contact.OtherMsgCount,
		"viaAlias":         outcome.ViaAlias,
		"profileTriggered": outcome.ProfileTriggered,
		"hasProfile":       outcome.Contact.ProfileJSON != "" && outcome.Contact.ProfileJSON != "{}",
		"coldStartCount":   s.cfg.Profile.ColdStartCount,
	}

	// 同步意图分析（桌面端结果窗需要）
	if req.Analyze {
		var latestOther string
		for i := len(outcome.Messages) - 1; i >= 0; i-- {
			if outcome.Messages[i].Sender == "other" {
				latestOther = outcome.Messages[i].Content
				break
			}
		}
		if latestOther != "" {
			// 意图分析要等一轮完整的 LLM 调用（最坏 ~122s），而桌面端是同步等这个响应的。
			// 给一个略小于客户端超时的硬上限：超时后仍然把已完成的入库结果返回给调用方，
			// 把失败原因单独放进 intentError，而不是让整个请求 504 掉。
			ctx, cancel := context.WithTimeout(r.Context(), ingestAnalyzeTimeout)
			intent, err := AnalyzeIntent(ctx, s.db, s.llm, outcome.Contact.ID, latestOther, outcome.Messages)
			cancel()
			if err == nil {
				resp["intent"] = intent
			} else {
				resp["intentError"] = err.Error()
			}
		}
	}
	writeJSON(w, 200, resp)
}

// ---- 状态 ----

func (s *apiServer) hStatus(w http.ResponseWriter, r *http.Request) {
	contacts, _ := GetAllContacts(s.db, false)

	// 消息计数从 metrics 聚合（归档感知，口径与 datareport 统一）。与 datareport 同纪律：
	// 持锁内先自愈（刚升级指标未建时报 0 会与其他面板不一致），GetAllContacts 已取尽释锁，此处不嵌套。
	var messages int64
	dbMu.Lock()
	ensureDailyMetricsSeededLocked(s.db)
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(me_count+other_count),0) FROM relationship_daily_metrics`).Scan(&messages); err != nil {
		slog.Warn("状态接口统计消息数失败", "err", err)
	}
	dbMu.Unlock()

	loggedIn, sessionExpired := false, false
	if s.client != nil {
		loggedIn = s.client.IsLoggedIn()
		sessionExpired = s.client.SessionExpired()
	}

	// 磁盘统计以数据文件所在目录为准（Docker 下即 /config 挂载卷）
	dataDir := filepath.Dir(dbPath())
	sysInfo := CollectSysInfo(dataDir)

	writeJSON(w, 200, map[string]interface{}{
		"ok":             true,
		"version":        appVersion,
		"contacts":       len(contacts),
		"messages":       messages,
		"loggedIn":       loggedIn,
		"sessionExpired": sessionExpired,
		"poll":           snapshotPollStatus(),
		"serverTime":     time.Now().Format(time.RFC3339),
		// clientIp 是服务端按白名单/封禁口径识别到的真实访客 IP；
		// 反代部署时用它核对 XFF 解析是否符合预期（应显示你的公网 IP 而非反代 IP）
		"clientIp":         s.realIP(r),
		"trustedProxy":     len(s.cfg.TrustedProxies) > 0,
		"whitelistEnabled": len(s.cfg.APIWhitelist) > 0,
		"sys":              sysInfo,
	})
}
