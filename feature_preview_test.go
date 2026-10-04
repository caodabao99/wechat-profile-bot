package main

// 网页面板「活体预览服务器」——仅用于人工/浏览器 UI 走查（PC + 手机视口），
// 用真实静态资源 + 真实路由 + 一份完整样例数据把面板跑起来，便于肉眼排查
// 按钮被挤没、表格横向溢出、弹窗超框、点击区过小等响应式 bug。
//
// 默认【跳过】，不会影响 go test ./... 与 CI。只有显式设环境变量才启动并阻塞：
//
//	PANEL_PREVIEW=1 go test -run TestPanelPreviewServer -timeout 40m -v
//
// 然后浏览器打开 http://127.0.0.1:8099 ，登录页里 Token 随便填（本预览把
// apiToken 置空，走「未配置 token 直接颁发会话」分支，无需 2FA）。

import (
	"database/sql"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestPanelPreviewServer(t *testing.T) {
	if os.Getenv("PANEL_PREVIEW") == "" {
		t.Skip("PANEL_PREVIEW 未设置：跳过活体预览服务器（需要时 PANEL_PREVIEW=1 运行）")
	}

	db := regressionDB(t)
	if err := ensureAllTablesForPreview(db); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	seedPreviewData(t, db)

	// LLM 指向一个立即失败的桩，避免任何端点真的去联网/挂起。
	deadLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no model in preview", http.StatusBadGateway)
	}))
	defer deadLLM.Close()

	// apiToken 留空 → 网页登录直接颁发会话（跳过 2FA），方便浏览器一键进应用。
	cfg := &Config{}
	cfg.LLM.BaseURL = deadLLM.URL
	cfg.LLM.Model = "preview"
	s := &apiServer{
		db:       db,
		cfg:      cfg,
		llm:      NewLLMClient(cfg),
		sessions: &webSessionStore{sessions: map[string]time.Time{}},
		guard:    newSecurityGuard(),
		ingestRL: newRateLimiter(1000, time.Minute),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/", s.route)
	mux.HandleFunc("/assets/", handleAssets)
	mux.HandleFunc("/", handleWebUI)
	handler := withSecurity(mux, s.guard, cfg.TrustedProxies)

	addr := os.Getenv("PANEL_PORT")
	if addr == "" {
		addr = "127.0.0.1:8099"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("监听 %s 失败（端口被占？换 PANEL_PORT=127.0.0.1:xxxx）: %v", addr, err)
	}
	srv := &http.Server{Handler: handler}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			t.Errorf("预览服务器退出: %v", err)
		}
	}()

	t.Logf("✅ 面板预览已就绪 → http://%s/  （Token 随便填即可登录）", addr)
	t.Logf("   建议视口：PC 1440x900、平板 820x1180、手机 390x844(iPhone14) / 360x780")

	// 阻塞直到 go test 超时或被 Ctrl-C，让浏览器有足够时间逐页走查。
	deadline := time.Now().Add(35 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
	}
	_ = srv.Close()
}

// ensureAllTablesForPreview 建齐面板各页所需的全部表。
func ensureAllTablesForPreview(db *sql.DB) error {
	for _, fn := range []func(*sql.DB) error{
		ensureTagTables, ensureTimelineTables, ensureFollowupTables,
		ensureAssistantTables, ensureArchiveTables, ensureModePresetTables,
	} {
		if err := fn(db); err != nil {
			return err
		}
	}
	return nil
}

