package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	// 安全相关的命令行开关（解封 / 查看名单 / 帮助）。
	// 必须在 setupLogging 和 LoadConfig 之前处理：否则仅仅为了改一条封禁记录，
	// 就会顺带创建 bot.log、生成 config.json 并写入一个全新的 apiToken。
	if handleSecurityCLI() {
		return
	}

	// 日志（控制台 + bot.log），后续所有运行信息都走 slog
	logFile := setupLogging()
	if logFile != nil {
		defer logFile.Close()
	}

	// 加载配置
	cfg, err := LoadConfig()
	if err != nil {
		slog.Error("配置错误", "err", err)
		os.Exit(1)
	}

	// 初始化数据库
	dbFilePath := dbPath()
	db, err := InitDB(dbFilePath)
	if err != nil {
		slog.Error("数据库初始化失败", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	slog.Info("数据库已就绪", "path", dbFilePath)

	// LLM 客户端（接入 db 后支持网页端运行时切换模型/推理开关/代理与用量统计，未配置档案时回落 config.json）
	llmClient := NewLLMClient(cfg).WithDB(db)
	if err := ensureLLMSettingsTable(db); err != nil {
		slog.Warn("模型与代理设置表初始化失败", "err", err)
	}

	// iLink 客户端
	credPath := credentialPath()
	client := NewILinkClient(credPath)

	// 尝试加载已有凭据
	if err := client.LoadCredentials(); err != nil {
		if os.IsNotExist(err) {
			slog.Info("未检测到已保存的登录凭据，需要扫码登录")
		} else {
			slog.Warn("登录凭据读取失败，需要重新扫码登录", "err", err)
		}
	} else {
		slog.Info("已加载保存的登录凭据")
	}

	// 首次绑定放到后面、API 服务就绪之后后台进行（见下方“开始消息轮询”前）：
	// 否则在 NAS 上会出现“进程在等扫码、面板还打不开”的空窗，看起来就像启动失败。

	// 加载 context token 缓存
	// 收取账本与游标回读（at-least-once 的前提）：重启后从「上次已成功处理」的位置续拉，
	// 既不丢消息，也不会因为空游标把历史消息整段重放一遍。
	if err := ensureIngestLedger(db); err != nil {
		slog.Warn("收取账本初始化失败（会退化为无持久去重，不影响收消息本身）", "err", err)
	}
	client.LoadCursor()
	// 文件不存在是首次启动的正常情况；文件存在但解析失败要告警——
	// 那意味着缓存损坏，所有用户都发不出主动消息，得让用户知道
	if err := client.LoadContextTokens(); err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("context_token 缓存加载失败，重启后需要对方先发消息才能回复", "err", err)
		}
	}

	// 创建命令处理器
	bot := NewBot(db, llmClient, client, cfg)

	// 关系助手（网页端增值功能）：建表 + 定时任务；未启用时调度器只读一行配置即返回，无副作用
	if err := ensureAssistantTables(db); err != nil {
		slog.Warn("关系助手数据表初始化失败", "err", err)
	} else {
		startAssistantScheduler(db, llmClient)
	}

	// 消息归档：建表 + 每日自动归档调度（未启用时调度器只读一行配置即返回）
	if err := ensureArchiveTables(db); err != nil {
		slog.Warn("消息归档数据表初始化失败", "err", err)
	} else {
		// 归档调度器要能随进程退出：这个 defer 注册在 db.Close() 之后，
		// 按 LIFO 会先于关库执行，不会留下「库已关还在写事务」的 goroutine
		archiveStop := make(chan struct{})
		archiveDone := startArchiveScheduler(db, archiveStop)
		defer func() {
			close(archiveStop)
			<-archiveDone
		}()
	}

	// 网页端增值功能数据表：标签 / 联系人时间线 / 待跟进。
	// 建表失败只告警不退出——核心记录与画像功能不依赖这三张表，
	// 相关接口在查询时会自行容忍「no such table」。
	for _, init := range []struct {
		name string
		fn   func(*sql.DB) error
	}{
		{"联系人标签", ensureTagTables},
		{"联系人时间线", ensureTimelineTables},
		{"待跟进事项", ensureFollowupTables},
		{"提示词模板", ensurePromptTemplates},
	} {
		if err := init.fn(db); err != nil {
			slog.Warn(init.name+"数据表初始化失败", "err", err)
		}
	}

	// 启动 REST API（供 Windows 桌面版远程调用）；apiPort 填负数表示禁用
	if cfg.APIPort > 0 {
		apiSrv, apiErr := startAPIServer(db, llmClient, client, cfg, cfg.APIPort)
		if apiErr != nil {
			// 面板是 NAS/容器下唯一的配置入口，端口绑不上就等于无法配置（包括改端口本身）。
			// 这种情况直接退出比“看起来在跑但网页打不开”有用得多。
			slog.Error("API 服务启动失败，进程退出", "err", apiErr,
				"处置", "把 config.json 的 apiPort 换成空闲端口，或释放占用该端口的进程/旧容器")
			os.Exit(1)
		}
		// 用 Shutdown 而不是 Close：Close 会直接掐断在途请求，
		// 桌面端那边表现为一次莫名其妙的连接重置。
		// 这个 defer 注册在 db.Close() 之后，所以会先于关库执行。
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := apiSrv.Shutdown(ctx); err != nil {
				apiSrv.Close()
			}
		}()
	} else {
		slog.Info("API 服务已禁用（config.json 中 apiPort 为负数）")
	}

	slog.Info("开始消息轮询，按 Ctrl+C 退出")

	// 未绑定时：API 已启动、面板已可访问，再在后台试一次终端扫码（照顾有终端的部署）。
	// 无论成败都不影响进程存活：用户可一直在网页「状态 → 重新扫码绑定」里完成扫码。
	if !client.IsLoggedIn() {
		go func() {
			if err := doQRLogin(client, 45*time.Second); err != nil {
				slog.Warn("扫码登录未完成（不影响面板启动与其他功能）", "err", err,
					"下一步", "网页「状态」→「重新扫码绑定」")
				return
			}
			if err := client.SaveCredentials(); err != nil {
				slog.Error("保存登录凭据失败", "err", err)
				return
			}
			slog.Info("微信绑定完成，消息轮询将自动开始")
		}()
	}

	// 面板地址：未绑定时提醒用户去哪里扫码。探测失败就回一个通用提示，不阻断启动。
	panelURL, _, panelErr := WebPanelURL(cfg)
	if panelErr != nil || panelURL == "" {
		port := cfg.APIPort
		if port <= 0 {
			port = 17965
		}
		panelURL = fmt.Sprintf("http://<服务器IP>:%d/", port)
	}

	// 优雅关闭
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// 长轮询循环
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})

	go func() {
		defer close(doneCh)
		// 所有等待都必须能被 stopCh 打断：裸 time.Sleep 会让 Ctrl+C 之后
		// 主 goroutine 卡在 <-doneCh 上，最坏要等满 60 秒进程才真正退出
		// （nssm/systemd 早就判定超时并强杀了）。
		wait := func(d time.Duration) bool {
			select {
			case <-stopCh:
				return false
			case <-time.After(d):
				return true
			}
		}

		// 会话过期退避：首次 60s，之后 ×3，上限 60 分钟（对齐官方插件 Session Guard 的
		// 「暂停该账号 60 分钟」做法）。关键是**不再永不 GetUpdates**。
		expiryBackoff := time.Minute
		lastHousekeep := time.Now()
		lastBindHint := time.Time{}

		for {
			select {
			case <-stopCh:
				return
			default:
			}

			// 尚未绑定微信（首次部署 / 重绑未完成）：不退进程，而是**守着面板等扫码**。
			// 旧行为是扫码失败就 os.Exit(1)，在没终端的 NAS 上等于把用户锁在门外。
			// 每 5 分钟提醒一次防刷屏；期间用户随时可用 /api/wechat/rebind 发起绑定，
			// 绑定成功后 IsLoggedIn 变真，下一轮就正常进入长轮询。
			if !client.IsLoggedIn() {
				if client.RebindActive() {
					if !wait(2 * time.Second) {
						return
					}
					continue
				}
				if time.Since(lastBindHint) > 5*time.Minute {
					slog.Info("尚未绑定微信：在网页「状态 → 重新扫码绑定」完成扫码后会自动开始收消息", "panel", panelURL)
					lastBindHint = time.Now()
				}
				if !wait(10 * time.Second) {
					return
				}
				continue
			}

			// 网页重绑进行中：挂起轮询，不与之抢会话（成功后过期位会被清、凭据已换新）
			if client.RebindActive() {
				if !wait(2 * time.Second) {
					return
				}
				continue
			}

			if client.SessionExpired() {
				slog.Warn("会话已过期：请在网页管理端点「重新扫码绑定」，或等自动探测恢复",
					"next_probe", expiryBackoff.String())
				recordPollErr(errors.New("会话已过期，需重新扫码登录"))
				if !wait(expiryBackoff) {
					return
				}
				if expiryBackoff < time.Hour {
					if expiryBackoff *= 3; expiryBackoff > time.Hour {
						expiryBackoff = time.Hour
					}
				}
				// 重要修复：旧代码在这里直接 continue，导致永远不再收消息——于是「重登」
				// 这条自救指令本身也收不到，机器人只能静默死亡（审计 C1）。
				// 现在清掉粘性位，让下面真的发一次请求：若会话仍无效，服务端会再次返回 -14 并重新置位。
				client.AllowProbe()
			}

			msgs, err := client.GetUpdates()
			if err != nil {
				// Shutdown() 取消了 ctx，这是正常的退出路径，不是错误
				if errors.Is(err, context.Canceled) {
					return
				}
				// 超时是正常的（服务端 hold ~35s 后返回空），不打印。
				// 用 net.Error 接口判断，别靠错误字符串里有没有 "Timeout"
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					// 长轮询 hold 住到超时说明连接本身是通的，算作一次成功连接
					recordPollOK()
					continue
				}
				if strings.Contains(err.Error(), "errcode=-14") ||
					strings.Contains(err.Error(), "会话已过期") {
					slog.Warn("会话过期（-14）：需在网页管理端重新扫码绑定", "next_probe", expiryBackoff.String())
					recordPollErr(errors.New("会话已过期，需重新扫码登录"))
					if !wait(5 * time.Second) {
						return
					}
					continue
				}
				slog.Error("轮询错误", "err", err)
				recordPollErr(err)
				if !wait(3 * time.Second) {
					return
				}
				continue
			}
			// 无错误返回（无论是否有新消息）说明与服务器的长轮询通道正常
			recordPollOK()
			expiryBackoff = time.Minute // 连接恢复，重置退避

			// 整批都处理成功才提交游标（at-least-once）：中途 panic/停机就不提交，
			// 下次用旧游标重拉，服务端会重放这批，由 ingest_ledger 保证不重复入库（审计 C9）。
			batchOK := true
			for i := range msgs {
				msg := msgs[i]
				select {
				case <-stopCh:
					return // 不提交游标：这批会重放，不丢消息
				default:
				}

				// 跳过 bot 自己发的消息（防回环）。
				// 注意：不能跳过 GetUserID()，因为登录用户给 bot 发消息时
				// from_user_id 就是登录用户的 ilink_user_id，跳过会导致用户消息全部丢失。
				if msg.FromUserID == client.GetBotID() {
					continue
				}

				key := messageKey(&msg)
				if process, attempts := ingestShouldProcess(db, key); !process {
					// 重放但已 done（或已达毒丸上限）：只标记并提交，不重复处理
					client.MarkProcessed(key)
					if ingestPoisoned(db, key) {
						slog.Error("放弃重试该消息（已达重试上限，需人工看日志）", "key", key, "from", msg.FromUserID)
					}
					continue
				} else if attempts > 0 {
					slog.Info("重试之前失败的消息", "key", key, "attempts", attempts)
				}

				slog.Info("收到消息", "from", msg.FromUserID, "text", preview(extractText(&msg), 100))

				// 处理消息并回复。HandleMessage 里的重活（画像生成、意图分析、合并）
				// 都已经放到后台 goroutine，这里只会短暂占用轮询循环。
				// SafeHandleMessage 隔离 panic：单条坏消息不再带走整个进程（审计 C2）。
				reply, ok := bot.SafeHandleMessage(&msg)
				if ok {
					if err := ingestMarkDone(db, key, msg.FromUserID); err != nil {
						slog.Warn("写收取账本 done 失败", "key", key, "err", err)
					}
					client.MarkProcessed(key)
				} else {
					n, ferr := ingestMarkFailed(db, key, msg.FromUserID, "handler panic")
					slog.Error("消息处理失败，不提交游标，等待重放重试", "key", key, "attempts", n, "err", ferr)
					batchOK = false
				}

				if reply != "" {
					if err := client.SendTextWithToken(msg.FromUserID, reply, msg.ContextToken); err != nil {
						slog.Error("发送回复失败", "to", msg.FromUserID, "err", err)
					} else {
						slog.Info("回复已发送", "to", msg.FromUserID, "len", len([]rune(reply)))
					}
				}
			}

			if batchOK {
				if err := client.CommitCursor(); err != nil {
					slog.Warn("游标提交失败", "err", err)
				}
			}

			// 周期性维护（每 6 小时）：清理过期收取账本（防表无限增长）+ 轮转日志（防磁盘填满）
			if time.Since(lastHousekeep) > 6*time.Hour {
				if n, err := ingestPrune(db); err == nil && n > 0 {
					slog.Info("已清理过期收取账本行", "rows", n)
				}
				rotateAllLogs()
				lastHousekeep = time.Now()
			}
		}
	}()

	<-sigCh
	fmt.Println("\n收到退出信号，正在关闭...")
	slog.Info("收到退出信号，正在关闭")
	client.Shutdown() // 取消长轮询中的 HTTP 请求，加速退出
	close(stopCh)
	<-doneCh
	fmt.Println("已退出")
	slog.Info("已退出")
}

