package main

// 会话过期后的自助重绑（网页端扫码）。
//
// 解决的问题（投产前审计 C1）：iLink 的 -14（会话过期）是必然事件，而旧实现唯一出路是
// 「让用户在微信里发『重登』」——可会话一过期就再也收不到消息，这条路径自己把自己堵死了；
// 重启也没用（凭据还在 → IsLoggedIn 为真 → 跳过扫码 → 再次 -14）。
// 社区/官方插件（OpenClaw Session Guard）的通行做法是：检测到 -14 就暂停并必须重新扫码。
// 这里做成网页上可自助完成：暂存旧凭据 → 取二维码（渲染 PNG 给浏览器）→ 轮询扫码状态 →
// confirmed 后落新凭据并清过期位，主循环随即恢复收消息。
//
// 两条额外的自我保护（投产前审计 N2）：
//   - 失败可回滚：-14 也可能只是服务端瞬时抛错，所以旧凭据先暂存而不是直接删除；
//     扫码没成（失败/过期/放弃）就原样恢复，用户一次误点不会毁掉仍可能可用的会话。
//   - 冷却：连续取码会打 get_bot_qrcode，而该协议已实测存在 rate limit（ret=-2），
//     故两次重绑之间至少间隔 rebindCooldown，避免自己把自己限流锁死。

import (
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"
)

const (
	rebindPollTimeout = 4 * time.Minute // 二维码 8 分钟有效，取一半留出重新取码余量
	rebindMaxRounds   = 3               // 二维码过期后可重新获取的次数
	rebindCooldown    = 5 * time.Minute // 两次重绑之间的最小间隔（防刷接口触发限流）
)

// errRebindCooldown 让 HTTP 层能区分「冷却中」与其他失败（→ 429 而非 500）。
var errRebindCooldown = errors.New("rebind cooldown")

// RebindActive 是否有重绑流程正在进行（主循环据此挂起轮询，不与之抢会话）。
func (c *ILinkClient) RebindActive() bool {
	c.rebindMu.Lock()
	defer c.rebindMu.Unlock()
	return c.rebindActive
}

func (c *ILinkClient) setRebind(active bool, status, qrData, errMsg string) {
	c.rebindMu.Lock()
	c.rebindActive, c.rebindStatus, c.rebindQRData, c.rebindError, c.rebindAt = active, status, qrData, errMsg, time.Now()
	c.rebindMu.Unlock()
}

// RebindSnapshot 返回当前重绑状态，供网页轮询与运维诊断。二维码用 dataURL 直接可 <img> 显示。
//
// 锁顺序：先取 c.mu 读会话字段，再取 rebindMu。runRebind 从不嵌套持两锁，
// 所以这个固定顺序不会死锁；少了 c.mu 就是真数据竞争（-race 实测抓到过）。
func (c *ILinkClient) RebindSnapshot() map[string]interface{} {
	c.mu.RLock()
	expired, loggedIn := c.sessionExpired, c.botToken != "" && c.botID != ""
	c.mu.RUnlock()

	c.rebindMu.Lock()
	st := c.rebindStatus
	if st == "" {
		st = "idle"
	}
	cooldownLeft := time.Duration(0)
	if !c.rebindLastAttempt.IsZero() {
		if d := rebindCooldown - time.Since(c.rebindLastAttempt); d > 0 {
			cooldownLeft = d.Round(time.Second)
		}
	}
	snap := map[string]interface{}{
		"active":     c.rebindActive,
		"status":     st,
		"qr_image":   c.rebindQRData,
		"error":      c.rebindError,
		"updated_at": c.rebindAt.Format(time.RFC3339),
	}
	c.rebindMu.Unlock()

	// 会话状态一并回传，网页可据此提示「当前需要重新扫码」
	snap["session_expired"] = expired
	snap["logged_in"] = loggedIn
	snap["cooldown_seconds"] = int(cooldownLeft.Seconds())
	return snap
}