// seedPreviewData 灌入一份“故意刁钻”的样例数据：长中文名、超长昵称、长摘要、
// 长消息、多种标签/状态、失败与成功的备份/邮件日志、归档、模式预设……
// 目的是把表格、卡片、表单、弹窗在小屏下的溢出/换行/挤压问题暴露出来。
func seedPreviewData(t *testing.T, db *sql.DB) {
	t.Helper()
	now := time.Now()

	profiles := []struct {
		name string
		pj   string
		sum  string
	}{
		{"超长中文昵称测试联系人张三李四王五赵六", `{"basic_info":{"occupation":"高级产品经理兼用户体验研究员","location":"北京市海淀区中关村科技园","important_dates":["1990-05-01","2015-09-10"]},"interests":["攀岩","摄影","爵士乐","手冲咖啡"],"important_facts":["有一个上小学的女儿","最近在准备跳槽"],"summary":"沟通直接、逻辑缜密、注重细节"}`, "沟通直接、逻辑缜密"},
		{"Alice Aaaaaaaaaa-Bbbbbbbbbb", `{"basic_info":{"occupation":"Freelance Designer","location":"Shanghai","important_dates":["1988-02-29"]},"interests":["hiking"],"important_facts":["vegan"],"summary":"warm, minimal"}`, "warm, minimal"},
		{"妈妈", `{"basic_info":{"occupation":"退休教师","location":"成都","important_dates":["1962-08-15"]},"interests":["广场舞","养花"],"important_facts":["有高血压，需定期复查"],"summary":"关心我，常唠叨吃饭穿衣"}`, "关心家人"},
		{"客户-某科技CEO-王总", `{"basic_info":{"occupation":"创始人兼CEO","location":"深圳南山区","important_dates":[]},"interests":["高尔夫","投资"],"important_facts":["决策快、看重ROI","不喜欢冗长汇报"],"summary":"结果导向，节奏快，讨厌废话"}`, "结果导向"},
		{"同事李", "", ""},
	}

	var ids []int64
	for i, p := range profiles {
		id := regressionContact(t, db, p.name)
		ids = append(ids, id)
		if p.pj != "" {
			if err := SaveProfile(db, id, p.pj, p.sum, "seed"); err != nil {
				t.Logf("SaveProfile %s: %v", p.name, err)
			}
		}
		// 每人若干条消息，含超长内容 + emoji + CJK，压一压消息气泡/表格。
		long := "这是一条故意写得非常长的消息用来测试在手机上气泡会不会溢出边界换行是否正常" +
			"以及超长英文单词supercalifragilisticexpialidocious和URL https://example.com/very/long/path?a=1&b=2 能否被优雅地折行"
		texts := []string{
			"周末有空吗？想约你聊聊",
			long,
			"好的收到👍 那咱们下周三下午三点，会议室A，记得带上上季度的数据和竞品分析报告哦～",
			"攀岩训练第" + string(rune('0'+i)) + "次记录",
		}
		regressionMessages(t, db, id, texts...)
		// 再灌一批带时间跨度的消息，供统计/趋势/时间线渲染。
		for j := 0; j < 12; j++ {
			if _, err := SaveMessages(db, id, []Message{
				{Sender: "other", Content: "日常分享一件小事记录编号" + string(rune('A'+j)), Timestamp: now.AddDate(0, 0, -40+j)},
				{Sender: "me", Content: "收到反馈回应" + string(rune('A'+j)), Timestamp: now.AddDate(0, 0, -40+j)},
			}); err != nil {
				t.Logf("SaveMessages %d/%d: %v", id, j, err)
			}
		}
		if _, _, err := RebuildFactsAndEvidence(db, id); err != nil {
			t.Logf("RebuildFacts %d: %v", id, err)
		}
		if _, err := RebuildDailyMetrics(db, id); err != nil {
			t.Logf("RebuildDailyMetrics %d: %v", id, err)
		}
	}

	// 标签 + 给联系人打标签
	tagNames := []string{"重要", "家人", "工作", "潜在客户", "待跟进超长标签名字示例"}
	var tagIDs []int64
	for _, n := range tagNames {
		if tg, err := CreateTag(db, n); err == nil && tg != nil {
			tagIDs = append(tagIDs, int64(tg.ID))
		}
	}
	if len(tagIDs) > 0 && len(ids) > 0 {
		_ = SetContactTags(db, ids[0], tagIDs)
	}

	// 待跟进若干条（含带金额/不同类型）
	if len(ids) > 0 {
		_, _ = insertFollowup(db, ids[0], "meeting", "确认下周三的会面是否改期", "", now.Format(time.RFC3339), now.Format(time.RFC3339))
		_, _ = insertFollowup(db, ids[3], "money", "客户报价单待回复", "12800", now.Format(time.RFC3339), now.Format(time.RFC3339))
	}

	// 助手设置（填 SMTP 让看板显示“已配置”）
	as := defaultAssistantSettings()
	as.Enabled = true
	as.SMTP = AssistantSMTP{Host: "smtp.example.com", Port: 465, SSL: true, User: "bot@example.com", Pass: "secret", From: "bot@example.com", To: []string{"me@example.com", "partner@example.com"}}
	as.CalendarKey = "previewcalkey1234567890"
	_ = saveAssistantSettings(db, as)
	_ = saveArchiveSettings(db, ArchiveSettings{Enabled: true, RetentionDays: 60})

	// 邮件发送日志：成功 + 失败，压表
	logEmail(db, "daily", "关系日报 · 2026-10-04", "me@example.com", "success", "")
	logEmail(db, "daily", "关系日报 · 2026-10-03", "me@example.com", "failed", "SMTP auth timeout，请检查应用专用密码是否过期或被服务商策略拦截")

	// 备份日志：成功/失败/加密
	LogBackupAction(db, "export", "web", "wechat-profile-backup-20261004-080000.zip", 3_145_728, "手动导出", true)
	LogBackupAction(db, "import", "web", "wechat-profile-backup-20261001-231500.zip", 2_621_440, "恢复失败：目标库版本不兼容（user_version=9 高于当前支持版本）", false)
	LogBackupAction(db, "export", "wechat", "wechat-profile-backup-20260930-070000.zip", 2_097_152, "每日自动备份（含口令加密旁路）", true)

	// 归档一批老消息，让归档页有内容
	if _, err := RunArchive(db, 30); err != nil {
		t.Logf("RunArchive: %v", err)
	}

	// 自定义模式预设一个（列表就有 内置 + 自定义 两类）
	if _, err := SaveModePreset(db, "我的自定义模式"); err != nil {
		t.Logf("SaveModePreset: %v", err)
	}

	t.Logf("预览数据就绪：联系人=%d 标签=%d 备份日志=3 邮件日志=2", len(ids), len(tagIDs))
}