// doQRLogin 执行扫码登录流程。wait 是等待扫码/确认的时间上限（容器里没有终端，
// 传短值让主流程尽快走下去，改由网页面板完成绑定）。
func doQRLogin(client *ILinkClient, wait time.Duration) error {
	slog.Info("正在获取二维码...")
	resp, err := client.GetQRCode()
	if err != nil {
		return err
	}

	// 在终端打印 ASCII 二维码（方便 Linux/Windows 直接扫码）
	fmt.Println("\n请用手机微信扫描以下二维码：")
	printQRCode(resp.QRCodeURL)
	// 链接同时打到控制台和日志：Windows 服务模式没有控制台，只能从 bot.log 里复制
	fmt.Printf("\n如果终端里的二维码无法识别，请复制下面这个链接到浏览器打开，再用微信扫页面里的二维码：\n%s\n\n", resp.QRCodeURL)
	slog.Info("二维码链接", "url", resp.QRCodeURL)
	slog.Info("等待扫码确认...", "最长等待", wait.String())

	if err := client.PollQRCodeStatus(resp.QRCode, wait); err != nil {
		return err
	}

	slog.Info("登录成功！")
	return nil
}

// dbPath 返回数据库文件路径
func dbPath() string {
	// Docker 场景：优先用 /config 目录
	if _, err := os.Stat("/config"); err == nil {
		return "/config/wechat-profile-bot.db"
	}
	exe, err := os.Executable()
	if err != nil {
		return "wechat-profile-bot.db"
	}
	return filepath.Join(filepath.Dir(exe), "wechat-profile-bot.db")
}

// credentialPath 返回凭据保存路径
func credentialPath() string {
	// Docker 场景：优先用 /config 目录
	if _, err := os.Stat("/config"); err == nil {
		return "/config/ilink_credentials.json"
	}
	exe, err := os.Executable()
	if err != nil {
		return "ilink_credentials.json"
	}
	return filepath.Join(filepath.Dir(exe), "ilink_credentials.json")
}