// StartRebind 幂等启动一次网页扫码重绑。返回 (是否本次新启动, 错误)。
// 已在进行时返回 false 且无错误；冷却未过返回 wrapped errRebindCooldown。
func (c *ILinkClient) StartRebind() (bool, error) {
	c.rebindMu.Lock()
	if c.rebindActive {
		c.rebindMu.Unlock()
		return false, nil
	}
	if !c.rebindLastAttempt.IsZero() {
		if wait := rebindCooldown - time.Since(c.rebindLastAttempt); wait > 0 {
			c.rebindMu.Unlock()
			return false, errRebindCooldown
		}
	}
	c.rebindLastAttempt = time.Now()
	c.rebindMu.Unlock()

	c.setRebind(true, "fetching", "", "")
	go c.runRebind()
	return true, nil
}

// runRebind 在后台执行完整扫码流程。任何失败都会写进状态，并把暂存的旧会话恢复回去。
func (c *ILinkClient) runRebind() {
	var succeeded bool
	stash, stashErr := c.stashCredentials()

	defer func() {
		// panic 也不能把重绑挂死：active 永久为 true 会让主循环一直等待
		if r := recover(); r != nil {
			slog.Error("重绑流程 panic", "panic", r)
			c.setRebind(false, "failed", "", "重绑流程内部错误，请稍后重试")
		}
		if succeeded {
			return
		}
		// 未成功：把旧凭据原样放回去（-14 可能只是瞬时错误，不该被一次误点放大成永久失联）
		if stash != nil && len(stash.files) > 0 {
			if err := stash.restore(c); err != nil {
				slog.Error("重绑失败后恢复旧会话文件失败", "err", err)
			} else {
				slog.Warn("重绑未成功，已恢复原会话文件（旧凭据未丢失，可稍后再试）")
			}
		}
	}()

	if stashErr != nil {
		c.setRebind(false, "failed", "", stashErr.Error())
		return
	}

	for round := 1; round <= rebindMaxRounds; round++ {
		if c.ctx.Err() != nil {
			c.setRebind(false, "failed", "", "进程正在退出")
			return
		}
		c.setRebind(true, "fetching", "", "")

		qr, err := c.GetQRCode()
		if err != nil {
			c.setRebind(false, "failed", "", "获取二维码失败: "+err.Error())
			return
		}
		png, err := qrcode.Encode(qr.QRCodeURL, qrcode.Medium, 256)
		if err != nil {
			c.setRebind(false, "failed", "", "二维码渲染失败: "+err.Error())
			return
		}
		dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
		c.setRebind(true, "waiting", dataURL, "")
		slog.Info("重绑二维码已就绪，等待手机微信扫码", "round", round)

		err = c.PollQRCodeStatus(qr.QRCode, rebindPollTimeout)
		if err == nil {
			if serr := c.SaveCredentials(); serr != nil {
				c.setRebind(false, "failed", "", "扫码成功但凭据保存失败: "+serr.Error())
				return
			}
			// 新会话已建立：旧凭据不再需要，丢弃暂存（defer 因此不会回滚）
			succeeded = true
			stash.discard()
			// 清掉会话过期位：主循环下一轮就能真正恢复 GetUpdates
			c.AllowProbe()
			c.setRebind(false, "confirmed", "", "")
			slog.Info("重绑成功：新凭据已保存，消息轮询将自动恢复")
			return
		}
		// 二维码过期 → 重新取一张（最多 rebindMaxRounds 轮）；其余错误直接结束并如实回传
		msg := err.Error()
		if strings.Contains(msg, "已过期") || strings.Contains(msg, "扫码超时") {
			slog.Warn("重绑二维码过期或超时，准备重新获取", "round", round)
			continue
		}
		if c.ctx.Err() != nil {
			c.setRebind(false, "failed", "", "进程正在退出")
			return
		}
		c.setRebind(false, "failed", "", msg)
		return
	}
	c.setRebind(false, "failed", "", "二维码多次过期，请稍后重试")
}
