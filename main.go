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

	// LLM 客户端
	llmClient := NewLLMClient(cfg)

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

	// 如未登录，启动扫码流程
	if !client.IsLoggedIn() {
		if err := doQRLogin(client); err != nil {
			slog.Error("扫码登录失败", "err", err)
			os.Exit(1)
		}
		if err := client.SaveCredentials(); err != nil {
			slog.Error("保存凭据失败", "err", err)
		}
	}

	// 加载 context token 缓存
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
		apiSrv := startAPIServer(db, llmClient, client, cfg, cfg.APIPort)
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

		for {
			select {
			case <-stopCh:
				return
			default:
			}

			if client.SessionExpired() {
				slog.Warn("会话已过期，请发送「重登」命令后重启程序")
				recordPollErr(errors.New("会话已过期，需重新扫码登录"))
				if !wait(60 * time.Second) {
					return
				}
				continue
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
					slog.Warn("会话过期，请发送「重登」命令后重启程序")
					recordPollErr(errors.New("会话已过期，需重新扫码登录"))
					if !wait(60 * time.Second) {
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

			for _, msg := range msgs {
				select {
				case <-stopCh:
					return
				default:
				}

				// 跳过 bot 自己发的消息（防止回环）。
				// 注意：不能跳过 GetUserID()，因为登录用户给 bot 发消息时
				// from_user_id 就是登录用户的 ilink_user_id，跳过会导致用户消息全部丢失。
				if msg.FromUserID == client.GetBotID() {
					continue
				}

				slog.Info("收到消息", "from", msg.FromUserID, "text", preview(extractText(&msg), 100))

				// 处理消息并回复。HandleMessage 里的重活（画像生成、意图分析、合并）
				// 都已经放到后台 goroutine，这里只会短暂占用轮询循环。
				reply := bot.HandleMessage(&msg)
				if reply != "" {
					if err := client.SendTextWithToken(msg.FromUserID, reply, msg.ContextToken); err != nil {
						slog.Error("发送回复失败", "to", msg.FromUserID, "err", err)
					} else {
						slog.Info("回复已发送", "to", msg.FromUserID, "len", len([]rune(reply)))
					}
				}
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

// doQRLogin 执行扫码登录流程
func doQRLogin(client *ILinkClient) error {
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
	slog.Info("等待扫码确认...")

	if err := client.PollQRCodeStatus(resp.QRCode, 5*time.Minute); err != nil {
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
